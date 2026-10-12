package ct

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"p8sos/internal/cloudinit"
	"p8sos/internal/ippool"
	"p8sos/internal/vm"
)

// Public IPs for containers come from the same pools as VMs' (leases keyed by
// name; containers and VMs share names). The address is set three ways so
// every distro picks it up: in the LXC config (before init runs), in the
// cloud-init seed, and in the distro's own network files.

// HostPort is the host side of a container's veth (fixed, at most 15 chars).
func HostPort(name string) string {
	h := sha256.Sum256([]byte(name))
	return fmt.Sprintf("vc%x", h[:6])
}

func (s *Service) lease(name string) (ippool.Lease, bool) {
	if s.Pools == nil {
		return ippool.Lease{}, false
	}
	return s.Pools.Lookup(name)
}

// checkPool validates a public-address request (fills in Network).
func (s *Service) checkPool(r *CreateRequest) error {
	if r.IPPool == "" {
		if r.Network == vm.NetPublic {
			return fmt.Errorf(`%w: network "public" needs ip_pool (which block of public addresses)`, vm.ErrInvalid)
		}
		return nil
	}
	if s.Pools == nil {
		return fmt.Errorf("%w: public IP pools are not available on this host", vm.ErrInvalid)
	}
	p, err := s.Pools.Get(r.IPPool)
	if err != nil {
		return fmt.Errorf("%w: no ip pool %q", vm.ErrInvalid, r.IPPool)
	}
	if s.BridgeUp != nil && !s.BridgeUp(p.Bridge) {
		return fmt.Errorf("%w: the bridge %s of pool %s is not up on this host", vm.ErrInvalid, p.Bridge, p.Name)
	}
	if p.Free == 0 {
		return fmt.Errorf("%w: pool %s has no free addresses", vm.ErrInvalid, p.Name)
	}
	if r.Network != "" && r.Network != vm.NetPublic {
		return fmt.Errorf(`%w: ip_pool can't be combined with network "nat" or "lan"`, vm.ErrInvalid)
	}
	r.Network = vm.NetPublic
	return nil
}

// takeIP leases an address (prefer = the old one, for restores).
func (s *Service) takeIP(pool, name, prefer string) (ippool.Lease, error) {
	l, err := s.Pools.Allocate(pool, name, prefer)
	switch {
	case errors.Is(err, ippool.ErrFull), errors.Is(err, ippool.ErrNotFound):
		return l, fmt.Errorf("%w: %v", vm.ErrInvalid, err)
	case errors.Is(err, ippool.ErrExists):
		return l, fmt.Errorf("%w: %v", vm.ErrConflict, err)
	}
	return l, err
}

func (s *Service) releaseIP(name string) {
	if s.Pools != nil {
		_ = s.Pools.Release(name)
	}
}

