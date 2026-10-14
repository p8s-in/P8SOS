package vm

import (
	"context"
	"fmt"

	"p8sos/internal/virt"
)

// ioTuner changes a running VM's limits at once (virt.Virsh).
type ioTuner interface {
	SetDiskLimits(ctx context.Context, name, target string, l virt.IOLimits) error
	SetNICLimits(ctx context.Context, name, mac string, l virt.IOLimits) error
}

// SetIOLimits caps the VM's disk and network speed (zero = no limit). A
// running VM gets the new limits at once; they also stay for its next start.
func (s *Service) SetIOLimits(ctx context.Context, name string, l virt.IOLimits) (VM, error) {
	if err := l.Check(); err != nil {
		return VM{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if err := s.redefine(ctx, dom, func(sp *virt.Spec) { sp.Limits = l }); err != nil {
		return VM{}, err
	}
	if t, ok := s.HV.(ioTuner); ok && st == virt.Running {
		sp, err := s.spec(ctx, dom)
		if err != nil {
			return VM{}, err
		}
		for _, d := range virt.DiskTargets(sp) {
			if err := t.SetDiskLimits(ctx, dom, d, l); err != nil {
				return VM{}, fmt.Errorf("saved, but the running VM's disk %s didn't take it (%v); it applies at the next start", d, err)
			}
		}
		macs := []string{sp.MAC}
		for _, n := range sp.NICs {
			macs = append(macs, n.MAC)
		}
		for _, m := range macs {
			if err := t.SetNICLimits(ctx, dom, m, l); err != nil {
				return VM{}, fmt.Errorf("saved, but the running VM's network card %s didn't take it (%v); it applies at the next start", m, err)
			}
		}
	}
	return s.get(ctx, dom)
}
