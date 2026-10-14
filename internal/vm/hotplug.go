package vm

import (
	"context"
	"fmt"

	"p8sos/internal/virt"
)

// CPU and RAM hot-plug (Proxmox's "Hotplug: CPU, Memory"): with hot-plug on,
// a running VM's vCPUs and RAM can be raised up to its ceilings without a
// restart. Turning it on or off changes the VM's virtual hardware, so the
// VM must be off for that.

// hotPlugger changes a running VM's vCPUs and RAM (virt.Virsh).
type hotPlugger interface {
	SetVCPUs(ctx context.Context, name string, n int) error
	AddMemory(ctx context.Context, name string, mib int) error
}

// memBlock: RAM is added in steps of 128 MiB (Linux puts memory online in
// blocks of that size).
const memBlock = 128

// SetHotplug turns hot-plug on with ceilings (0 = sensible defaults) or off
// (on=false). The VM must be shut down.
func (s *Service) SetHotplug(ctx context.Context, name string, on bool, maxVCPUs, maxMiB int) (VM, error) {
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if st != virt.Stopped {
		return VM{}, fmt.Errorf("%w: shut vm %q down to turn hot-plug on or off (it changes the VM's virtual hardware)", ErrConflict, name)
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if !on {
		maxVCPUs, maxMiB = 0, 0
	} else {
		if maxVCPUs == 0 {
			maxVCPUs = min(s.Limits.MaxVCPUs, max(sp.VCPUs*2, sp.VCPUs+2))
		}
		if maxMiB == 0 {
			maxMiB = min(s.Limits.MaxMemoryMiB, sp.MemoryMiB*2)
		}
		maxMiB = (maxMiB + memBlock - 1) / memBlock * memBlock
		switch {
		case maxVCPUs < sp.VCPUs || maxVCPUs > s.Limits.MaxVCPUs:
			return VM{}, fmt.Errorf("%w: most vCPUs must be %d-%d", ErrInvalid, sp.VCPUs, s.Limits.MaxVCPUs)
		case maxMiB < sp.MemoryMiB || maxMiB > s.Limits.MaxMemoryMiB+memBlock:
			return VM{}, fmt.Errorf("%w: most RAM must be %d-%d MiB", ErrInvalid, sp.MemoryMiB, s.Limits.MaxMemoryMiB)
		case len(sp.PCI) > 0:
			return VM{}, fmt.Errorf("%w: VMs with PCI devices can't use RAM hot-plug", ErrConflict)
		}
	}
	if err := s.redefine(ctx, dom, func(x *virt.Spec) { x.HotCPUs, x.HotMemMiB = maxVCPUs, maxMiB }); err != nil {
		return VM{}, err
	}
	return s.get(ctx, dom)
}

// hotResize is Resize for a running VM with hot-plug on: more vCPUs (or
// fewer), more RAM; the new values also stay for its next start.
func (s *Service) hotResize(ctx context.Context, dom string, sp virt.Spec, r ResizeRequest) (string, error) {
	hp, ok := s.HV.(hotPlugger)
	if !ok {
		return "", fmt.Errorf("%w: this server can't change a running VM", ErrConflict)
	}
	if r.DiskGB > sp.DiskGB {
		return "", fmt.Errorf("%w: shut the VM down to grow its system disk (add a data disk instead: Hardware → Extra disks)", ErrConflict)
	}
	note := ""
	if r.VCPUs != 0 && r.VCPUs != sp.VCPUs {
		if sp.HotCPUs == 0 || r.VCPUs > sp.HotCPUs {
			return "", fmt.Errorf("%w: while it runs, vCPUs can go up to %d (its hot-plug ceiling); shut it down for more", ErrConflict, max(sp.HotCPUs, sp.VCPUs))
		}
		if err := hp.SetVCPUs(ctx, dom, r.VCPUs); err != nil {
			return "", fmt.Errorf("changing vCPUs while it runs: %w", err)
		}
		note = fmt.Sprintf("%d vCPUs now", r.VCPUs)
	}
	if r.MemoryMiB != 0 && r.MemoryMiB != sp.MemoryMiB {
		switch {
		case r.MemoryMiB < sp.MemoryMiB:
			return "", fmt.Errorf("%w: RAM can't be taken away while the VM runs; shut it down to lower it", ErrConflict)
		case sp.HotMemMiB == 0 || r.MemoryMiB > sp.HotMemMiB:
			return "", fmt.Errorf("%w: while it runs, RAM can go up to %d MiB (its hot-plug ceiling); shut it down for more", ErrConflict, max(sp.HotMemMiB, sp.MemoryMiB))
		}
		add := (r.MemoryMiB - sp.MemoryMiB + memBlock - 1) / memBlock * memBlock
		if sp.MemoryMiB+add > sp.HotMemMiB {
			add -= memBlock
		}
		if add <= 0 {
			return "", fmt.Errorf("%w: RAM is added in steps of %d MiB", ErrInvalid, memBlock)
		}
		if err := hp.AddMemory(ctx, dom, add); err != nil {
			return "", fmt.Errorf("adding RAM while it runs: %w", err)
		}
		r.MemoryMiB = sp.MemoryMiB + add
		if note != "" {
			note += ", "
		}
		note += fmt.Sprintf("%d MiB RAM now (Windows: Server or Enterprise edition needed to see it)", r.MemoryMiB)
	}
	if err := s.redefine(ctx, dom, func(x *virt.Spec) {
		if r.VCPUs != 0 {
			x.VCPUs = r.VCPUs
		}
		if r.MemoryMiB != 0 {
			x.MemoryMiB = r.MemoryMiB
		}
	}); err != nil {
		return "", err
	}
	return note, nil
}
