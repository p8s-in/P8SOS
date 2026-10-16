package vm

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"p8sos/internal/cloudinit"
	"p8sos/internal/ippool"
)

// NetPublic is a VM with a public IP address from an IP pool (static, set by
// cloud-init) on the pool's bridge.
const NetPublic = "public"

// checkPool validates CreateRequest.IPPool; it returns a problem or "".
func (s *Service) checkPool(r *CreateRequest) string {
	if r.IPPool == "" {
		if r.Network == NetPublic {
			return `network "public" needs ip_pool (which block of public addresses)`
		}
		return ""
	}
	if s.Pools == nil {
		return "public IP pools are not available on this host"
	}
	p, err := s.Pools.Get(r.IPPool)
	if err != nil {
		return fmt.Sprintf("no ip pool %q", r.IPPool)
	}
	if s.BridgeUp != nil && !s.BridgeUp(p.Bridge) {
		return fmt.Sprintf("the bridge %s of pool %s is not up on this host", p.Bridge, p.Name)
	}
	if p.Free == 0 {
		return fmt.Sprintf("pool %s has no free addresses (%d of %d used)", p.Name, p.Size-p.Free, p.Size)
	}
	if r.Network != "" && r.Network != NetPublic {
		return `ip_pool can't be combined with network "nat" or "lan"`
	}
	r.Network = NetPublic
	return ""
}

// lease takes an address for a new VM; release undoes it.
func (s *Service) lease(pool, vm, prefer string) (*ippool.Lease, func(), error) {
	l, err := s.Pools.Allocate(pool, vm, prefer)
	switch {
	case errors.Is(err, ippool.ErrFull), errors.Is(err, ippool.ErrNotFound):
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	case errors.Is(err, ippool.ErrExists):
		return nil, nil, fmt.Errorf("%w: %v", ErrConflict, err)
	case err != nil:
		return nil, nil, err
	}
	return &l, func() { s.logErr("release ip "+l.IP, s.Pools.Release(vm)) }, nil
}

func static(l *ippool.Lease, mac string) *cloudinit.Static {
	return &cloudinit.Static{MAC: mac, IP: l.IP, Prefix: l.Prefix, Gateway: l.Gateway, DNS: l.DNS, OnLink: l.OnLink,
		Extra: l.Extra, IP6: l.IP6, Prefix6: l.Prefix6, Gateway6: l.Gateway6}
}

// movedAway: VMs being deleted after a move; their address went with them.
var movedAway sync.Map

// DeleteMoved deletes a VM that was moved to another server: its public
// address stays (it belongs to the VM on its new server now).
func (s *Service) DeleteMoved(ctx context.Context, name string) error {
	movedAway.Store(name, true)
	defer movedAway.Delete(name)
	return s.Delete(ctx, name)
}

// releaseIP frees a deleted VM's address and its pinning rules.
func (s *Service) releaseIP(ctx context.Context, name string) {
	if s.Pools == nil {
		return
	}
	if _, moved := movedAway.Load(name); moved {
		s.logErr("firewall after move of "+name, s.ApplyFirewalls(ctx))
		return
	}
	if _, had := s.Pools.Lookup(name); !had {
		return
	}
	s.logErr("release ip of "+name, s.Pools.Release(name))
	s.logErr("firewall after delete of "+name, s.ApplyFirewalls(ctx))
}
