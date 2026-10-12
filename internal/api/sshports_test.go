package api

import "testing"

func TestSSHPortsAPI(t *testing.T) {
	m := newMultiUserCfg(t, func(s *Server) {
		s.SSHPorts = func() map[string]int { return map[string]int{"web1": 2201, "shop": 2202} }
	})
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	if v := m.req("GET", "/api/v1/ssh-ports", secret, "", 200)["ports"].(map[string]any); len(v) != 2 {
		t.Fatalf("admin %v", v)
	}
	v := m.req("GET", "/api/v1/ssh-ports", alice, "", 200)["ports"].(map[string]any)
	if len(v) != 1 || v["shop"].(float64) != 2202 {
		t.Fatalf("alice sees %v", v)
	}
}
