// Package sso signs people in with a company login (OpenID Connect: Google
// Workspace, Microsoft Entra ID, Keycloak, Authentik, Okta, Zitadel, ...).
//
// Flow: the dashboard sends the browser to /api/v1/sso/start, which redirects
// to the provider (authorization code + PKCE + nonce). The provider sends the
// browser back to /api/v1/sso/callback with a code; p8sd exchanges it for an
// ID token, checks the token's signature (the provider's published keys),
// issuer, audience, expiry and nonce, and maps the person's groups to a role.
package sso

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid = errors.New("invalid")
	ErrOff     = errors.New("single sign-on is not set up")
	ErrDenied  = errors.New("sign-in refused")
)

// Settings are kept in data_dir/sso.json.
type Settings struct {
	Enabled      bool     `json:"enabled"`
	Name         string   `json:"name"`   // button text: "Sign in with <Name>"
	Issuer       string   `json:"issuer"` // e.g. https://accounts.google.com
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	RedirectBase string   `json:"redirect_base"` // the dashboard's address, e.g. https://p8s.example.com:8006
	Scopes       []string `json:"scopes,omitempty"`
	Domains      []string `json:"allowed_domains,omitempty"` // email domains allowed; empty = any
	GroupsClaim  string   `json:"groups_claim,omitempty"`    // default "groups"
	AdminGroups  []string `json:"admin_groups,omitempty"`
	OperGroups   []string `json:"operator_groups,omitempty"`
	AuditGroups  []string `json:"auditor_groups,omitempty"`
	CustGroups   []string `json:"customer_groups,omitempty"`
	// DefaultRole for people in none of the groups: "" = refuse (unless they
	// already have an account), or "customer".
	DefaultRole string `json:"default_role,omitempty"`
}

// Identity is who signed in, with the role their groups give ("" = none).
type Identity struct {
	Key      string   `json:"key"` // issuer + "|" + subject (stable)
	Email    string   `json:"email"`
	Verified bool     `json:"email_verified"`
	Name     string   `json:"name"`
	Username string   `json:"username"` // suggested account name
	Groups   []string `json:"groups"`
	Role     string   `json:"role"`
}

type pending struct {
	nonce, verifier string
	at              time.Time
}

type discovery struct {
	Issuer      string   `json:"issuer"`
	AuthURL     string   `json:"authorization_endpoint"`
	TokenURL    string   `json:"token_endpoint"`
	JWKSURL     string   `json:"jwks_uri"`
	AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
}

// Manager keeps the settings and the sign-ins in progress.
type Manager struct {
	Dir  string
	HTTP *http.Client
	Now  func() time.Time

	mu      sync.Mutex
	set     *Settings
	pend    map[string]pending
	disc    *discovery
	discAt  time.Time
	keys    map[string]crypto.PublicKey
	keysAt  time.Time
	keysFor string
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (m *Manager) file() string { return filepath.Join(m.Dir, "sso.json") }

func (m *Manager) loadLocked() {
	if m.set != nil {
		return
	}
	s := &Settings{}
	if b, err := os.ReadFile(m.file()); err == nil {
		_ = json.Unmarshal(b, s)
	}
	m.set = s
}

// Settings returns the settings (with the secret).
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	return *m.set
}

// On reports whether SSO is enabled, and the button name.
func (m *Manager) On() (bool, string) {
	s := m.Settings()
	name := s.Name
	if name == "" {
		name = "company login"
	}
	return s.Enabled && s.Issuer != "" && s.ClientID != "", name
}

var reGroup = regexp.MustCompile(`^[^\x00-\x1f]{1,200}$`)

