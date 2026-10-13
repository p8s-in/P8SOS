package vlan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestVLAN(t *testing.T) {
	sys := t.TempDir()
	for _, p := range []string{"br0/brif/enp3s0", "br0/brif/vnet3", "enp3s0"} {
		os.MkdirAll(filepath.Join(sys, p), 0o755)
	}
	run := &testfake.Runner{}
	m := &Manager{Run: run, LANBridge: "br0", ListFile: filepath.Join(t.TempDir(), "vlans.list"), SysNet: sys}
	if up, err := m.Uplink(); err != nil || up != "enp3s0" {
		t.Fatalf("uplink %q %v", up, err)
	}
	br, err := m.Bridge(context.Background(), 100)
	if err != nil || br != "br0v100" {
		t.Fatalf("%q %v", br, err)
	}
	calls := strings.Join(run.Calls, "\n")
	for _, w := range []string{"ip link add link enp3s0 name p8sv100 type vlan id 100", "ip link add br0v100 type bridge", "ip link set p8sv100 master br0v100", "disable_ipv6=1"} {
		if !strings.Contains(calls, w) {
			t.Errorf("missing %q in\n%s", w, calls)
		}
	}
	m.Bridge(context.Background(), 20)
	if l := m.List(); len(l) != 2 || l[0] != 20 || l[1] != 100 {
		t.Fatalf("list %v", l)
	}
	b, _ := os.ReadFile(m.ListFile)
	if !strings.Contains(string(b), "enp3s0 100 br0v100") {
		t.Fatal(string(b))
	}
	if m.VLANOf("br0v100") != 100 || m.VLANOf("br0") != 0 || m.VLANOf("virbr0") != 0 || m.VLANOf("br0vx") != 0 {
		t.Fatal("VLANOf")
	}
	if _, err := m.Bridge(context.Background(), 5000); err == nil {
		t.Fatal("5000 accepted")
	}
	// already there: nothing is created again
	os.MkdirAll(filepath.Join(sys, "br0v100"), 0o755)
	os.MkdirAll(filepath.Join(sys, "p8sv100"), 0o755)
	run.Calls = nil
	m.Bridge(context.Background(), 100)
	if len(run.Calls) != 0 {
		t.Fatalf("re-created: %v", run.Calls)
	}
	if _, err := (&Manager{LANBridge: "br9", SysNet: sys}).Uplink(); err == nil {
		t.Fatal("missing bridge")
	}
}
