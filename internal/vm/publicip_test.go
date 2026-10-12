package vm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"p8sos/internal/firewall"
	"p8sos/internal/ippool"
	"p8sos/internal/virt"
)

func TestPublicIP(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, seeds := newSvc()
	svc.Pools = &ippool.Store{}
	up := true
	svc.BridgeUp = func(b string) bool { return b == "br0" && up }
	var applied []firewall.VM
	svc.Firewall = func(_ context.Context, vms []firewall.VM) error { applied = vms; return nil }
	key := []string{"ssh-ed25519 AAAA test"}

	if _, err := svc.Create(ctx, CreateRequest{Name: "pub1", Template: "debian-13", SSHKeys: key, IPPool: "nope"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown pool: %v", err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "pub1", Template: "debian-13", SSHKeys: key, Network: NetPublic}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("public without pool: %v", err)
	}
	if _, err := svc.Pools.Add(ippool.Pool{Name: "dc1", Bridge: "br0", CIDR: "203.0.113.8/30", Gateway: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "pub1", Template: "debian-13", SSHKeys: key, IPPool: "dc1", Network: NetLAN}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pool + lan: %v", err)
	}
	v, err := svc.Create(ctx, CreateRequest{Name: "pub1", Template: "debian-13", SSHKeys: key, IPPool: "dc1"})
	if err != nil {
		t.Fatal(err)
	}
	// /30: .8 network, .9 gateway, .11 broadcast -> only .10
	if v.Network != NetPublic || v.Public == nil || v.Public.IP != "203.0.113.10" || v.IP != "203.0.113.10" {
		t.Fatalf("public VM %+v public=%+v", v, v.Public)
	}
	st := seeds.last.Static
	if st == nil || st.IP != "203.0.113.10" || st.Prefix != 30 || st.Gateway != "203.0.113.9" || st.MAC != v.MAC {
		t.Fatalf("cloud-init static config %+v", st)
	}
	sp, _ := virt.ParseDomainXML(hv.doms[Prefix+"pub1"])
	if sp.Bridge != "br0" {
		t.Fatalf("VM on bridge %q, want the pool's br0", sp.Bridge)
	}
	if len(applied) != 1 || applied[0].LockIP != "203.0.113.10" || applied[0].MAC != v.MAC || len(applied[0].Ports) == 0 {
		t.Fatalf("address not pinned: %+v", applied)
	}
	// pool full
	if _, err := svc.Create(ctx, CreateRequest{Name: "pub2", Template: "debian-13", SSHKeys: key, IPPool: "dc1"}); err == nil || !strings.Contains(err.Error(), "no free") {
		t.Fatalf("full pool: %v", err)
	}
	// bridge gone
	up = false
	if _, err := svc.Pools.Add(ippool.Pool{Name: "dc2", Bridge: "br0", CIDR: "198.51.100.0/29", Gateway: "198.51.100.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "pub3", Template: "debian-13", SSHKeys: key, IPPool: "dc2"}); err == nil || !strings.Contains(err.Error(), "not up") {
		t.Fatalf("bridge down: %v", err)
	}
	up = true
	// delete gives the address back
	if err := svc.Delete(ctx, "pub1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Pools.Lookup("pub1"); ok {
		t.Fatal("address kept after delete")
	}
	if p, _ := svc.Pools.Get("dc1"); p.Free != 1 {
		t.Fatalf("pool after delete %+v", p)
	}
	if len(applied) != 0 {
		t.Fatalf("pin left after delete: %+v", applied)
	}
}
