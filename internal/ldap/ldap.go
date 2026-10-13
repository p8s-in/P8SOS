// Package ldap signs people in against a company directory: Microsoft Active
// Directory, OpenLDAP, FreeIPA, Samba AD, JumpCloud... (LDAP v3, simple bind).
//
// Sign-in: bind with the service account, find the person with the user
// filter, bind as them with the password they typed (that is the password
// check), read their groups, map the groups to a role. Plain ldap:// only
// with StartTLS; passwords never go over the network unencrypted.
package ldap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid")
	ErrOff      = errors.New("directory sign-in is not set up")
	ErrBadLogin = errors.New("wrong user name or password")
	ErrNoRole   = errors.New("no role")
)

// Settings are kept in data_dir/ldap.json.
type Settings struct {
	Enabled      bool     `json:"enabled"`
	URL          string   `json:"url"`       // ldaps://dc1.corp.local or ldap://ldap.example.com (then StartTLS)
	StartTLS     bool     `json:"start_tls"` // required for ldap://
	CACert       string   `json:"ca_cert,omitempty"`
	SkipVerify   bool     `json:"skip_verify,omitempty"` // testing only
	BindDN       string   `json:"bind_dn"`
	BindPassword string   `json:"bind_password,omitempty"`
	BaseDN       string   `json:"base_dn"`
	UserFilter   string   `json:"user_filter,omitempty"`  // {user} = what was typed
	GroupFilter  string   `json:"group_filter,omitempty"` // {dn} = the person's DN; empty = memberOf only
	AdminGroups  []string `json:"admin_groups,omitempty"`
	OperGroups   []string `json:"operator_groups,omitempty"`
	AuditGroups  []string `json:"auditor_groups,omitempty"`
	CustGroups   []string `json:"customer_groups,omitempty"`
	DefaultRole  string   `json:"default_role,omitempty"` // "" = refuse people in no group, or "customer"
}

// DefaultUserFilter finds people by Unix name, Windows name or user@domain.
const DefaultUserFilter = "(&(|(objectClass=person)(objectClass=user))(|(uid={user})(sAMAccountName={user})(userPrincipalName={user})(mail={user})))"

// Identity is who signed in.
type Identity struct {
	Key      string   `json:"key"` // "ldap|" + entryUUID / objectGUID (else the DN)
	DN       string   `json:"dn"`
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
	Role     string   `json:"role"`
}

// Manager keeps the settings.
type Manager struct {
	Dir     string
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error) // nil = net.Dialer
	Timeout time.Duration                                                     // 0 = 10 s

	mu  sync.Mutex
	set *Settings
}

func (m *Manager) file() string { return filepath.Join(m.Dir, "ldap.json") }

// Settings returns the settings (with the password).
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.set == nil {
		s := &Settings{}
		if b, err := os.ReadFile(m.file()); err == nil {
			_ = json.Unmarshal(b, s)
		}
		m.set = s
	}
	return *m.set
}

// On reports whether directory sign-in is enabled.
func (m *Manager) On() bool {
	s := m.Settings()
	return s.Enabled && s.URL != "" && s.BaseDN != ""
}

