package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"p8sos/internal/ldap"
	"p8sos/internal/secpol"
	"p8sos/internal/storage"
	"p8sos/internal/users"
)

func TestSecurity(t *testing.T) {
	defer users.SetPolicy(users.Policy{MinLength: 10})
	sec := &secpol.Store{Dir: t.TempDir()}
	m := newMultiUserCfg(t, func(s *Server) { s.Security = sec })
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1"}`, 201)

	// allowed networks: 127.0.0.1 (this test) is always allowed; another address isn't
	m.req("PUT", "/api/v1/security", secret, `{"allow_nets":["192.0.2.0/24","bad"],"password":{"min_length":12}}`, 400)
	m.req("PUT", "/api/v1/security", secret, `{"allow_nets":["192.0.2.0/24","198.51.100.7"],"password":{"min_length":12,"mix":true}}`, 200)
	h := m.server.Handler()
	for addr, want := range map[string]int{"203.0.113.5:1000": 403, "192.0.2.44:1000": 401, "198.51.100.7:9": 401, "10.250.0.2:5": 401} {
		req := httptest.NewRequest("GET", "/api/v1/vms", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: %d want %d", addr, rec.Code, want)
		}
	}
	req := httptest.NewRequest("POST", "/api/v1/cluster/register", nil)
	req.RemoteAddr = "203.0.113.5:1000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 403 {
		t.Fatal("joining servers must reach /cluster/register")
	}
	// a list without the saver's own address is refused (here: as if from outside)
	sec2 := &secpol.Store{Dir: t.TempDir()}
	if _, err := sec2.Save(secpol.Settings{AllowNets: []string{"192.0.2.0/24"}, Password: users.Policy{MinLength: 10}}, "203.0.113.9"); err == nil {
		t.Fatal("lock-out accepted")
	}

	// password policy
	m.req("POST", "/api/v1/users", secret, `{"name":"al","password":"abcdefghijkl"}`, 400) // no digit
	m.req("POST", "/api/v1/users", secret, `{"name":"al","password":"short1"}`, 400)       // too short
	m.req("POST", "/api/v1/users", secret, `{"name":"al","password":"password1234"}`, 400) // common
	m.req("POST", "/api/v1/users", secret, `{"name":"al","password":"blue-river-42"}`, 201)

	// staff must use 2FA
	m.req("PUT", "/api/v1/security", secret, `{"staff_2fa":true,"password":{"min_length":10}}`, 200)
	boss := m.login("boss", "boss-password-1", "")
	m.req("GET", "/api/v1/vms", boss, "", 403)
	if v := m.req("GET", "/api/v1/me", boss, "", 200); v["need_2fa"] != true {
		t.Fatalf("me %v", v)
	}
	m.req("POST", "/api/v1/me/2fa/setup", boss, "", 200)
	al := m.login("al", "blue-river-42", "") // customers aren't forced
	m.req("GET", "/api/v1/vms", al, "", 200)
	m.req("GET", "/api/v1/vms", secret, "", 200) // the host's token neither
	_ = http.StatusOK
}

func TestDirectoryLogin(t *testing.T) {
	d := &ldap.FakeDirectory{BindDN: "cn=svc,dc=corp", BindPassword: "svc-pass", People: []ldap.FakePerson{
		{DN: "uid=meera,ou=people,dc=corp", Password: "dir-pass-1", UID: "meera", Mail: "meera@corp.in", Name: "Meera", UUID: "u-1",
			MemberOf: []string{"cn=p8s-ops,ou=groups,dc=corp"}},
		{DN: "uid=guest,ou=people,dc=corp", Password: "dir-pass-2", UID: "guest", UUID: "u-2"},
	}}
	url, err := d.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lm := &ldap.Manager{Dir: t.TempDir()}
	m := newMultiUserCfg(t, func(s *Server) { s.LDAP = lm })
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1"}`, 201)
	body := `{"enabled":true,"url":"` + url + `","skip_verify":true,"bind_dn":"cn=svc,dc=corp","bind_password":"svc-pass","base_dn":"dc=corp","operator_groups":["p8s-ops"]}`
	if v := m.req("POST", "/api/v1/ldap/test", secret, body, 200); v["message"] == nil {
		t.Fatal(v)
	}
	if v := m.req("PUT", "/api/v1/ldap", secret, body, 200); v["has_password"] != true {
		t.Fatal(v)
	}
	// a directory person signs in: account made, role from the group
	tok := m.login("meera", "dir-pass-1", "")
	if me := m.req("GET", "/api/v1/me", tok, "", 200); me["role"] != "operator" || me["email"] != "meera@corp.in" {
		t.Fatalf("me %v", me)
	}
	m.req("POST", "/api/v1/login", "", `{"user":"meera","password":"wrong"}`, 401)
	// in no group, no default role: refused
	m.req("POST", "/api/v1/login", "", `{"user":"guest","password":"dir-pass-2"}`, 403)
	// local accounts still sign in locally (the directory isn't asked)
	m.login("boss", "boss-password-1", "")
	// directory down: a clear answer, not "wrong password"
	d.Close()
	m.req("POST", "/api/v1/login", "", `{"user":"meera","password":"dir-pass-1"}`, 502)
}

func TestEncryptionKey(t *testing.T) {
	key := filepath.Join(t.TempDir(), "p8s.key")
	os.WriteFile(key, []byte{1, 2, 3, 0xff}, 0o400)
	m := newMultiUserCfg(t, func(s *Server) {
		s.Encryption = func(context.Context) (storage.Encryption, error) {
			return storage.Encryption{On: true, Algorithm: "aes-256-gcm", Root: "tank/p8s", KeyFile: key, KeyStatus: "available"}, nil
		}
	})
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"ops","role":"operator","password":"operator-pass-1"}`, 201)
	boss := m.login("boss", "boss-password-1", "")
	if v := m.req("GET", "/api/v1/node/encryption", boss, "", 200); v["on"] != true {
		t.Fatal(v)
	}
	m.req("POST", "/api/v1/node/encryption-key", boss, `{"password":"wrong-password"}`, 403)
	if v := m.req("POST", "/api/v1/node/encryption-key", boss, `{"password":"boss-password-1"}`, 200); v["key_hex"] != "010203ff" {
		t.Fatal(v)
	}
	ops := m.login("ops", "operator-pass-1", "")
	m.req("POST", "/api/v1/node/encryption-key", ops, `{"password":"operator-pass-1"}`, 403)
}
