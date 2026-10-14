package api

import (
	"strings"
	"testing"
	"time"

	"p8sos/internal/users"
)

func TestScopedTokens(t *testing.T) {
	m := newMultiUserCfg(t, nil)
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1"}`, 201)
	boss := m.login("boss", "boss-password-1", "")
	ro := m.req("POST", "/api/v1/me/tokens", boss, `{"name":"grafana","read_only":true}`, 201)["secret"].(string)
	one := m.req("POST", "/api/v1/me/tokens", boss, `{"name":"deploy","vms":["web1"]}`, 201)["secret"].(string)
	short := m.req("POST", "/api/v1/me/tokens", boss, `{"name":"contractor","days":1}`, 201)
	m.req("POST", "/api/v1/me/tokens", boss, `{"name":"bad","vms":["Not A VM"]}`, 400)

	// read-only: looks, can't change
	m.req("GET", "/api/v1/vms", ro, "", 200)
	m.req("POST", "/api/v1/vms/web1/stop", ro, "", 403)
	// one VM: sees and runs only that VM
	if v := m.req("GET", "/api/v1/vms", one, "", 200); len(v["vms"].([]any)) != 1 {
		t.Fatalf("list %v", v)
	}
	m.req("GET", "/api/v1/vms/web1", one, "", 200)
	m.req("GET", "/api/v1/vms/shop", one, "", 403)
	m.req("POST", "/api/v1/vms", one, `{"name":"x"}`, 403)
	m.req("GET", "/api/v1/users", one, "", 403)
	// expiry
	sec := short["secret"].(string)
	if !strings.HasPrefix(short["token"].(map[string]any)["expires_at"].(string), "20") {
		t.Fatalf("expiry %v", short)
	}
	m.req("GET", "/api/v1/vms", sec, "", 200)
	m.now = m.now.Add(25 * time.Hour)
	m.req("GET", "/api/v1/vms", sec, "", 401)
	// the list shows the scopes
	l := m.req("GET", "/api/v1/me/tokens", boss, "", 200)["tokens"].([]any)
	if len(l) != 3 || l[0].(map[string]any)["read_only"] != true {
		t.Fatalf("tokens %v", l)
	}
}

func TestVMShares(t *testing.T) {
	m := newMultiUserCfg(t, func(s *Server) { s.Shares = &users.Shares{} })
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"bob-password-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"carol","password":"carol-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	bob := m.login("bob", "bob-password-1", "")
	carol := m.login("carol", "carol-password", "")
	m.req("GET", "/api/v1/vms/shop", bob, "", 404) // not shared yet: doesn't exist for him
	m.req("PUT", "/api/v1/vms/shop/shares", bob, `{"user":"bob","level":"user"}`, 404)
	m.req("PUT", "/api/v1/vms/shop/shares", alice, `{"user":"bob","level":"user"}`, 200)    // the owner shares
	m.req("PUT", "/api/v1/vms/shop/shares", alice, `{"user":"carol","level":"viewer"}`, 200) // a viewer
	m.req("PUT", "/api/v1/vms/shop/shares", alice, `{"user":"nobody","level":"user"}`, 404)
	if v := m.req("GET", "/api/v1/vms", bob, "", 200); len(v["vms"].([]any)) != 1 || v["vms"].([]any)[0].(map[string]any)["access"] != "user" {
		t.Fatalf("bob's list %v", v)
	}
	m.req("POST", "/api/v1/vms/shop/stop", bob, "", 202)   // users run it
	m.req("DELETE", "/api/v1/vms/shop", bob, "", 403)      // but don't delete it
	m.req("PATCH", "/api/v1/vms/shop", bob, `{}`, 403)     // or resize it
	m.req("PUT", "/api/v1/vms/shop/shares", bob, `{"user":"carol","level":"user"}`, 403) // or share it further
	m.req("GET", "/api/v1/vms/shop", carol, "", 200)       // viewers look
	m.req("POST", "/api/v1/vms/shop/start", carol, "", 403) // only
	if l := m.req("GET", "/api/v1/vms/shop/shares", alice, "", 200)["shares"].([]any); len(l) != 2 {
		t.Fatalf("shares %v", l)
	}
	m.req("PUT", "/api/v1/vms/shop/shares", alice, `{"user":"bob","level":""}`, 200) // taken away
	m.req("GET", "/api/v1/vms/shop", bob, "", 404)
}