// SetSettings validates and stores; an empty bind password keeps the old one.
func (m *Manager) SetSettings(s Settings) (Settings, error) {
	s.URL = strings.TrimSpace(s.URL)
	if s.Enabled {
		u, err := url.Parse(s.URL)
		if err != nil || u.Host == "" || (u.Scheme != "ldap" && u.Scheme != "ldaps") {
			return Settings{}, fmt.Errorf("%w: address must look like ldaps://dc1.example.com", ErrInvalid)
		}
		if u.Scheme == "ldap" && !s.StartTLS {
			return Settings{}, fmt.Errorf("%w: ldap:// sends passwords in the clear; use ldaps:// or turn on StartTLS", ErrInvalid)
		}
		if strings.TrimSpace(s.BaseDN) == "" {
			return Settings{}, fmt.Errorf("%w: base DN is missing (e.g. dc=corp,dc=example,dc=com)", ErrInvalid)
		}
		if s.CACert != "" {
			if !x509.NewCertPool().AppendCertsFromPEM([]byte(s.CACert)) {
				return Settings{}, fmt.Errorf("%w: the CA certificate isn't PEM", ErrInvalid)
			}
		}
		for _, f := range []string{s.UserFilter, s.GroupFilter} {
			if f != "" {
				if _, err := compile(strings.NewReplacer("{user}", "x", "{dn}", "x").Replace(f)); err != nil {
					return Settings{}, fmt.Errorf("%w: filter %s: %v", ErrInvalid, f, err)
				}
			}
		}
	}
	if s.DefaultRole != "" && s.DefaultRole != "customer" {
		return Settings{}, fmt.Errorf("%w: people in none of the groups can only become customers (or be refused)", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.BindPassword == "" && m.set != nil {
		s.BindPassword = m.set.BindPassword
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
	m.set = &s
	return s, nil
}

// --- connection ---

type conn struct {
	c  net.Conn
	r  *bufio.Reader
	id int64
}

func (m *Manager) connect(ctx context.Context, s Settings) (*conn, error) {
	u, err := url.Parse(s.URL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), map[string]string{"ldap": "389", "ldaps": "636"}[u.Scheme])
	}
	to := m.Timeout
	if to == 0 {
		to = 10 * time.Second
	}
	dial := m.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: to}
		dial = d.DialContext
	}
	nc, err := dial(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("can't reach the directory %s: %w", host, err)
	}
	_ = nc.SetDeadline(time.Now().Add(to))
	tc := &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: s.SkipVerify, MinVersion: tls.VersionTLS12}
	if s.CACert != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(s.CACert))
		tc.RootCAs = pool
	}
	c := &conn{c: nc, r: bufio.NewReader(nc)}
	if u.Scheme == "ldaps" {
		t := tls.Client(nc, tc)
		if err := t.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, fmt.Errorf("TLS with the directory: %w", err)
		}
		c.c, c.r = t, bufio.NewReader(t)
	} else if s.StartTLS {
		op := app(23, true, ctxStr(0, "1.3.6.1.4.1.1466.20037"))
		if _, err := c.send(op); err != nil {
			nc.Close()
			return nil, err
		}
		resp, err := c.recv()
		if err != nil || resp.tag != classApplication|constructed|24 || resultCode(resp) != 0 {
			nc.Close()
			return nil, errors.New("the directory refused StartTLS")
		}
		t := tls.Client(nc, tc)
		if err := t.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, fmt.Errorf("TLS with the directory: %w", err)
		}
		c.c, c.r = t, bufio.NewReader(t)
	}
	return c, nil
}

func (c *conn) close() {
	_, _ = c.send(app(2, false))
	_ = c.c.Close()
}

func (c *conn) send(op *packet) (int64, error) {
	c.id++
	_, err := c.c.Write(seq(intVal(c.id), op).bytes())
	return c.id, err
}

// recv reads the next message for the last request and returns its operation.
func (c *conn) recv() (*packet, error) {
	for {
		p, err := readPacket(c.r)
		if err != nil {
			return nil, err
		}
		if len(p.children) < 2 {
			return nil, errors.New("ldap: bad message")
		}
		if p.children[0].int() == c.id {
			return p.children[1], nil
		}
	}
}

func resultCode(op *packet) int64 {
	if len(op.children) == 0 {
		return -1
	}
	return op.children[0].int()
}

func resultMsg(op *packet) string {
	if len(op.children) >= 3 {
		return op.children[2].str()
	}
	return ""
}

func (c *conn) bind(dn, pw string) error {
	if _, err := c.send(app(0, true, intVal(3), str(dn), ctxStr(0, pw))); err != nil {
		return err
	}
	resp, err := c.recv()
	if err != nil {
		return err
	}
	switch code := resultCode(resp); code {
	case 0:
		return nil
	case 49:
		return ErrBadLogin
	default:
		return fmt.Errorf("directory said %d %s", code, resultMsg(resp))
	}
}

type entry struct {
	dn    string
	attrs map[string][][]byte
}

func (e entry) first(name string) string {
	if v := e.attrs[strings.ToLower(name)]; len(v) > 0 {
		return string(v[0])
	}
	return ""
}