// writeNet writes the network setup inside the container: a static address
// (l != nil) or DHCP. Only the systems that exist in the image are touched.
func (s *Service) writeNet(name string, mac string, l *ippool.Lease) error {
	root := s.rootfs(name)
	has := func(rel string) bool { st, err := os.Lstat(filepath.Join(root, rel)); return err == nil && st.IsDir() }
	var errs []error
	put := func(rel, data string, mode os.FileMode) {
		if err := s.writeAs(name, rel, []byte(data), mode); err != nil {
			errs = append(errs, err)
		}
	}
	cidr := ""
	if l != nil {
		cidr = fmt.Sprintf("%s/%d", l.IP, l.Prefix)
	}
	// cloud-init: otherwise it writes its DHCP fallback over ours on first boot
	if has("var/lib/cloud/seed/nocloud") {
		if l != nil {
			put("var/lib/cloud/seed/nocloud/network-config", cloudinit.NetworkConfig(cloudinit.Static{
				MAC: mac, IP: l.IP, Prefix: l.Prefix, Gateway: l.Gateway, DNS: l.DNS, OnLink: l.OnLink}), 0o600)
		} else {
			_ = os.Remove(filepath.Join(root, "var/lib/cloud/seed/nocloud/network-config"))
		}
	}
	if has("etc/netplan") { // Ubuntu
		y := "network:\n  version: 2\n  ethernets:\n    eth0:\n"
		if l == nil {
			y += "      dhcp4: true\n"
		} else {
			y += fmt.Sprintf("      addresses: [%q]\n      routes:\n        - to: default\n          via: %s\n", cidr, l.Gateway)
			if l.OnLink {
				y += "          on-link: true\n"
			}
			if len(l.DNS) > 0 {
				y += fmt.Sprintf("      nameservers:\n        addresses: [%s]\n", strings.Join(l.DNS, ", "))
			}
		}
		put("etc/netplan/10-lxc.yaml", y, 0o600)
	}
	if has("etc/systemd/network") { // Debian, Arch, Fedora cloud images
		n := "[Match]\nName=eth0\n\n[Network]\n"
		if l == nil {
			n += "DHCP=yes\n"
		} else {
			n += "Address=" + cidr + "\n"
			for _, d := range l.DNS {
				n += "DNS=" + d + "\n"
			}
			n += "\n[Route]\nGateway=" + l.Gateway + "\n"
			if l.OnLink {
				n += "GatewayOnLink=yes\n"
			}
		}
		put("etc/systemd/network/eth0.network", n, 0o644)
	}
	if st, err := os.Lstat(filepath.Join(root, "etc/network/interfaces")); err == nil && st.Mode().IsRegular() { // Alpine, ifupdown
		n := "auto lo\niface lo inet loopback\n\nauto eth0\n"
		switch {
		case l == nil:
			n += "iface eth0 inet dhcp\n"
		case l.OnLink:
			n += fmt.Sprintf("iface eth0 inet static\n\taddress %s\n\tpost-up ip route add %s dev eth0\n\tpost-up ip route add default via %s\n", cidr, l.Gateway, l.Gateway)
		default:
			n += fmt.Sprintf("iface eth0 inet static\n\taddress %s\n\tgateway %s\n", cidr, l.Gateway)
		}
		if err := s.rewrite(root, "etc/network/interfaces", []byte(n)); err != nil {
			errs = append(errs, err)
		}
	}
	if has("etc/NetworkManager") { // Rocky, Alma
		n := "[connection]\nid=eth0\ntype=ethernet\ninterface-name=eth0\n\n[ipv4]\n"
		if l == nil {
			n += "method=auto\n"
		} else {
			n += fmt.Sprintf("method=manual\naddress1=%s,%s\n", cidr, l.Gateway)
			if len(l.DNS) > 0 {
				n += "dns=" + strings.Join(l.DNS, ";") + ";\n"
			}
		}
		n += "\n[ipv6]\nmethod=auto\n"
		put("etc/NetworkManager/system-connections/eth0.nmconnection", n, 0o600)
	}
	if has("etc/sysconfig/network-scripts") {
		n := "DEVICE=eth0\nONBOOT=yes\nTYPE=Ethernet\n"
		if l == nil {
			n += "BOOTPROTO=dhcp\n"
		} else {
			n += fmt.Sprintf("BOOTPROTO=none\nIPADDR=%s\nPREFIX=%d\nGATEWAY=%s\n", l.IP, l.Prefix, l.Gateway)
			for i, d := range l.DNS {
				n += fmt.Sprintf("DNS%d=%s\n", i+1, d)
			}
		}
		put("etc/sysconfig/network-scripts/ifcfg-eth0", n, 0o644)
	}
	// name servers for systems without a resolver service (plain file only)
	if l != nil && len(l.DNS) > 0 {
		if st, err := os.Lstat(filepath.Join(root, "etc/resolv.conf")); err == nil && st.Mode().IsRegular() {
			r := ""
			for _, d := range l.DNS {
				r += "nameserver " + d + "\n"
			}
			_ = s.rewrite(root, "etc/resolv.conf", []byte(r))
		}
	}
	return errors.Join(errs...)
}
