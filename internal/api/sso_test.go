package api

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/sso"
)

// fakeIdP is a tiny OpenID Connect provider.
type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]map[string]any // code → claims (+ the PKCE challenge)
	claims map[string]any            // who signs in next
	aud    string
}

func newIdP(t *testing.T) *fakeIdP {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &fakeIdP{key: k, codes: map[string]map[string]any{}, aud: "p8s-client"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/auth",
			"token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		e := big.NewInt(int64(k.E)).Bytes()
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e)}}})
	})
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) { // a browser signs in as p.claims
		q := r.URL.Query()
		c := map[string]any{"iss": p.srv.URL, "aud": p.aud, "exp": float64(time.Now().Add(5 * time.Minute).Unix()),
			"nonce": q.Get("nonce"), "_challenge": q.Get("code_challenge")}
		for k, v := range p.claims {
			c[k] = v
		}
		p.mu.Lock()
		p.codes["b-"+q.Get("state")] = c
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{"code": {"b-" + q.Get("state")}, "state": {q.Get("state")}}.Encode(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		id, sec, _ := r.BasicAuth()
		p.mu.Lock()
		c := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if id != "p8s-client" || sec != "s3cret" || c == nil || c["_challenge"] != base64.RawURLEncoding.EncodeToString(sum[:]) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		delete(c, "_challenge")
		json.NewEncoder(w).Encode(map[string]string{"id_token": p.sign(c), "token_type": "Bearer"})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) sign(c map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	b, _ := json.Marshal(c)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// signIn plays the browser: start → provider → callback; returns the dashboard fragment.
func (p *fakeIdP) signIn(t *testing.T, m *mu, claims map[string]any) string {
	t.Helper()
	nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := nc.Get(m.srv.URL + "/api/v1/sso/start")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), p.srv.URL+"/auth?") || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "https://p8s.example.com/api/v1/sso/callback" {
		t.Fatalf("start → %s", loc)
	}
	c := map[string]any{"iss": p.srv.URL, "aud": p.aud, "exp": float64(time.Now().Add(5 * time.Minute).Unix()),
		"iat": float64(time.Now().Unix()), "nonce": q.Get("nonce"), "_challenge": q.Get("code_challenge")}
	for k, v := range claims {
		c[k] = v
	}
	p.mu.Lock()
	p.codes["code-"+q.Get("state")] = c
	p.mu.Unlock()
	resp, err = nc.Get(m.srv.URL + "/api/v1/sso/callback?" + url.Values{"state": {q.Get("state")}, "code": {"code-" + q.Get("state")}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	l := resp.Header.Get("Location")
	frag, _ := url.QueryUnescape(strings.TrimPrefix(l, "/#"))
	return frag
}

func TestSSO(t *testing.T) {
	idp := newIdP(t)
	m := newMultiUserCfg(t, func(s *Server) { s.SSO = &sso.Manager{Dir: t.TempDir()} })
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1"}`, 201)
	if v := m.req("GET", "/api/v1/auth", "", "", 200); v["sso"] != nil {
		t.Fatal("sso shown while off")
	}
	m.req("PUT", "/api/v1/sso", secret, `{"enabled":true,"issuer":"ftp://x","client_id":"a","redirect_base":"https://p8s.example.com"}`, 400)
	body := `{"enabled":true,"name":"P8S Google","issuer":"` + idp.srv.URL + `","client_id":"p8s-client","client_secret":"s3cret",
	  "redirect_base":"https://p8s.example.com/","admin_groups":["p8s-admins"],"auditor_groups":["audit"],"allowed_domains":["p8s.in"]}`
	v := m.req("PUT", "/api/v1/sso", secret, body, 200)
	if v["client_secret"] != nil || v["has_secret"] != true || v["callback_url"] != "https://p8s.example.com/api/v1/sso/callback" {
		t.Fatalf("settings %v", v)
	}
	if v := m.req("GET", "/api/v1/auth", "", "", 200); v["sso"] != "P8S Google" {
		t.Fatalf("auth %v", v)
	}

	// an admin by group: account made, signed in
	f := idp.signIn(t, m, map[string]any{"sub": "u1", "email": "ravi@p8s.in", "email_verified": true, "preferred_username": "ravi@p8s.in",
		"name": "Ravi K", "groups": []any{"p8s-admins"}})
	tok, ok := strings.CutPrefix(f, "sso=")
	if !ok {
		t.Fatalf("fragment %q", f)
	}
	if me := m.req("GET", "/api/v1/me", tok, "", 200); me["name"] != "ravi" || me["role"] != "admin" || me["email"] != "ravi@p8s.in" {
		t.Fatalf("me %v", me)
	}
	// next time the groups say auditor: the role follows
	f = idp.signIn(t, m, map[string]any{"sub": "u1", "email": "ravi@p8s.in", "email_verified": true, "groups": []any{"audit"}})
	tok = strings.TrimPrefix(f, "sso=")
	if me := m.req("GET", "/api/v1/me", tok, "", 200); me["name"] != "ravi" || me["role"] != "auditor" {
		t.Fatalf("me %v", me)
	}
	// no group, no default role: refused; other domain: refused
	if f := idp.signIn(t, m, map[string]any{"sub": "u2", "email": "x@p8s.in", "email_verified": true}); !strings.Contains(f, "no account") {
		t.Fatalf("no group: %q", f)
	}
	if f := idp.signIn(t, m, map[string]any{"sub": "u3", "email": "x@gmail.com", "email_verified": true, "groups": []any{"p8s-admins"}}); !strings.Contains(f, "allowed") {
		t.Fatalf("domain: %q", f)
	}
	// a token for another app or with a bad signature is refused
	idp.aud = "other-app"
	if f := idp.signIn(t, m, map[string]any{"sub": "u1", "email": "ravi@p8s.in", "email_verified": true}); !strings.Contains(f, "sso_error") {
		t.Fatalf("aud: %q", f)
	}
	idp.aud = "p8s-client"
	// a replayed callback (state used) is refused
	nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := nc.Get(m.srv.URL + "/api/v1/sso/callback?state=nope&code=x")
	if !strings.Contains(resp.Header.Get("Location"), "expired") {
		t.Fatalf("replay %s", resp.Header.Get("Location"))
	}
	// operators can't change SSO
	m.req("POST", "/api/v1/users", secret, `{"name":"ops","role":"operator","password":"operator-pass-1"}`, 201)
	ops := m.login("ops", "operator-pass-1", "")
	m.req("PUT", "/api/v1/sso", ops, body, 403)
}
