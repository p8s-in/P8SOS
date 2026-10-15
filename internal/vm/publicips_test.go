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

func TestChangePublicIPs(t *testing.T) {
	ctx := context.Background()
	svc, hv0, _, _ := newSvc()
	hv := &cloneHV{fakeHV: hv0}
	svc.HV = hv
	svc.Pools = &ippool.Store{}
	svc.BridgeUp = func(b string) bool { return true }
	var applied []firewall.VM
	svc.Firewall = func(_ context.Context, vms []firewall.VM) error { applied = vms; return nil }
	svc.Pools.Add(ippool.Pool{Name: "dc1", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9", CIDR6: "2001:db8:5::/64", Gateway6: "fe80::1"})
	svc.Pools.Add(ippool.Pool{Name: "dc2", Bridge: "br1", CIDR: "198.51.100.0/29", Gateway: "198.51.100.1"})
	key := []string{"ssh-ed25519 AAAA test"}
	v, err := svc.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", SSHKeys: key, IPPool: "dc1"})
	if err != nil || v.Public.IP6 != "2001:db8:5::100" {
		t.Fatalf("%+v %v", v.Public, err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "nat1", Template: "debian-13", SSHKeys: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ChangePublicIPs(ctx, "nat1", PublicIPRequest{Action: "add"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("nat VM: %v", err)
	}
	// an extra address, set inside the running VM by the guest agent
	res, err := svc.ChangePublicIPs(ctx, "web", PublicIPRequest{Action: "add"})
	if err != nil || len(res.Lease.Extra) != 1 || !res.Applied {
		t.Fatalf("%+v %v", res, err)
	}
	last := hv.scripts[len(hv.scripts)-1]
	if !strings.Contains(last, "ip addr replace 203.0.113.11/29") || !strings.Contains(last, "ip addr replace 2001:db8:5::100/64") {
		t.Fatalf("script %s", last)
	}
	if len(applied) == 0 || applied[0].LockIP != "203.0.113.10" || len(applied[0].LockMore) != 1 || applied[0].LockIP6[0] != "2001:db8:5::100" {
		t.Fatalf("firewall %+v", applied)
	}
	// a new main address in the same pool: the old one is taken off inside
	res, err = svc.ChangePublicIPs(ctx, "web", PublicIPRequest{Action: "change", IP: "203.0.113.13"})
	if err != nil || res.Lease.IP != "203.0.113.13" {
		t.Fatalf("%+v %v", res, err)
	}
	if last = hv.scripts[len(hv.scripts)-1]; !strings.Contains(last, "ip addr del 203.0.113.10/29") {
		t.Fatalf("old address not removed: %s", last)
	}
	// another pool on another bridge: only when shut down
	if _, err := svc.ChangePublicIPs(ctx, "web", PublicIPRequest{Action: "change", Pool: "dc2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("running, other bridge: %v", err)
	}
	if _, err := svc.Stop(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	res, err = svc.ChangePublicIPs(ctx, "web", PublicIPRequest{Action: "change", Pool: "dc2"})
	if err != nil || res.Lease.Pool != "dc2" || res.Applied || !strings.Contains(res.Note, "198.51.100.2/29") {
		t.Fatalf("%+v %v", res, err)
	}
	sp, _ := virt.ParseDomainXML(hv.doms[Prefix+"web"])
	if sp.Bridge != "br1" {
		t.Fatalf("bridge %s", sp.Bridge)
	}
	if v, _ := svc.Get(ctx, "web"); v.Public == nil || v.Public.IP != "198.51.100.2" || v.Public.IP6 != "" {
		t.Fatalf("%+v", v.Public)
	}
	if _, err := svc.ChangePublicIPs(ctx, "web", PublicIPRequest{Action: "ipv6"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ipv6 in a pool without: %v", err)
	}
	if _, err := svc.ChangePublicIPs(ctx, "web", PublicIPRequest{Action: "jump"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad action: %v", err)
	}
}
