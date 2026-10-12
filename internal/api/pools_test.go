package api

import "testing"

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
