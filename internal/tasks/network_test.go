package tasks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNetwork(t *testing.T) {
	sys, proc, data := t.TempDir(), t.TempDir(), t.TempDir()
	mk := func(p, v string) {
		os.MkdirAll(filepath.Dir(filepath.Join(sys, p)), 0o755)
		os.WriteFile(filepath.Join(sys, p), []byte(v), 0o644)
	}
	for _, d := range []string{"br0/brif/bond0", "br0/brif/vnet0", "bond0", "enp3s0/device", "enp4s0/device", "wlp2s0/device", "wlp2s0/wireless", "virbr0"} {
		os.MkdirAll(filepath.Join(sys, d), 0o755)
	}
	mk("enp3s0/carrier", "1\n")
	mk("enp3s0/speed", "1000\n")
	mk("enp4s0/carrier", "0\n")
	os.Symlink("../bond0", filepath.Join(sys, "enp3s0/master"))
	os.Symlink("../bond0", filepath.Join(sys, "enp4s0/master"))
	os.WriteFile(filepath.Join(proc, "bond0"), []byte("Bonding Mode: fault-tolerance (active-backup)\nCurrently Active Slave: enp3s0\n"), 0o644)
	os.WriteFile(filepath.Join(data, "ipv6-nat"), []byte("fd12:3456:789a::/64\n"), 0o644)
	n := &Node{LANBridge: "br0", SysNet: sys, ProcBonding: proc, DataDir: data}
	net := n.network()
	if net.Uplink != "bond0" || net.BondMode != "fault-tolerance (active-backup)" || net.IPv6NAT != "fd12:3456:789a::/64" {
		t.Fatalf("%+v", net)
	}
	ports := map[string]Port{}
	for _, p := range net.Ports {
		ports[p.Name] = p
	}
	if len(ports) != 3 || !ports["enp3s0"].Active || !ports["enp3s0"].Carrier || ports["enp3s0"].Speed != 1000 || ports["enp3s0"].In != "bond0" ||
		ports["enp4s0"].Carrier || ports["enp4s0"].Active || !ports["wlp2s0"].Wifi {
		t.Fatalf("%+v", ports)
	}
}
