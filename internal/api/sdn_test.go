package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/sdn"
	"p8sos/internal/vm"
)

type sdnRun struct{ cmds []string }

func (f *sdnRun) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, c)
	if strings.HasPrefix(c, "ip -j link show") || strings.HasPrefix(c, "bridge -j") {
		return []byte("[]"), nil
	}
	return nil, nil
}

func TestSDNAPI(t *testing.T) {
	dir := t.TempDir()
	run := &sdnRun{}
	mgr := &sdn.Manager{Store: &sdn.Store{Path: filepath.Join(dir, "sdn.json")}, Run: run, DataDir: dir}
	m := newMultiUserCfg(t, func(s *Server) { s.VNets = mgr })
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","role":"customer","password":"user-pass-123"}`, 201)
	bob := m.login("bob", "user-pass-123", "")
	m.req("PUT", "/api/v1/sdn", bob, `{"vnets":[]}`, 403)
	m.req("PUT", "/api/v1/sdn", secret, `{"vnets":[{"name":"web","subnet":"8.8.8.0/24"}]}`, 400)
	v := m.req("PUT", "/api/v1/sdn", secret, `{"vnets":[{"name":"web","subnet":"10.60.0.0/24"},{"name":"db"}]}`, 200)
	if l := v["vnets"].([]any); len(l) != 2 || l[1].(map[string]any)["vni"].(float64) != 10000 {
		t.Fatalf("%v", v)
	}
	if !strings.Contains(strings.Join(run.cmds, "\n"), "ip link add vxb10000 type bridge") {
		t.Fatalf("not applied: %v", run.cmds)
	}
	if g := m.req("GET", "/api/v1/sdn", bob, "", 200); len(g["vnets"].([]any)) != 2 || g["gateway_here"] != true {
		t.Fatalf("%v", g)
	}
	// a VM uses web: it can't be removed or renumbered
	w := m.vms.vms["web1"]
	w.Network = "vnet:web"
	m.vms.vms["web1"] = w
	m.req("PUT", "/api/v1/sdn", secret, `{"vnets":[{"name":"db","vni":10001}]}`, 409)
	m.req("PUT", "/api/v1/sdn", secret, `{"vnets":[{"name":"web","vni":5,"subnet":"10.60.0.0/24"},{"name":"db","vni":10001}]}`, 409)
	// changing its subnet is fine; db (unused) can go
	m.req("PUT", "/api/v1/sdn", secret, `{"vnets":[{"name":"web","vni":10000,"subnet":"10.61.0.0/24"}]}`, 200)
	if c := mgr.Store.Load(); len(c.VNets) != 1 || c.VNets[0].Subnet != "10.61.0.0/24" {
		t.Fatalf("%+v", c)
	}
	_ = vm.IsVNet
}
