package sdn

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	no := false
	c, err := Validate(Config{VNets: []VNet{{Name: "web", Subnet: "10.60.0.0/24", NAT: &no}, {Name: "db"}, {Name: "x", VNI: 10000}}})
	if err != nil {
		t.Fatal(err)
	}
	if c.VNets[0].Name != "db" || c.VNets[0].VNI != 10002 || c.VNets[0].NAT != nil || c.VNets[1].VNI != 10001 || c.VNets[2].VNI != 10000 {
		t.Fatalf("%+v", c.VNets)
	}
	for _, bad := range []Config{
		{VNets: []VNet{{Name: "Web"}}},
		{VNets: []VNet{{Name: "a"}, {Name: "a"}}},
		{VNets: []VNet{{Name: "a", VNI: 5}, {Name: "b", VNI: 5}}},
		{VNets: []VNet{{Name: "a", Subnet: "10.250.0.0/24"}}},
		{VNets: []VNet{{Name: "a", Subnet: "10.60.0.0/24"}, {Name: "b", Subnet: "10.60.0.128/25"}}},
		{VNets: []VNet{{Name: "a", Subnet: "8.8.8.0/24"}}},
		{VNets: []VNet{{Name: "a", Subnet: "10.60.0.1/24"}}},
		{VNets: []VNet{{Name: "a", Subnet: "10.0.0.0/8"}}},
		{VNets: []VNet{{Name: "a", VNI: 10000000}}},
	} {
		if _, err := Validate(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	pl, _ := PlanOf("10.60.0.0/24")
	if pl.Gateway.String() != "10.60.0.1" || pl.First.String() != "10.60.0.10" || pl.Last.String() != "10.60.0.254" || maskOf(pl) != "255.255.255.0" {
		t.Fatalf("%+v", pl)
	}
	pl, _ = PlanOf("192.168.50.16/28")
	if pl.First.String() != "192.168.50.18" || pl.Last.String() != "192.168.50.30" || maskOf(pl) != "255.255.255.240" {
		t.Fatalf("%+v", pl)
	}
	if VNIOf("vxb10000") != 10000 || VNIOf("virbr0") != 0 || VNIOf("vxbx") != 0 {
		t.Fatal("VNIOf")
	}
}

type fakeRun struct {
	cmds  []string
	links string
	fdb   string
}

func (f *fakeRun) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, c)
	switch {
	case strings.HasPrefix(c, "ip -j link show"):
		return []byte(f.links), nil
	case c == "ip -d -o link show dev vxl10001":
		return []byte("7: vxl10001: <BROADCAST> mtu 1230 qdisc noop\\    link/ether 2a:1b brd ff \\    vxlan id 10001 local 10.250.0.9 dev p8s-wg srcport 0 0 dstport 4789 ttl auto"), nil
	case strings.HasPrefix(c, "bridge -j fdb show"):
		return []byte(f.fdb), nil
	case strings.HasPrefix(c, "systemctl list-units"):
		return []byte("p8s-vnet-old.service loaded active running x\n"), nil
	}
	return nil, nil
}

func (f *fakeRun) has(t *testing.T, want ...string) {
	t.Helper()
	all := strings.Join(f.cmds, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in\n%s", w, all)
		}
	}
}

func TestApply(t *testing.T) {
	dir := t.TempDir()
	st := &Store{Path: filepath.Join(dir, "sdn.json")}
	if _, err := st.Save(Config{VNets: []VNet{{Name: "web", Subnet: "10.60.0.0/24"}, {Name: "db"}}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRun{links: `[{"ifname":"vxb99"},{"ifname":"eth0"},{"ifname":"vxl10001"}]`,
		fdb: `[{"mac":"00:00:00:00:00:00","dst":"10.250.0.7"},{"mac":"52:54:00:aa:bb:cc","dst":"10.250.0.2"}]`}
	m := &Manager{Store: st, Run: f, DataDir: dir, Where: func() Where {
		return Where{Self: "10.250.0.1", Peers: []string{"10.250.0.2"}, Gateway: true}
	}}
	if err := m.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.has(t,
		"ip link add vxb10000 type bridge", "ip link add vxl10000 type vxlan id 10000 local 10.250.0.1 dstport 4789",
		"ip link set vxl10000 mtu 1230", "ip link set vxl10000 master vxb10000",
		"bridge fdb append 00:00:00:00:00:00 dev vxl10000 dst 10.250.0.2",
		"ip link del vxl10001", // wrong local address: made again
		"bridge fdb del 00:00:00:00:00:00 dev vxl10001 dst 10.250.0.7",
		"ip link del vxb99", // a vnet that is gone
		"ip -4 addr replace 10.60.0.1/24 dev vxb10000",
		"systemd-run --unit=p8s-vnet-web", "--dhcp-range=10.60.0.10,10.60.0.254,255.255.255.0,12h",
		"systemctl stop p8s-vnet-old", "ip -4 addr flush dev vxb10001", "nft -f")
	if strings.Contains(strings.Join(f.cmds, "\n"), "ip link del eth0") {
		t.Fatal("touched a device that isn't a vnet")
	}
	if br, err := m.BridgeOf(context.Background(), "web"); err != nil || br != "vxb10000" {
		t.Fatal(br, err)
	}
	if _, err := m.BridgeOf(context.Background(), "nope"); err == nil {
		t.Fatal("unknown vnet")
	}
	if m.NameOf("vxb10001") != "db" || m.NameOf("virbr0") != "" {
		t.Fatal("NameOf")
	}

	// a worker: no gateway, no address on the bridge
	f2 := &fakeRun{links: "[]", fdb: "[]"}
	m2 := &Manager{Store: st, Run: f2, Where: func() Where { return Where{Self: "10.250.0.2", Peers: []string{"10.250.0.1"}} }}
	if err := m2.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(f2.cmds, "\n"); strings.Contains(s, "addr replace") || strings.Contains(s, "systemd-run") {
		t.Fatalf("worker runs a gateway:\n%s", s)
	}
	f2.has(t, "ip -4 addr flush dev vxb10000", "bridge fdb append 00:00:00:00:00:00 dev vxl10000 dst 10.250.0.1")

	// adopting the master's copy
	other := &Store{Path: filepath.Join(dir, "w.json")}
	if ch, err := other.Adopt(st.Path); !ch || err != nil || len(other.Load().VNets) != 2 {
		t.Fatal("adopt", ch, err)
	}
	if ch, _ := other.Adopt(st.Path); ch {
		t.Fatal("adopt again should change nothing")
	}
}