func (c *conn) search(base, filter string, attrs []string, limit int64) ([]entry, error) {
	f, err := compile(filter)
	if err != nil {
		return nil, err
	}
	var al []*packet
	for _, a := range attrs {
		al = append(al, str(a))
	}
	op := app(3, true, str(base), enumVal(2), enumVal(0), intVal(limit), intVal(10), boolean(false), f, seq(al...))
	if _, err := c.send(op); err != nil {
		return nil, err
	}
	var out []entry
	for {
		resp, err := c.recv()
		if err != nil {
			return nil, err
		}
		switch resp.tag {
		case classApplication | constructed | 4: // entry
			if len(resp.children) < 2 {
				continue
			}
			e := entry{dn: resp.children[0].str(), attrs: map[string][][]byte{}}
			for _, a := range resp.children[1].children {
				if len(a.children) < 2 {
					continue
				}
				k := strings.ToLower(a.children[0].str())
				for _, v := range a.children[1].children {
					e.attrs[k] = append(e.attrs[k], v.value)
				}
			}
			out = append(out, e)
		case classApplication | constructed | 5: // done
			code := resultCode(resp)
			if code != 0 && code != 4 { // 4 = size limit (we asked for few)
				return nil, fmt.Errorf("directory search failed: %d %s", code, resultMsg(resp))
			}
			return out, nil
		}
	}
}

// --- filters (RFC 4515) ---

// Escape makes typed text safe inside a filter value.
func Escape(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch r {
		case '*', '(', ')', '\\', 0:
			fmt.Fprintf(&b, "\\%02x", r)
		default:
			b.WriteByte(r)
		}
	}
	return b.String()
}

func compile(f string) (*packet, error) {
	f = strings.TrimSpace(f)
	p, rest, err := parseFilter(f)
	if err != nil {
		return nil, err
	}
	if rest != "" {
		return nil, errors.New("text after the filter")
	}
	return p, nil
}

func parseFilter(f string) (*packet, string, error) {
	if !strings.HasPrefix(f, "(") {
		return nil, "", errors.New("filters start with (")
	}
	f = f[1:]
	if f == "" {
		return nil, "", errors.New("unfinished filter")
	}
	switch f[0] {
	case '&', '|':
		tag := byte(0xa0)
		if f[0] == '|' {
			tag = 0xa1
		}
		p := &packet{tag: tag}
		f = f[1:]
		for strings.HasPrefix(f, "(") {
			c, rest, err := parseFilter(f)
			if err != nil {
				return nil, "", err
			}
			p.children = append(p.children, c)
			f = rest
		}
		if !strings.HasPrefix(f, ")") || len(p.children) == 0 {
			return nil, "", errors.New("bad & or | filter")
		}
		return p, f[1:], nil
	case '!':
		c, rest, err := parseFilter(f[1:])
		if err != nil {
			return nil, "", err
		}
		if !strings.HasPrefix(rest, ")") {
			return nil, "", errors.New("bad ! filter")
		}
		return &packet{tag: 0xa2, children: []*packet{c}}, rest[1:], nil
	}
	end := strings.IndexByte(f, ')')
	if end < 0 {
		return nil, "", errors.New("missing )")
	}
	item, rest := f[:end], f[end+1:]
	var attr, op, val string
	for _, o := range []string{">=", "<=", "~=", "="} {
		if i := strings.Index(item, o); i > 0 {
			attr, op, val = item[:i], o, item[i+len(o):]
			break
		}
	}
	if attr == "" {
		return nil, "", fmt.Errorf("bad item %q", item)
	}
	if op == "=" && val == "*" {
		return &packet{tag: 0x87, value: []byte(attr)}, rest, nil
	}
	if op == "=" && strings.Contains(val, "*") {
		parts := strings.Split(val, "*")
		var subs []*packet
		for i, part := range parts {
			if part == "" {
				continue
			}
			u, err := unescape(part)
			if err != nil {
				return nil, "", err
			}
			t := byte(0x81)
			if i == 0 {
				t = 0x80
			} else if i == len(parts)-1 {
				t = 0x82
			}
			subs = append(subs, &packet{tag: t, value: u})
		}
		return &packet{tag: 0xa4, children: []*packet{str(attr), seq(subs...)}}, rest, nil
	}
	u, err := unescape(val)
	if err != nil {
		return nil, "", err
	}
	tag := map[string]byte{"=": 0xa3, ">=": 0xa5, "<=": 0xa6, "~=": 0xa8}[op]
	return &packet{tag: tag, children: []*packet{str(attr), {tag: tagOctetString, value: u}}}, rest, nil
}

func unescape(s string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			if i+2 >= len(s) {
				return nil, errors.New("bad escape")
			}
			b, err := hex.DecodeString(s[i+1 : i+3])
			if err != nil {
				return nil, errors.New("bad escape")
			}
			out = append(out, b[0])
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out, nil
}

