package api

import (
	"context"
	"testing"

	"p8sos/internal/vm"
)

type nicVMs struct {
	*ownedVMs
	added, removed []string
}

func (n *nicVMs) AddNIC(_ context.Context, name, network string, vlan int) (vm.VM, string, error) {
	n.added = append(n.added, name+" "+network)
	return n.vms[name], "added", nil
}
func (n *nicVMs) RemoveNIC(_ context.Context, name, mac string) (vm.VM, error) {
	n.removed = append(n.removed, name+" "+mac)
	return n.vms[name], nil
}

func TestNICAPI(t *testing.T) {
	m := newMultiUserCfg(t, nil)
	nv := &nicVMs{ownedVMs: m.vms}
	m.server.VMs = nv
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	m.req("POST", "/api/v1/vms/shop/nics", alice, `{"network":"nat"}`, 403) // customers can't, even on their own VM
	if v := m.req("POST", "/api/v1/vms/web1/nics", secret, `{"network":"lan","vlan":20}`, 201); v["note"] != "added" {
		t.Fatalf("add %v", v)
	}
	m.req("DELETE", "/api/v1/vms/web1/nics/not-a-mac", secret, "", 404)
	m.req("DELETE", "/api/v1/vms/web1/nics/52:54:00:12:34:56", secret, "", 200)
	m.req("POST", "/api/v1/vms/nope/nics", secret, `{}`, 404)
	if len(nv.added) != 1 || nv.added[0] != "web1 lan" || len(nv.removed) != 1 {
		t.Fatalf("%v %v", nv.added, nv.removed)
	}
}
