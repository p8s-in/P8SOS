package api

import (
	"context"
	"strings"
	"testing"

	"p8sos/internal/vpn"
)

func TestRemoteAccess(t *testing.T) {
	m := newMultiUserCfg(t, func(s *Server) {
		s.Remote = &Remote{VPN: &vpn.Manager{Dir: t.TempDir()},
			PublicIP: func(context.Context) (string, error) { return "49.37.1.2", nil },
			FindRouter: func(context.Context) (vpn.Router, error) {
				return vpn.Router{Found: true, Name: "JioFiber", ExternalIP: "100.80.1.1", CGNAT: true}, nil
			}}
	})
	v := m.req("GET", "/api/v1/remote", secret, "", 200)
	if v["public_ip"] != "49.37.1.2" || v["router"].(map[string]any)["cgnat"] != true {
		t.Fatalf("%v", v)
	}
	m.req("POST", "/api/v1/remote/vpn/devices", secret, `{"name":"phone"}`, 400) // VPN off
	m.req("PUT", "/api/v1/remote/vpn", secret, `{"enabled":true,"endpoint":"49.37.1.2"}`, 200)
	v = m.req("POST", "/api/v1/remote/vpn/devices", secret, `{"name":"Ravi phone"}`, 201)
	conf, _ := v["config"].(string)
	if !strings.Contains(conf, "Endpoint = 49.37.1.2:51830") {
		t.Fatal(conf)
	}
	id := v["device"].(map[string]any)["id"].(string)
	if v := m.req("GET", "/api/v1/remote", secret, "", 200); len(v["vpn"].(map[string]any)["devices"].([]any)) != 1 {
		t.Fatal(v)
	}
	m.req("POST", "/api/v1/users", secret, `{"name":"ops","role":"operator","password":"operator-pass-1"}`, 201)
	ops := m.login("ops", "operator-pass-1", "")
	m.req("GET", "/api/v1/remote", ops, "", 200)
	m.req("POST", "/api/v1/remote/vpn/devices", ops, `{"name":"x"}`, 403)
	m.req("DELETE", "/api/v1/remote/vpn/devices/"+id, secret, "", 204)
	m.req("DELETE", "/api/v1/remote/vpn/devices/"+id, secret, "", 404)
}
