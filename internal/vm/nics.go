package vm

import (
	"context"
	"fmt"
	"strings"

	"p8sos/internal/virt"
)

// NIC is a VM's second, third … network card (the first is MAC/Network/VLAN).
type NIC struct {
	MAC     string `json:"mac"`
	Network string `json:"network"`        // "nat" or "lan"
	VLAN    int    `json:"vlan,omitempty"` // tagged VLAN on the home network
	Device  string `json:"device"`         // its name inside a typical Linux guest (enp2s0 / eth1 …: shown as "card 2")
}

// nicAttacher plugs network cards into a running VM and out again.
type nicAttacher interface {
	AttachDisk(ctx context.Context, name string, xml []byte) error // virsh attach-device --live (any device)
	DetachDevice(ctx context.Context, name string, xml []byte) error
}

func nicAt(sp virt.Spec, mac string) int {
	for i, n := range sp.NICs {
		if strings.EqualFold(n.MAC, mac) {
			return i
		}
	}
	return -1
}

// AddNIC gives the VM another network card on network ("nat" or "lan", with
// an optional VLAN). A running VM gets it at once (virtio and e1000 cards can
// be plugged in live); the note says what to do inside the guest.
func (s *Service) AddNIC(ctx context.Context, name, network string, vid int) (VM, string, error) {
	switch network {
	case "", NetNAT:
		network = NetNAT
	case NetLAN:
		if !s.LANAvailable() {
			return VM{}, "", fmt.Errorf("%w: the home network isn't set up on this server (Server page → Home network)", ErrInvalid)
		}
	default:
		return VM{}, "", fmt.Errorf("%w: network must be \"nat\" or \"lan\" (a public address is only for the first card)", ErrInvalid)
	}
	if msg := s.checkVLAN(network, vid); msg != "" {
		return VM{}, "", fmt.Errorf("%w: %s", ErrInvalid, msg)
	}
	dom := Prefix + name
	unlock := s.lock(dom)
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	if len(sp.NICs) >= virt.MaxExtraNICs {
		return VM{}, "", fmt.Errorf("%w: vm %q already has %d network cards (the most)", ErrConflict, name, len(sp.NICs)+1)
	}
	bridge, err := s.bridgeVLAN(ctx, network, vid)
	if err != nil {
		return VM{}, "", err
	}
	if bridge == "" {
		return VM{}, "", fmt.Errorf("%w: no bridge for network %q on this server", ErrInvalid, network)
	}
	e := virt.ExtraNIC{MAC: virt.RandomMAC(), Bridge: bridge}
	sp.NICs = append(sp.NICs, e)
	x, err := virt.DomainXML(sp)
	if err == nil {
		err = s.HV.Define(ctx, x)
	}
	if err != nil {
		return VM{}, "", err
	}
	what := "private network"
	if network == NetLAN {
		what = "home network"
		if vid > 0 {
			what = fmt.Sprintf("VLAN %d", vid)
		}
	}
	note := fmt.Sprintf("network card %d (%s, MAC %s) added", len(sp.NICs)+1, what, e.MAC)
	if st == virt.Running {
		att, ok := s.HV.(nicAttacher)
		if !ok {
			note += "; it appears after the VM is shut down and started again"
		} else if nx, err := virt.NICXML(sp, e); err != nil {
			note += "; it appears after a shut down and start: " + err.Error()
		} else if err := att.AttachDisk(ctx, dom, nx); err != nil {
			note += "; couldn't plug it in while the VM runs, it appears after a shut down and start: " + err.Error()
		} else {
			note += " and plugged in"
		}
	}
	if sp.OSType == "windows" || sp.OSType == "windows11" {
		note += ". Windows sets it up by itself (DHCP)"
	} else {
		note += ". Most Linux systems ask DHCP on it by themselves; if not, bring it up inside the VM (e.g. sudo dhclient <the new interface>)"
	}
	unlock()
	unlock = nil
	s.refreshFirewall(ctx)
	v, err := s.get(ctx, dom)
	return v, note, err
}

// RemoveNIC takes network card mac out of the VM (live when it runs).
func (s *Service) RemoveNIC(ctx context.Context, name, mac string) (VM, error) {
	dom := Prefix + name
	unlock := s.lock(dom)
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if strings.EqualFold(sp.MAC, mac) {
		return VM{}, fmt.Errorf("%w: that is the VM's first network card; it can't be removed", ErrInvalid)
	}
	i := nicAt(sp, mac)
	if i < 0 {
		return VM{}, fmt.Errorf("%w: vm %q has no network card %s", ErrNotFound, name, mac)
	}
	e := sp.NICs[i]
	if st == virt.Running {
		att, ok := s.HV.(nicAttacher)
		if !ok {
			return VM{}, fmt.Errorf("%w: shut vm %q down to remove a network card", ErrConflict, name)
		}
		nx, err := virt.NICXML(sp, e)
		if err == nil {
			err = att.DetachDevice(ctx, dom, nx)
		}
		if err != nil {
			return VM{}, fmt.Errorf("couldn't unplug the network card while the VM runs (shut it down and try again): %w", err)
		}
	}
	sp.NICs = append(sp.NICs[:i:i], sp.NICs[i+1:]...)
	x, err := virt.DomainXML(sp)
	if err == nil {
		err = s.HV.Define(ctx, x)
	}
	if err != nil {
		return VM{}, err
	}
	unlock()
	unlock = nil
	s.refreshFirewall(ctx)
	return s.get(ctx, dom)
}

// nicsOf lists the extra cards of a domain for the API.
func (s *Service) nicsOf(sp virt.Spec) []NIC {
	var out []NIC
	for i, n := range sp.NICs {
		out = append(out, NIC{MAC: n.MAC, Network: s.networkOf(n.Bridge), VLAN: s.vlanOf(n.Bridge), Device: fmt.Sprintf("card %d", i+2)})
	}
	return out
}

// refreshFirewall re-applies the firewall table (new cards must be covered by
// the VM's firewall at once). Errors are logged: the card works regardless.
func (s *Service) refreshFirewall(ctx context.Context) {
	if s.Firewall == nil {
		return
	}
	s.logErr("firewall after network card change", s.ApplyFirewalls(ctx))
}
