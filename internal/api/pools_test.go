package api

import (
	"context"
	"fmt"
	"testing"

	"p8sos/internal/ippool"
	"p8sos/internal/vm"
)

func TestPools(t *testing.T) {
	m := newMultiUser(t)
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password","quota":{"vms":3,"vcpus":3,"memory_mib":4096,"disk_gb":40,"public_ips":1}}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"bob-password-1","quota":{"vms":3,"vcpus":3,"memory_mib":4096,"disk_gb":40,"public_ips":0}}`, 201)
	alice := m.login("alice", "alice-password", "")
	bob := m.login("bob", "bob-password-1", "")

	m.req("POST", "/api/v1/ippools", alice, `{"name":"dc1","bridge":"br0","cidr":"203.0.113.8/29","gateway":"203.0.113.9"}`, 403)
	m.req("POST", "/api/v1/ippools", secret, `{"name":"dc1","bridge":"br0","cidr":"203.0.113.8/29","gateway":"nope"}`, 400)
	v := m.req("POST", "/api/v1/ippools", secret, `{"name":"dc1","bridge":"br0","cidr":"203.0.113.8/29","gateway":"203.0.113.9"}`, 201)
	if v["size"] != 5.0 || v["free"] != 5.0 {
		t.Fatalf("pool %v", v)
	}
	m.req("POST", "/api/v1/ippools", secret, `{"name":"dc1","bridge":"br0","cidr":"198.51.100.0/30","gateway":"198.51.100.1"}`, 409)

	// not open to customers yet
	if l := m.req("GET", "/api/v1/ippools", alice, "", 200); len(l["pools"].([]any)) != 0 {
		t.Fatalf("customer sees closed pool: %v", l)
	}
	m.req("POST", "/api/v1/vms", alice, `{"name":"pub1","template":"debian-13","ip_pool":"dc1"}`, 404)
	m.req("PATCH", "/api/v1/ippools/dc1", secret, `{"dns":["9.9.9.9"],"customers":true}`, 200)
	l := m.req("GET", "/api/v1/ippools", alice, "", 200)["pools"].([]any)
	if len(l) != 1 || l[0].(map[string]any)["free"] != 5.0 || l[0].(map[string]any)["cidr"] != nil {
		t.Fatalf("customer pool view %v", l)
	}
	if full := m.req("GET", "/api/v1/ippools", secret, "", 200)["pools"].([]any); full[0].(map[string]any)["gateway"] != "203.0.113.9" {
		t.Fatalf("admin pool view %v", full)
	}
	m.req("POST", "/api/v1/vms", bob, `{"name":"pub2","template":"debian-13","ip_pool":"dc1"}`, 403) // plan: 0 public IPs
	m.req("POST", "/api/v1/vms", alice, `{"name":"pub1","template":"debian-13","ip_pool":"dc1"}`, 201)
	m.req("DELETE", "/api/v1/ippools/dc1", alice, "", 403)
	m.req("DELETE", "/api/v1/ippools/dc1", secret, "", 204)
}

type fakePubIPs struct{ got []vm.PublicIPRequest }

func (f *fakePubIPs) ChangePublicIPs(_ context.Context, name string, r vm.PublicIPRequest) (vm.PublicIPResult, error) {
	if name != "web" {
		return vm.PublicIPResult{}, fmt.Errorf("%w: vm %q", vm.ErrNotFound, name)
	}
	f.got = append(f.got, r)
	return vm.PublicIPResult{Lease: ippool.Lease{IP: "203.0.113.10"}, Applied: true, Note: "set inside the VM"}, nil
}

func TestPoolIPv6AndChanges(t *testing.T) {
	m := newMultiUser(t)
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password","quota":{"vms":3,"vcpus":3,"memory_mib":4096,"disk_gb":40,"public_ips":1}}`, 201)
	alice := m.login("alice", "alice-password", "")
	v := m.req("POST", "/api/v1/ippools", secret, `{"name":"dc1","bridge":"br0","cidr":"203.0.113.8/29","gateway":"203.0.113.9","cidr6":"2001:db8:5::/64","gateway6":"fe80::1"}`, 201)
	if v["cidr6"] != "2001:db8:5::/64" {
		t.Fatalf("%v", v)
	}
	m.req("PATCH", "/api/v1/ippools/dc1", secret, `{"dns":["1.1.1.1"],"cidr6":"nonsense","gateway6":"x"}`, 400)
	if v = m.req("PATCH", "/api/v1/ippools/dc1", secret, `{"dns":["1.1.1.1"],"cidr6":""}`, 200); v["cidr6"] != nil {
		t.Fatalf("v6 not removed %v", v)
	}
	f := &fakePubIPs{}
	m.server.LocalPublicIPs = f
	m.req("POST", "/api/v1/vms/web/public-ips", alice, `{"action":"add"}`, 403)
	r := m.req("POST", "/api/v1/vms/web/public-ips", secret, `{"action":"change","ip":"203.0.113.12","pool":"dc1"}`, 200)
	if r["applied"] != true || len(f.got) != 1 || f.got[0].IP != "203.0.113.12" {
		t.Fatalf("%v %+v", r, f.got)
	}
	m.req("POST", "/api/v1/vms/nope/public-ips", secret, `{"action":"add"}`, 404)
}
