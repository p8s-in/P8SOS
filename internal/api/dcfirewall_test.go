package api

import (
	"path/filepath"
	"testing"

	"p8sos/internal/firewall"
)

func TestDCFirewallAPI(t *testing.T) {
	st := &firewall.GlobalStore{Path: filepath.Join(t.TempDir(), "fw.json")}
	m := newMultiUserCfg(t, func(s *Server) { s.DCFirewall = st })
	m.req("POST", "/api/v1/users", secret, `{"name":"ops","role":"operator","password":"operator-pass-1"}`, 201)
	ops := m.login("ops", "operator-pass-1", "")
	m.req("GET", "/api/v1/firewall", ops, "", 200)
	m.req("PUT", "/api/v1/firewall", ops, `{}`, 403)
	m.req("PUT", "/api/v1/firewall", secret, `{"block":["+nope"]}`, 400)
	v := m.req("PUT", "/api/v1/firewall", secret, `{"sets":{"office":["203.0.113.7"]},"block":["192.0.2.66"],"allow":[{"proto":"tcp","ports":"9100","source":"+office"}]}`, 200)
	if v["firewall"].(map[string]any)["block"].([]any)[0] != "192.0.2.66/32" {
		t.Fatalf("%v", v)
	}
	if g := st.Load(); len(g.Sets["office"]) != 1 || len(g.Allow) != 1 {
		t.Fatalf("%+v", g)
	}
}
