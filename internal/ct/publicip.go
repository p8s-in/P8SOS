package ct

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

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

// movedAway: containers being deleted after a move (their address went with them).
var movedAway sync.Map

// DeleteMoved deletes a container that moved to another server; its public address stays.
func (s *Service) DeleteMoved(ctx context.Context, name string) error {
	movedAway.Store(name, true)
	defer movedAway.Delete(name)
	return s.Delete(ctx, name)
}

func (s *Service) releaseIP(name string) {
	if _, moved := movedAway.Load(name); moved {
		return
	}
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
				MAC: mac, IP: l.IP, Prefix: l.Prefix, Gateway: l.Gateway, DNS: l.DNS, OnLink: l.OnLink,
				Extra: l.Extra, IP6: l.IP6, Prefix6: l.Prefix6, Gateway6: l.Gateway6}), 0o600)
		} else {
			_ = os.Remove(filepath.Join(root, "var/lib/cloud/seed/nocloud/network-config"))
		}
	}
	if has("etc/netplan") { // Ubuntu
		y := "network:\n  version: 2\n  ethernets:\n    eth0:\n"
		if l == nil {
			y += "      dhcp4: true\n"
		} else {
			q := []string{fmt.Sprintf("%q", cidr)}
			for _, a := range extraCIDRs(l) {
				q = append(q, fmt.Sprintf("%q", a))
			}
			y += fmt.Sprintf("      addresses: [%s]\n      routes:\n        - to: default\n          via: %s\n", strings.Join(q, ", "), l.Gateway)
			if l.OnLink {
				y += "          on-link: true\n"
			}
			if l.IP6 != "" {
				y += fmt.Sprintf("        - to: \"::/0\"\n          via: %s\n", l.Gateway6)
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
			for _, a := range extraCIDRs(l) {
				n += "Address=" + a + "\n"
			}
			for _, d := range l.DNS {
				n += "DNS=" + d + "\n"
			}
			n += "\n[Route]\nGateway=" + l.Gateway + "\n"
			if l.OnLink {
				n += "GatewayOnLink=yes\n"
			}
			if l.IP6 != "" {
				n += "\n[Route]\nGateway=" + l.Gateway6 + "\n"
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
		if l != nil {
			for _, a := range l.Extra {
				n += fmt.Sprintf("\tup ip addr add %s/%d dev eth0\n", a, l.Prefix)
			}
			if l.IP6 != "" {
				n += fmt.Sprintf("\niface eth0 inet6 static\n\taddress %s/%d\n\tgateway %s\n", l.IP6, l.Prefix6, l.Gateway6)
			}
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
			for i, a := range l.Extra {
				n += fmt.Sprintf("address%d=%s/%d\n", i+2, a, l.Prefix)
			}
			if len(l.DNS) > 0 {
				n += "dns=" + strings.Join(l.DNS, ";") + ";\n"
			}
		}
		if l != nil && l.IP6 != "" {
			n += fmt.Sprintf("\n[ipv6]\nmethod=manual\naddress1=%s/%d,%s\n", l.IP6, l.Prefix6, l.Gateway6)
		} else {
			n += "\n[ipv6]\nmethod=auto\n"
		}
		put("etc/NetworkManager/system-connections/eth0.nmconnection", n, 0o600)
	}
	if has("etc/sysconfig/network-scripts") {
		n := "DEVICE=eth0\nONBOOT=yes\nTYPE=Ethernet\n"
		if l == nil {
			n += "BOOTPROTO=dhcp\n"
		} else {
			n += fmt.Sprintf("BOOTPROTO=none\nIPADDR=%s\nPREFIX=%d\nGATEWAY=%s\n", l.IP, l.Prefix, l.Gateway)
			for i, a := range l.Extra {
				n += fmt.Sprintf("IPADDR%d=%s\nPREFIX%d=%d\n", i+1, a, i+1, l.Prefix)
			}
			if l.IP6 != "" {
				n += fmt.Sprintf("IPV6INIT=yes\nIPV6ADDR=%s/%d\nIPV6_DEFAULTGW=%s\n", l.IP6, l.Prefix6, l.Gateway6)
			}
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

// extraCIDRs are a lease's other addresses as address/prefix (IPv4 extras, then IPv6).
func extraCIDRs(l *ippool.Lease) []string {
	var out []string
	for _, a := range l.Extra {
		out = append(out, fmt.Sprintf("%s/%d", a, l.Prefix))
	}
	if l.IP6 != "" {
		out = append(out, fmt.Sprintf("%s/%d", l.IP6, l.Prefix6))
	}
	return out
}

// ChangePublicIPs applies one change to a container's public addresses: the
// pool, its config and network files at once, and inside it right away when
// it runs (a pool on another bridge needs it stopped).
func (s *Service) ChangePublicIPs(ctx context.Context, name string, r vm.PublicIPRequest) (vm.PublicIPResult, error) {
	if s.Pools == nil {
		return vm.PublicIPResult{}, fmt.Errorf("%w: public IP pools are not available on this host", vm.ErrConflict)
	}
	m, err := s.get(name)
	if err != nil {
		return vm.PublicIPResult{}, err
	}
	old, ok := s.lease(name)
	if !ok || m.Network != vm.NetPublic {
		return vm.PublicIPResult{}, fmt.Errorf("%w: container %s has no public address", vm.ErrConflict, name)
	}
	running := s.state(ctx, name) == "running"
	var nw ippool.Lease
	switch r.Action {
	case "add":
		nw, err = s.Pools.AddExtra(name, strings.TrimSpace(r.IP))
	case "remove":
		nw, err = s.Pools.RemoveExtra(name, strings.TrimSpace(r.IP))
	case "ipv6":
		nw, err = s.Pools.AddV6(name)
	case "change":
		if r.Pool != "" && r.Pool != old.Pool {
			p, gerr := s.Pools.Get(r.Pool)
			if gerr != nil {
				return vm.PublicIPResult{}, vm.PoolError(gerr)
			}
			if p.Bridge != old.Bridge {
				if s.BridgeUp != nil && !s.BridgeUp(p.Bridge) {
					return vm.PublicIPResult{}, fmt.Errorf("%w: the bridge %s of pool %s is not up on this host", vm.ErrInvalid, p.Bridge, p.Name)
				}
				if running {
					return vm.PublicIPResult{}, fmt.Errorf("%w: pool %s is on another bridge (%s): stop %s first", vm.ErrConflict, p.Name, p.Bridge, name)
				}
			}
		}
		_, nw, err = s.Pools.Change(name, r.Pool, strings.TrimSpace(r.IP))
	default:
		return vm.PublicIPResult{}, fmt.Errorf(`%w: action must be "add", "remove", "ipv6" or "change"`, vm.ErrInvalid)
	}
	if err != nil {
		return vm.PublicIPResult{}, vm.PoolError(err)
	}
	res := vm.PublicIPResult{Lease: nw}
	if err := s.writeConfig(name, &m, nw.Bridge); err != nil {
		return res, err
	}
	if err := s.writeNet(name, m.MAC, &nw); err != nil {
		return res, fmt.Errorf("the container's network files: %w", err)
	}
	if s.Refresh != nil {
		_ = s.Refresh(ctx)
	}
	if !running {
		res.Note = "takes effect when it starts"
		return res, nil
	}
	var remove []string
	for _, a := range old.Addrs4() {
		if !contains(nw.Addrs4(), a) {
			remove = append(remove, fmt.Sprintf("%s/%d", a, old.Prefix))
		}
	}
	if old.IP6 != "" && old.IP6 != nw.IP6 {
		remove = append(remove, fmt.Sprintf("%s/%d", old.IP6, old.Prefix6))
	}
	st := cloudinit.Static{MAC: m.MAC, IP: nw.IP, Prefix: nw.Prefix, Gateway: nw.Gateway, DNS: nw.DNS, OnLink: nw.OnLink,
		Extra: nw.Extra, IP6: nw.IP6, Prefix6: nw.Prefix6, Gateway6: nw.Gateway6}
	out, err := s.Run.Run(ctx, "lxc-attach", "-n", name, "--clear-env", "--", "/bin/sh", "-c", cloudinit.ApplyScript(st, remove, false))
	if err != nil {
		res.Note = fmt.Sprintf("saved; restart the container to use it (setting it now failed: %v %s)", err, strings.TrimSpace(string(out)))
		return res, nil
	}
	res.Applied, res.Note = true, "set inside the container (and kept after restarts)"
	return res, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
