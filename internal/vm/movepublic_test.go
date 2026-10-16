package vm

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"p8sos/internal/firewall"
	"p8sos/internal/ippool"
	"p8sos/internal/virt"
)

// A VM with a public IP moves within a cluster (the servers share the pools).
func TestMovePublicVM(t *testing.T) {
	ctx := context.Background()
	pools := &ippool.Store{} // the cluster's pools, seen by both servers
	pools.Add(ippool.Pool{Name: "dc1", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9"})
	a, _, dA, _ := newSvc()
	b, hvB, dB, seedsB := newSvc()
	ma, mb := &memDisks{fakeDisks: dA, data: map[string][]byte{}}, &memDisks{fakeDisks: dB, data: map[string][]byte{}}
	a.Disks, b.Disks = ma, mb
	upB := false
	a.BridgeUp = func(string) bool { return true }
	b.BridgeUp = func(br string) bool { return br == "br0" && upB }
	var pinned []firewall.VM
	a.Firewall = func(context.Context, []firewall.VM) error { return nil }
	b.Firewall = func(_ context.Context, l []firewall.VM) error { pinned = l; return nil }
	a.Pools, b.Pools = pools, pools
	if _, err := a.Create(ctx, CreateRequest{Name: "pub1", Template: "debian-13", IPPool: "dc1", SSHKeys: []string{"ssh-ed25519 AAAA x"}}); err != nil {
		t.Fatal(err)
	}
	a.Stop(ctx, "pub1", true)
	ma.data["p8s-pub1"] = []byte("disk")
	if why, _ := a.MoveCheck(ctx, "pub1"); !strings.Contains(why, "public IP") {
		t.Fatalf("standalone: %q", why)
	}
	a.PoolsShared = func() bool { return true }
	if why, _ := a.MoveCheck(ctx, "pub1"); why != "" {
		t.Fatalf("cluster: %q", why)
	}
	var buf bytes.Buffer
	if err := a.ExportVM(ctx, "pub1", &buf); err != nil {
		t.Fatal(err)
	}
	stream := buf.Bytes()
	if _, _, err := b.ImportMovedVM(ctx, bytes.NewReader(stream)); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "bridge br0") {
		t.Fatalf("no bridge here: %v", err)
	}
	upB = true
	v, note, err := b.ImportMovedVM(ctx, bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if v.Public == nil || v.Public.IP != "203.0.113.10" || !strings.Contains(note, "203.0.113.10 came along") {
		t.Fatalf("%+v %q", v.Public, note)
	}
	sp, _ := virt.ParseDomainXML(hvB.doms["p8s-pub1"])
	if sp.Bridge != "br0" || seedsB.last.Static == nil || seedsB.last.Static.IP != "203.0.113.10" {
		t.Fatalf("bridge %q seed %+v", sp.Bridge, seedsB.last.Static)
	}
	if pinned == nil {
		t.Fatal("firewall not refreshed on the new server")
	}
	// the old copy goes; the address stays the VM's
	if err := a.DeleteMoved(ctx, "pub1"); err != nil {
		t.Fatal(err)
	}
	if l, ok := pools.Lookup("pub1"); !ok || l.IP != "203.0.113.10" {
		t.Fatal("address released with the old copy")
	}
	// deleting the VM itself gives it back
	if err := b.Delete(ctx, "pub1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := pools.Lookup("pub1"); ok {
		t.Fatal("address kept after delete")
	}
}

// A running VM with a public IP moves live within a cluster.
func TestLiveMovePublicVM(t *testing.T) {
	ctx := context.Background()
	pools := &ippool.Store{}
	pools.Add(ippool.Pool{Name: "dc1", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9"})
	src, _, _ := liveSvc(t)
	dst, _, _ := liveSvc(t)
	src.Pools, dst.Pools = pools, pools
	src.BridgeUp = func(string) bool { return true }
	up := false
	dst.BridgeUp = func(b string) bool { return b == "br0" && up }
	src.Firewall = func(context.Context, []firewall.VM) error { return nil }
	dst.Firewall = src.Firewall
	if _, err := src.Create(ctx, CreateRequest{Name: "pub2", Template: "debian-13", IPPool: "dc1", SSHKeys: []string{"ssh-ed25519 AAAA x"}}); err != nil {
		t.Fatal(err)
	}
	if why, _ := src.LiveCheck(ctx, "pub2"); !strings.Contains(why, "public IP") {
		t.Fatalf("standalone: %q", why)
	}
	src.PoolsShared = func() bool { return true }
	if why, _ := src.LiveCheck(ctx, "pub2"); why != "" {
		t.Fatalf("cluster: %q", why)
	}
	h, err := src.LiveHeader(ctx, "pub2")
	if err != nil || h.PublicPool != "dc1" {
		t.Fatalf("%v %q", err, h.PublicPool)
	}
	if _, err := dst.LivePrepare(ctx, h); err == nil || !strings.Contains(err.Error(), "bridge br0") {
		t.Fatalf("no bridge: %v", err)
	}
	up = true
	plan, err := dst.LivePrepare(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.XML, "br0") {
		t.Fatalf("bridge in the plan:\n%s", plan.XML)
	}
	if err := src.LiveCleanup(ctx, "pub2"); err == nil { // still defined here: nothing removed
		t.Fatal("cleanup while defined")
	}
	if _, ok := pools.Lookup("pub2"); !ok {
		t.Fatal("address lost")
	}
}