// --- sign-in ---

// Authenticate checks a user name and password against the directory.
func (m *Manager) Authenticate(ctx context.Context, user, password string) (Identity, error) {
	s := m.Settings()
	if !m.On() {
		return Identity{}, ErrOff
	}
	user = strings.TrimSpace(user)
	if user == "" || password == "" { // an empty password would be an "anonymous bind" that succeeds
		return Identity{}, ErrBadLogin
	}
	c, err := m.connect(ctx, s)
	if err != nil {
		return Identity{}, err
	}
	defer c.close()
	if err := c.bind(s.BindDN, s.BindPassword); err != nil {
		if errors.Is(err, ErrBadLogin) {
			return Identity{}, errors.New("the directory refused the service account (check bind DN and password)")
		}
		return Identity{}, err
	}
	uf := s.UserFilter
	if uf == "" {
		uf = DefaultUserFilter
	}
	attrs := []string{"mail", "displayName", "cn", "uid", "sAMAccountName", "memberOf", "entryUUID", "objectGUID", "userPrincipalName"}
	found, err := c.search(s.BaseDN, strings.ReplaceAll(uf, "{user}", Escape(user)), attrs, 2)
	if err != nil {
		return Identity{}, err
	}
	if len(found) != 1 {
		return Identity{}, ErrBadLogin // unknown (or ambiguous) person: same answer as a wrong password
	}
	e := found[0]
	if err := c.bind(e.dn, password); err != nil {
		return Identity{}, err
	}
	id := Identity{DN: e.dn, Email: strings.ToLower(e.first("mail")), Name: firstOf(e.first("displayName"), e.first("cn"))}
	id.Username = firstOf(e.first("uid"), e.first("sAMAccountName"), strings.Split(e.first("userPrincipalName"), "@")[0], user)
	switch {
	case e.first("entryUUID") != "":
		id.Key = "ldap|" + e.first("entryUUID")
	case len(e.attrs["objectguid"]) > 0:
		id.Key = "ldap|" + hex.EncodeToString(e.attrs["objectguid"][0])
	default:
		id.Key = "ldap|" + strings.ToLower(e.dn)
	}
	for _, g := range e.attrs["memberof"] {
		id.Groups = append(id.Groups, string(g))
	}
	if s.GroupFilter != "" {
		if err := c.bind(s.BindDN, s.BindPassword); err == nil {
			gs, err := c.search(s.BaseDN, strings.ReplaceAll(s.GroupFilter, "{dn}", Escape(e.dn)), []string{"cn"}, 500)
			if err != nil {
				return Identity{}, err
			}
			for _, g := range gs {
				id.Groups = append(id.Groups, g.dn)
			}
		}
	}
	id.Role = roleOf(s, id.Groups)
	return id, nil
}

// Test checks the settings: service bind and one search.
func (m *Manager) Test(ctx context.Context, s Settings) (string, error) {
	if s.BindPassword == "" {
		s.BindPassword = m.Settings().BindPassword
	}
	c, err := m.connect(ctx, s)
	if err != nil {
		return "", err
	}
	defer c.close()
	if err := c.bind(s.BindDN, s.BindPassword); err != nil {
		if errors.Is(err, ErrBadLogin) {
			return "", errors.New("the directory refused the service account (check bind DN and password)")
		}
		return "", err
	}
	l, err := c.search(s.BaseDN, "(|(objectClass=person)(objectClass=user))", []string{"cn"}, 50)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("connected; the service account sees %d+ people under %s", len(l), s.BaseDN), nil
}

func cnOf(dn string) string {
	first, _, _ := strings.Cut(dn, ",")
	if k, v, ok := strings.Cut(first, "="); ok && strings.EqualFold(strings.TrimSpace(k), "cn") {
		return strings.TrimSpace(v)
	}
	return ""
}

func roleOf(s Settings, groups []string) string {
	in := func(want []string) bool {
		for _, w := range want {
			for _, g := range groups {
				if strings.EqualFold(w, g) || strings.EqualFold(w, cnOf(g)) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case in(s.AdminGroups):
		return "admin"
	case in(s.OperGroups):
		return "operator"
	case in(s.AuditGroups):
		return "auditor"
	case in(s.CustGroups):
		return "customer"
	}
	return s.DefaultRole
}

func firstOf(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}
