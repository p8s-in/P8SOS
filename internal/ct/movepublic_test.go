package ct

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/ippool"
)

// A container with a public IP moves within a cluster (shared pools).
func TestMovePublicContainer(t *testing.T) {
	s, run := newSvc(t)
	ctx := context.Background()
	s.Pools = &ippool.Store{}
	s.Pools.Add(ippool.Pool{Name: "dc1", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9", Bridge: "br0"})
	_ = os.MkdirAll(filepath.Join(s.MountDir, "web"), 0o755)
	if _, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", IPPool: "dc1"}); err != nil {
		t.Fatal(err)
	}
	run.Out["lxc-info -n web -sH"] = "STOPPED\n"
	if why, _ := s.MoveCheck(ctx, "web"); !strings.Contains(why, "public IP") {
		t.Fatalf("standalone: %q", why)
	}
	s.PoolsShared = func() bool { return true }
	if why, _ := s.MoveCheck(ctx, "web"); why != "" {
		t.Fatalf("cluster: %q", why)
	}
	if why, _ := s.LiveMoveCheck(ctx, "web"); why != "" {
		t.Fatalf("live: %q", why)
	}
	// the other server: its bridge is needed
	h := MoveHeader{Name: "web", Template: "debian-13", VCPUs: 1, MemoryMiB: 512, DiskGB: 8, MAC: "52:54:00:aa:bb:cc", Network: "public"}
	s.BridgeUp = func(string) bool { return false }
	if _, _, err := s.finishImport(ctx, h); err == nil || !strings.Contains(err.Error(), "bridge br0") {
		t.Fatalf("no bridge: %v", err)
	}
	s.BridgeUp = func(b string) bool { return b == "br0" }
	_, note, err := s.finishImport(ctx, h)
	if err != nil || !strings.Contains(note, "203.0.113.10 came along") {
		t.Fatalf("%q %v", note, err)
	}
	// the old copy goes, the address stays
	if err := s.DeleteMoved(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Pools.Lookup("web"); !ok {
		t.Fatal("address released with the old copy")
	}
}
