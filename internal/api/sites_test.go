package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"p8sos/internal/tunnel"
)

// okCF answers every Cloudflare call successfully (the tunnel package tests the details).
func okCF(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var res any = map[string]any{}
		switch {
		case r.URL.Path == "/zones":
			res = []map[string]any{{"id": "z", "name": "example.com", "account": map[string]string{"id": "a"}}}
		case strings.HasSuffix(r.URL.Path, "/cfd_tunnel") && r.Method == "GET", strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == "GET":
			res = []any{}
		case strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
			res = map[string]string{"id": "c1744f8b-faa1-48a4-9e5c-02ac921467fa"}
		case strings.HasSuffix(r.URL.Path, "/token"):
			res = "tunnel-token"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": res})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSites(t *testing.T) {
	cf := okCF(t)
	m := newMultiUserWith(t, &tunnel.Manager{Dir: t.TempDir(), Dashboard: "https://127.0.0.1:8006",
		Client: func(tok string) *tunnel.Cloudflare { return &tunnel.Cloudflare{Token: tok, Base: cf.URL} },
		IPs: func(context.Context) (map[string]string, error) {
			return map[string]string{"shop": "192.168.122.9"}, nil
		}})

	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password","quota":{"vms":2,"vcpus":2,"memory_mib":2048,"disk_gb":20,"sites":1}}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"bob-password-1"}`, 201)
	alice := m.login("alice", "alice-password", "")
	bob := m.login("bob", "bob-password-1", "")

	if v := m.req("GET", "/api/v1/tunnel", alice, "", 200); v["set_up"] != false {
		t.Fatalf("tunnel status %v", v)
	}
	m.req("POST", "/api/v1/vms/shop/sites", alice, `{"name":"shop","port":80}`, 409) // not set up yet
	m.req("POST", "/api/v1/tunnel/setup", alice, `{"api_token":"x","domain":"example.com"}`, 403)
	v := m.req("POST", "/api/v1/tunnel/setup", secret, `{"api_token":"x","domain":"example.com"}`, 200)
	if v["set_up"] != true || v["domain"] != "example.com" || v["tunnel_token"] != nil {
		t.Fatalf("setup %v", v)
	}
	if v := m.req("GET", "/api/v1/tunnel", alice, "", 200); v["domain"] != "example.com" || v["tunnel_id"] != nil {
		t.Fatalf("customer view of tunnel %v", v)
	}
	m.req("PUT", "/api/v1/tunnel/dashboard", secret, `{"name":"panel"}`, 200)

	site := m.req("POST", "/api/v1/vms/shop/sites", alice, `{"name":"shop","port":80}`, 201)
	if site["host"] != "shop.example.com" || site["owner"] != "alice" || site["ip"] != "192.168.122.9" {
		t.Fatalf("site %v", site)
	}
	m.req("POST", "/api/v1/vms/shop/sites", alice, `{"name":"shop2","port":81}`, 403) // plan: 1 site
	m.req("POST", "/api/v1/vms/shop/sites", bob, `{"name":"x","port":80}`, 404)       // not bob's VM
	m.req("POST", "/api/v1/vms/web1/sites", alice, `{"name":"x","port":80}`, 404)
	if v := m.req("GET", "/api/v1/sites", bob, "", 200); len(v["sites"].([]any)) != 0 {
		t.Fatal("bob sees alice's site")
	}
	if v := m.req("GET", "/api/v1/sites", alice, "", 200); len(v["sites"].([]any)) != 1 {
		t.Fatal("alice's site list")
	}
	if me := m.req("GET", "/api/v1/me", alice, "", 200); me["usage"].(map[string]any)["sites"] != 1.0 {
		t.Fatalf("site usage %v", me["usage"])
	}
	m.req("DELETE", "/api/v1/sites/shop.example.com", bob, "", 404)
	m.req("DELETE", "/api/v1/sites/shop.example.com", alice, "", 204)
	m.req("POST", "/api/v1/vms/shop/sites", alice, `{"name":"www","port":80}`, 400) // reserved
	m.req("POST", "/api/v1/vms/shop/sites", alice, `{"name":"shop","port":80}`, 201)
	m.req("DELETE", "/api/v1/vms/shop", alice, "", 204)
	if v := m.req("GET", "/api/v1/sites", secret, "", 200); len(v["sites"].([]any)) != 0 {
		t.Fatalf("site of deleted VM left: %v", v)
	}
	m.req("DELETE", "/api/v1/tunnel", alice, "", 403)
	m.req("DELETE", "/api/v1/tunnel", secret, "", 204)
	if v := m.req("GET", "/api/v1/tunnel", secret, "", 200); v["set_up"] != false {
		t.Fatalf("after disconnect %v", v)
	}
}