// SetSettings validates and stores; an empty secret keeps the old one.
func (m *Manager) SetSettings(s Settings) (Settings, error) {
	s.Issuer = strings.TrimRight(strings.TrimSpace(s.Issuer), "/")
	s.RedirectBase = strings.TrimRight(strings.TrimSpace(s.RedirectBase), "/")
	if s.Enabled {
		u, err := url.Parse(s.Issuer)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLocal(u.Hostname()))) {
			return Settings{}, fmt.Errorf("%w: issuer must be an https:// address", ErrInvalid)
		}
		if s.ClientID == "" {
			return Settings{}, fmt.Errorf("%w: client ID is missing", ErrInvalid)
		}
		if r, err := url.Parse(s.RedirectBase); err != nil || r.Host == "" || (r.Scheme != "https" && r.Scheme != "http") {
			return Settings{}, fmt.Errorf("%w: dashboard address must look like https://p8s.example.com:8006", ErrInvalid)
		}
	}
	switch s.DefaultRole {
	case "", "customer":
	default:
		return Settings{}, fmt.Errorf("%w: people in none of the groups can only become customers (or be refused)", ErrInvalid)
	}
	for _, l := range [][]string{s.AdminGroups, s.OperGroups, s.AuditGroups, s.CustGroups, s.Domains} {
		for _, g := range l {
			if !reGroup.MatchString(g) {
				return Settings{}, fmt.Errorf("%w: bad group or domain %q", ErrInvalid, g)
			}
		}
	}
	if len(s.Name) > 40 {
		return Settings{}, fmt.Errorf("%w: name too long", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	if s.ClientSecret == "" {
		s.ClientSecret = m.set.ClientSecret
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return Settings{}, err
	}
	if err := os.WriteFile(m.file()+".tmp", b, 0o600); err != nil {
		return Settings{}, err
	}
	if err := os.Rename(m.file()+".tmp", m.file()); err != nil {
		return Settings{}, err
	}
	m.set, m.disc, m.keys = &s, nil, nil
	return s, nil
}

func isLocal(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

// CallbackURL is where the provider sends people back (register it there).
func (s Settings) CallbackURL() string { return s.RedirectBase + "/api/v1/sso/callback" }

func randStr(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (m *Manager) discover(ctx context.Context, s Settings) (*discovery, error) {
	m.mu.Lock()
	if m.disc != nil && m.now().Sub(m.discAt) < time.Hour {
		d := m.disc
		m.mu.Unlock()
		return d, nil
	}
	m.mu.Unlock()
	var d discovery
	if err := m.getJSON(ctx, s.Issuer+"/.well-known/openid-configuration", &d); err != nil {
		return nil, fmt.Errorf("provider settings: %w", err)
	}
	if strings.TrimRight(d.Issuer, "/") != s.Issuer || d.AuthURL == "" || d.TokenURL == "" || d.JWKSURL == "" {
		return nil, fmt.Errorf("provider settings don't match the issuer %s", s.Issuer)
	}
	m.mu.Lock()
	m.disc, m.discAt = &d, m.now()
	m.mu.Unlock()
	return &d, nil
}

func (m *Manager) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Start begins a sign-in and returns the provider's address to send the browser to.
func (m *Manager) Start(ctx context.Context) (string, error) {
	s := m.Settings()
	if on, _ := m.On(); !on {
		return "", ErrOff
	}
	d, err := m.discover(ctx, s)
	if err != nil {
		return "", err
	}
	state, nonce, verifier := randStr(24), randStr(24), randStr(32)
	m.mu.Lock()
	if m.pend == nil {
		m.pend = map[string]pending{}
	}
	for k, p := range m.pend { // forget abandoned sign-ins
		if m.now().Sub(p.at) > 10*time.Minute {
			delete(m.pend, k)
		}
	}
	if len(m.pend) > 1000 {
		m.mu.Unlock()
		return "", fmt.Errorf("too many sign-ins in progress; try again in a few minutes")
	}
	m.pend[state] = pending{nonce, verifier, m.now()}
	m.mu.Unlock()
	sum := sha256.Sum256([]byte(verifier))
	scopes := s.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	q := url.Values{"response_type": {"code"}, "client_id": {s.ClientID}, "redirect_uri": {s.CallbackURL()},
		"scope": {strings.Join(scopes, " ")}, "state": {state}, "nonce": {nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	sep := "?"
	if strings.Contains(d.AuthURL, "?") {
		sep = "&"
	}
	return d.AuthURL + sep + q.Encode(), nil
}

// Finish completes a sign-in from the provider's answer.
func (m *Manager) Finish(ctx context.Context, state, code string) (Identity, error) {
	s := m.Settings()
	if on, _ := m.On(); !on {
		return Identity{}, ErrOff
	}
	m.mu.Lock()
	p, ok := m.pend[state]
	delete(m.pend, state)
	m.mu.Unlock()
	if !ok || m.now().Sub(p.at) > 10*time.Minute {
		return Identity{}, fmt.Errorf("%w: the sign-in expired; try again", ErrDenied)
	}
	d, err := m.discover(ctx, s)
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.CallbackURL()},
		"client_id": {s.ClientID}, "code_verifier": {p.verifier}}
	basic := len(d.AuthMethods) == 0
	for _, a := range d.AuthMethods {
		basic = basic || a == "client_secret_basic"
	}
	if !basic && s.ClientSecret != "" {
		form.Set("client_secret", s.ClientSecret)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", d.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic && s.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(s.ClientID), url.QueryEscape(s.ClientSecret))
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok)
	if resp.StatusCode != 200 || tok.IDToken == "" {
		return Identity{}, fmt.Errorf("%w: the provider said %s %s (HTTP %d)", ErrDenied, tok.Error, tok.Desc, resp.StatusCode)
	}
	claims, err := m.verify(ctx, s, d, tok.IDToken)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrDenied, err)
	}
	if n, _ := claims["nonce"].(string); n != p.nonce {
		return Identity{}, fmt.Errorf("%w: wrong nonce", ErrDenied)
	}
	return identityOf(s, claims)
}

func identityOf(s Settings, c map[string]any) (Identity, error) {
	sub, _ := c["sub"].(string)
	if sub == "" {
		return Identity{}, fmt.Errorf("%w: no subject in the token", ErrDenied)
	}
	id := Identity{Key: s.Issuer + "|" + sub}
	id.Email, _ = c["email"].(string)
	id.Email = strings.ToLower(id.Email)
	switch v := c["email_verified"].(type) {
	case bool:
		id.Verified = v
	case string:
		id.Verified = v == "true"
	}
	if strings.HasPrefix(s.Issuer, "https://login.microsoftonline.com/") && id.Email == "" { // Entra ID
		id.Email, _ = c["preferred_username"].(string)
		id.Email = strings.ToLower(id.Email)
	}
	id.Name, _ = c["name"].(string)
	id.Username, _ = c["preferred_username"].(string)
	if id.Username == "" || strings.Contains(id.Username, "@") {
		id.Username, _, _ = strings.Cut(firstNonEmpty(id.Username, id.Email), "@")
	}
	gc := s.GroupsClaim
	if gc == "" {
		gc = "groups"
	}
	switch g := c[gc].(type) {
	case []any:
		for _, x := range g {
			if str, ok := x.(string); ok {
				id.Groups = append(id.Groups, str)
			}
		}
	case string:
		id.Groups = strings.Fields(strings.ReplaceAll(g, ",", " "))
	}
	if len(s.Domains) > 0 {
		_, dom, _ := strings.Cut(id.Email, "@")
		ok := false
		for _, d := range s.Domains {
			ok = ok || strings.EqualFold(strings.TrimPrefix(d, "@"), dom)
		}
		if !ok || !id.Verified {
			return Identity{}, fmt.Errorf("%w: %s isn't an allowed (verified) email domain", ErrDenied, firstNonEmpty(id.Email, "this account"))
		}
	}
	in := func(l []string) bool {
		for _, a := range l {
			for _, g := range id.Groups {
				if a == g {
					return true
				}
			}
		}
		return false
	}
	switch {
	case in(s.AdminGroups):
		id.Role = "admin"
	case in(s.OperGroups):
		id.Role = "operator"
	case in(s.AuditGroups):
		id.Role = "auditor"
	case in(s.CustGroups):
		id.Role = "customer"
	default:
		id.Role = s.DefaultRole
	}
	return id, nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// --- ID token (JWT) checks ---

func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func (m *Manager) verify(ctx context.Context, s Settings, d *discovery, jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, errors.New("the ID token isn't a JWT")
	}
	hb, err1 := b64(parts[0])
	pb, err2 := b64(parts[1])
	sig, err3 := b64(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, errors.New("the ID token is garbled")
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &h) != nil {
		return nil, errors.New("bad token header")
	}
	key, err := m.key(ctx, d, h.Kid)
	if err != nil {
		return nil, err
	}
	signed := []byte(parts[0] + "." + parts[1])
	if err := checkSig(h.Alg, key, signed, sig); err != nil {
		return nil, err
	}
	var c map[string]any
	if json.Unmarshal(pb, &c) != nil {
		return nil, errors.New("bad token claims")
	}
	if iss, _ := c["iss"].(string); strings.TrimRight(iss, "/") != s.Issuer {
		return nil, fmt.Errorf("token from %q, not %s", iss, s.Issuer)
	}
	audOK := false
	switch a := c["aud"].(type) {
	case string:
		audOK = a == s.ClientID
	case []any:
		for _, x := range a {
			audOK = audOK || x == s.ClientID
		}
	}
	if !audOK {
		return nil, errors.New("the token is for another application")
	}
	now := float64(m.now().Unix())
	if exp, ok := c["exp"].(float64); !ok || now > exp+60 {
		return nil, errors.New("the token has expired (is this server's clock right?)")
	}
	if iat, ok := c["iat"].(float64); ok && iat > now+300 {
		return nil, errors.New("the token is from the future (is this server's clock right?)")
	}
	return c, nil
}

func checkSig(alg string, key crypto.PublicKey, signed, sig []byte) error {
	var hf crypto.Hash
	var sum []byte
	switch alg {
	case "RS256", "ES256", "PS256":
		s := sha256.Sum256(signed)
		hf, sum = crypto.SHA256, s[:]
	case "RS384", "ES384":
		s := sha512.Sum384(signed)
		hf, sum = crypto.SHA384, s[:]
	case "RS512":
		s := sha512.Sum512(signed)
		hf, sum = crypto.SHA512, s[:]
	default:
		return fmt.Errorf("token signed with %q, which isn't accepted", alg)
	}
	switch k := key.(type) {
	case *rsa.PublicKey:
		if alg[0] == 'P' {
			if rsa.VerifyPSS(k, hf, sum, sig, nil) != nil {
				return errors.New("bad token signature")
			}
			return nil
		}
		if alg[0] != 'R' || rsa.VerifyPKCS1v15(k, hf, sum, sig) != nil {
			return errors.New("bad token signature")
		}
	case *ecdsa.PublicKey:
		n := (k.Curve.Params().BitSize + 7) / 8
		if alg[0] != 'E' || len(sig) != 2*n {
			return errors.New("bad token signature")
		}
		r, s := new(big.Int).SetBytes(sig[:n]), new(big.Int).SetBytes(sig[n:])
		if !ecdsa.Verify(k, sum, r, s) {
			return errors.New("bad token signature")
		}
	default:
		return errors.New("unknown key type")
	}
	return nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// key finds the provider's signing key by id (keys are re-read when a new id shows up).
func (m *Manager) key(ctx context.Context, d *discovery, kid string) (crypto.PublicKey, error) {
	m.mu.Lock()
	k, ok := m.keys[kid]
	same := m.keysFor == d.JWKSURL
	fresh := same && m.now().Sub(m.keysAt) < 5*time.Minute
	m.mu.Unlock()
	if ok && same {
		return k, nil
	}
	if fresh {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := m.getJSON(ctx, d.JWKSURL, &set); err != nil {
		return nil, fmt.Errorf("provider keys: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, j := range set.Keys {
		if j.Use != "" && j.Use != "sig" {
			continue
		}
		switch j.Kty {
		case "RSA":
			n, err1 := b64(j.N)
			e, err2 := b64(j.E)
			if err1 != nil || err2 != nil || len(n) < 256 {
				continue
			}
			keys[j.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case "EC":
			var c elliptic.Curve
			switch j.Crv {
			case "P-256":
				c = elliptic.P256()
			case "P-384":
				c = elliptic.P384()
			default:
				continue
			}
			x, err1 := b64(j.X)
			y, err2 := b64(j.Y)
			if err1 != nil || err2 != nil {
				continue
			}
			keys[j.Kid] = &ecdsa.PublicKey{Curve: c, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		}
	}
	m.mu.Lock()
	m.keys, m.keysAt, m.keysFor = keys, m.now(), d.JWKSURL
	m.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(keys) == 1 {
		for _, k := range keys {
			return k, nil
		}
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}
