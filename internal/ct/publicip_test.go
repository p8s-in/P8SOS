package ct

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/ippool"
	"p8sos/internal/vm"
)

func TestPublicIP(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	s.Pools = &ippool.Store{}
	if _, err := s.Pools.Add(ippool.Pool{Name: "dc1", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9", Bridge: "br0", DNS: []string{"1.1.1.1"}}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.MountDir, "web")
	for _, d := range []string{"etc/systemd/network", "etc/netplan", "etc/NetworkManager", "var/lib/cloud/seed/nocloud"} {
		_ = os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	if _, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", Network: "public"}); !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("public without pool: %v", err)
	}
	if _, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", IPPool: "dc1", Network: "nat"}); !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("pool + nat: %v", err)
	}
	c, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", IPPool: "dc1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicIP != "203.0.113.10" || c.Network != "public" {
		t.Fatalf("%+v", c)
	}
	conf, _ := os.ReadFile(filepath.Join(s.LXCDir, "web", "config"))
	for _, want := range []string{"lxc.net.0.link = br0", "lxc.net.0.ipv4.address = 203.0.113.10/29", "lxc.net.0.ipv4.gateway = 203.0.113.9", "lxc.net.0.veth.pair = " + HostPort("web")} {
		if !strings.Contains(string(conf), want) {
			t.Fatalf("config missing %q:\n%s", want, conf)
		}
	}
	if len(HostPort("a-very-long-container-name-that-goes-on-and-on")) > 15 {
		t.Fatal("veth name too long")
	}
	for f, want := range map[string]string{
		"etc/systemd/network/eth0.network":                        "Address=203.0.113.10/29",
		"etc/netplan/10-lxc.yaml":                                 `addresses: ["203.0.113.10/29"]`,
		"etc/NetworkManager/system-connections/eth0.nmconnection": "address1=203.0.113.10/29,203.0.113.9",
		"var/lib/cloud/seed/nocloud/network-config":               "203.0.113.10/29",
	} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil || !strings.Contains(string(b), want) {
			t.Fatalf("%s: %v\n%s", f, err, b)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc/sysconfig")); err == nil {
		t.Fatal("wrote ifcfg into an image without network-scripts")
	}
	if err := s.Delete(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Pools.Lookup("web"); ok {
		t.Fatal("address not released")
	}
}
