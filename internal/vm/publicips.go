package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"p8sos/internal/cloudinit"
	"p8sos/internal/ippool"
	"p8sos/internal/virt"
)

// Changing a VM's public addresses after it was made: extra IPv4 addresses,
// an IPv6 address, or a new main address (from the same or another pool).
// The pool and the anti-spoofing rules change at once; inside the VM the
// address is set through the QEMU guest agent when the VM runs (else the
// result says what to set by hand).

// PublicIPRequest is one change.
type PublicIPRequest struct {
	Action string `json:"action"`         // add | remove | ipv6 | change
	IP     string `json:"ip,omitempty"`   // add: the address wanted (optional); remove: which one; change: the new main one (optional)
	Pool   string `json:"pool,omitempty"` // change: move to this pool ("" = the same)
}

// PublicIPResult says what happened, also inside the guest.
type PublicIPResult struct {
	Lease   ippool.Lease `json:"public"`
	Applied bool         `json:"applied"` // set inside the guest now
	Note    string       `json:"note"`
}

// PoolError turns pool errors into API errors.
func PoolError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ippool.ErrNotFound), errors.Is(err, ippool.ErrInvalid), errors.Is(err, ippool.ErrFull):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	case errors.Is(err, ippool.ErrExists), errors.Is(err, ippool.ErrInUse):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// ChangePublicIPs applies one change to a VM's public addresses.
func (s *Service) ChangePublicIPs(ctx context.Context, name string, r PublicIPRequest) (PublicIPResult, error) {
	if s.Pools == nil {
		return PublicIPResult{}, fmt.Errorf("%w: public IP pools are not available on this host", ErrConflict)
	}
	dom := Prefix + name
	unlock := s.lock(dom)
	v, err := s.get(ctx, dom)
	if err != nil {
		unlock()
		return PublicIPResult{}, err
	}
	if v.Public == nil {
		unlock()
		return PublicIPResult{}, fmt.Errorf("%w: %s has no public address (it is on the %s network)", ErrConflict, name, v.Network)
	}
	old := *v.Public
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
				unlock()
				return PublicIPResult{}, PoolError(gerr)
			}
			if p.Bridge != old.Bridge {
				if s.BridgeUp != nil && !s.BridgeUp(p.Bridge) {
					unlock()
					return PublicIPResult{}, fmt.Errorf("%w: the bridge %s of pool %s is not up on this host", ErrInvalid, p.Bridge, p.Name)
				}
				if v.State != virt.Stopped {
					unlock()
					return PublicIPResult{}, fmt.Errorf("%w: pool %s is on another bridge (%s): shut %s down first", ErrConflict, p.Name, p.Bridge, name)
				}
			}
		}
		_, nw, err = s.Pools.Change(name, r.Pool, strings.TrimSpace(r.IP))
	default:
		unlock()
		return PublicIPResult{}, fmt.Errorf(`%w: action must be "add", "remove", "ipv6" or "change"`, ErrInvalid)
	}
	if err != nil {
		unlock()
		return PublicIPResult{}, PoolError(err)
	}
	if nw.Bridge != old.Bridge {
		if err := s.redefine(ctx, dom, func(sp *virt.Spec) { sp.Bridge = nw.Bridge }); err != nil {
			unlock()
			return PublicIPResult{Lease: nw}, fmt.Errorf("the address changed to %s, but the VM's network card couldn't be moved to %s: %w", nw.IP, nw.Bridge, err)
		}
	}
	unlock()
	s.logErr("firewall after address change of "+name, s.ApplyFirewalls(ctx))
	res := PublicIPResult{Lease: nw}
	st := static(&nw, v.MAC)
	remove := gone(old, nw)
	if v.State == virt.Running {
		if ge, ok := s.HV.(guestExec); ok {
			code, out, gerr := ge.GuestExec(ctx, dom, cloudinit.ApplyScript(*st, remove, true), time.Minute)
			if gerr == nil && code == 0 {
				res.Applied = true
				res.Note = "set inside the VM (" + persisted(out) + ")"
				return res, nil
			}
			if gerr != nil {
				res.Note = "the guest agent didn't answer (" + gerr.Error() + "); "
			} else {
				res.Note = fmt.Sprintf("setting it inside the VM failed (%d: %s); ", code, strings.TrimSpace(out))
			}
		}
	}
	res.Note += "set it inside the VM: " + Manual(nw)
	return res, nil
}

// gone are the addresses (with prefix) the old lease had and the new one hasn't.
func gone(old, nw ippool.Lease) []string {
	have := map[string]bool{}
	for _, a := range nw.Addrs4() {
		have[a] = true
	}
	have[nw.IP6] = true
	var out []string
	for _, a := range old.Addrs4() {
		if !have[a] {
			out = append(out, fmt.Sprintf("%s/%d", a, old.Prefix))
		}
	}
	if old.IP6 != "" && !have[old.IP6] {
		out = append(out, fmt.Sprintf("%s/%d", old.IP6, old.Prefix6))
	}
	return out
}

func persisted(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if h, ok := strings.CutPrefix(strings.TrimSpace(l), "persisted: "); ok {
			if strings.HasPrefix(h, "no") {
				return "for now; " + h
			}
			return "kept after reboots via " + h
		}
	}
	return "done"
}

// Manual says what to type into a guest's network settings.
func Manual(l ippool.Lease) string {
	m := fmt.Sprintf("address %s/%d, gateway %s", l.IP, l.Prefix, l.Gateway)
	if len(l.Extra) > 0 {
		m += fmt.Sprintf(", extra addresses %s (/%d)", strings.Join(l.Extra, ", "), l.Prefix)
	}
	if l.IP6 != "" {
		m += fmt.Sprintf(", IPv6 %s/%d gateway %s", l.IP6, l.Prefix6, l.Gateway6)
	}
	if len(l.DNS) > 0 {
		m += ", DNS " + strings.Join(l.DNS, " ")
	}
	return m
}
