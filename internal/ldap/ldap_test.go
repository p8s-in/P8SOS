package ldap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func set(children ...*packet) *packet { return &packet{tag: tagSet, children: children} }

// fakeDir is a tiny directory server: one service account, one person, one group.
type fakeDir struct {
	mu      sync.Mutex
	filters [][]byte
	binds   []string
}

func result(n byte, code int64) *packet { return app(n, true, enumVal(code), str(""), str("")) }

func (d *fakeDir) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		p, err := readPacket(r)
		if err != nil {
			return
		}
		id, op := p.children[0].int(), p.children[1]
		reply := func(o *packet) { c.Write(seq(intVal(id), o).bytes()) }
		switch op.tag {
		case classApplication | constructed | 0: // bind
			dn, pw := op.children[1].str(), op.children[2].str()
			d.mu.Lock()
			d.binds = append(d.binds, dn)
			d.mu.Unlock()
			ok := dn == "cn=svc,dc=corp" && pw == "svc-pass" || dn == "uid=ravi,ou=people,dc=corp" && pw == "right-pass"
			reply(result(1, map[bool]int64{true: 0, false: 49}[ok]))
		case classApplication | constructed | 3: // search
			f := op.children[6].bytes()
			d.mu.Lock()
			d.filters = append(d.filters, f)
			d.mu.Unlock()
			switch {
			case bytes.Contains(f, []byte("member")):
				reply(app(4, true, str("cn=auditors,ou=groups,dc=corp"), seq()))
			case bytes.Contains(f, []byte("ravi")):
				reply(app(4, true, str("uid=ravi,ou=people,dc=corp"), seq(
					seq(str("mail"), set(str("Ravi@P8S.in"))),
					seq(str("displayName"), set(str("Ravi K"))),
					seq(str("uid"), set(str("ravi"))),
					seq(str("entryUUID"), set(str("1111-2222"))),
					seq(str("memberOf"), set(str("cn=p8s-admins,ou=groups,dc=corp"), str("cn=staff,ou=groups,dc=corp"))))))

			}
			reply(result(5, 0))
		case classApplication | 2: // unbind
			return
		}
	}
}

func tlsPair(t *testing.T) (*tls.Config, func(net.Conn) net.Conn) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dc1.corp"}, DNSNames: []string{"dc1.corp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	return cfg, func(c net.Conn) net.Conn { return tls.Server(c, cfg) }
}

func TestDirectory(t *testing.T) {
	d := &fakeDir{}
	_, wrap := tlsPair(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(wrap(c))
		}
	}()
	m := &Manager{Dir: t.TempDir(), Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return net.Dial("tcp", ln.Addr().String())
	}}
	if _, err := m.SetSettings(Settings{Enabled: true, URL: "ldap://dc1.corp", BaseDN: "dc=corp"}); err == nil {
		t.Fatal("plain ldap:// accepted")
	}
	if _, err := m.SetSettings(Settings{Enabled: true, URL: "ldaps://dc1.corp", BaseDN: "dc=corp", UserFilter: "(uid={user}"}); err == nil {
		t.Fatal("bad filter accepted")
	}
	s := Settings{Enabled: true, URL: "ldaps://dc1.corp", SkipVerify: true, BindDN: "cn=svc,dc=corp", BindPassword: "svc-pass",
		BaseDN: "dc=corp", AdminGroups: []string{"p8s-admins"}, AuditGroups: []string{"cn=auditors,ou=groups,dc=corp"}}
	if _, err := m.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := m.Authenticate(ctx, "ravi", "right-pass")
	if err != nil {
		t.Fatal(err)
	}
	if id.Key != "ldap|1111-2222" || id.Email != "ravi@p8s.in" || id.Name != "Ravi K" || id.Username != "ravi" || id.Role != "admin" {
		t.Fatalf("%+v", id)
	}
	if _, err := m.Authenticate(ctx, "ravi", "wrong"); err != ErrBadLogin {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := m.Authenticate(ctx, "ravi", ""); err != ErrBadLogin {
		t.Fatal("empty password must never bind")
	}
	if _, err := m.Authenticate(ctx, "nobody", "x"); err != ErrBadLogin {
		t.Fatalf("unknown: %v", err)
	}
	// typed text can't change the filter
	m.Authenticate(ctx, "*)(uid=*", "x")
	last := d.filters[len(d.filters)-1]
	if !bytes.Contains(last, []byte("*)(uid=*")) || bytes.Contains(last, []byte{0x87}) {
		t.Fatalf("filter %q", last)
	}
	// groups by search: the auditor group comes from the group filter
	s.GroupFilter = "(&(objectClass=groupOfNames)(member={dn}))"
	s.AdminGroups = nil
	m.SetSettings(s)
	if id, err := m.Authenticate(ctx, "ravi", "right-pass"); err != nil || id.Role != "auditor" {
		t.Fatalf("%+v %v", id, err)
	}
	// wrong service password
	s.BindPassword = "nope"
	m.SetSettings(s)
	if _, err := m.Authenticate(ctx, "ravi", "right-pass"); err == nil || !strings.Contains(err.Error(), "service account") {
		t.Fatalf("svc: %v", err)
	}
	if msg, err := m.Test(ctx, Settings{URL: "ldaps://dc1.corp", SkipVerify: true, BindDN: "cn=svc,dc=corp", BindPassword: "svc-pass", BaseDN: "dc=corp"}); err != nil || !strings.Contains(msg, "connected") {
		t.Fatalf("test %q %v", msg, err)
	}
}

func TestFilter(t *testing.T) {
	for _, f := range []string{"(cn=a)", "(&(a=b)(|(c=d)(e=*)))", "(!(a=b))", "(cn=ab*cd*ef)", "(a>=5)", "(cn=x\\2ay)"} {
		if _, err := compile(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, f := range []string{"cn=a", "(cn=a", "(&)", "(=a)", "(cn=\\zz)"} {
		if _, err := compile(f); err == nil {
			t.Errorf("%s accepted", f)
		}
	}
	if Escape("a*(b)\\") != "a\\2a\\28b\\29\\5c" {
		t.Fatal(Escape("a*(b)\\"))
	}
	p, _ := compile("(n=x)")
	q, err := parse(p.tag, p.bytes()[2:])
	if err != nil || len(q.children) != 2 || q.children[1].str() != "x" {
		t.Fatalf("roundtrip %v %+v", err, q)
	}
	if intVal(-2).int() != -2 || intVal(300).int() != 300 || intVal(128).int() != 128 {
		t.Fatal("ints")
	}
}
