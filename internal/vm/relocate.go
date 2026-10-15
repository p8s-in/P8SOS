package vm

import (
	"context"
	"fmt"
	"strings"

	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

// diskRelocator copies a VM's disks to another storage (storage.ZFS).
type diskRelocator interface {
	Relocate(ctx context.Context, vm, target string, idx []int, progress func(string)) (map[int]string, error)
}

// MoveStorage moves a stopped VM's disks to another storage on this server
// (local ZFS ↔ network storage ↔ Ceph), as a background job. Its snapshots
// can't come along: they are deleted (dropSnaps must say yes when it has any).
func (s *Service) MoveStorage(ctx context.Context, name, target string, dropSnaps bool) (jobs.Job, error) {
	rl, ok := s.Disks.(diskRelocator)
	if !ok || s.Jobs == nil {
		return jobs.Job{}, fmt.Errorf("%w: this server can't move disks between storages", ErrInvalid)
	}
	if target == "" {
		target = "local"
	}
	if msg := s.checkStorage(target); msg != "" {
		return jobs.Job{}, fmt.Errorf("%w: %s", ErrInvalid, msg)
	}
	dom := Prefix + name
	st, err := s.state(ctx, dom)
	if err != nil {
		return jobs.Job{}, err
	}
	if st != virt.Stopped {
		return jobs.Job{}, fmt.Errorf("%w: shut vm %q down first (its disks are copied while it is off)", ErrConflict, name)
	}
	if l := s.linkedClones(ctx, dom); len(l) > 0 {
		return jobs.Job{}, fmt.Errorf("%w: its linked clones (%s) use its disks; delete them or make them independent first", ErrConflict, strings.Join(l, ", "))
	}
	if lk, ok := s.Disks.(linker); ok && lk.LinkedTo(ctx, dom) != "" {
		return jobs.Job{}, fmt.Errorf("%w: %s is a linked clone; make it independent first (Manage → Options)", ErrConflict, name)
	}
	cur := s.storageOf(dom)
	if cur == "" {
		cur = "local"
	}
	if cur == target {
		return jobs.Job{}, fmt.Errorf("%w: vm %q is on %s already", ErrConflict, name, target)
	}
	if sd, err := s.snapDisks(); err == nil {
		if snaps, err := sd.Snapshots(ctx, dom); err == nil && len(snaps) > 0 && !dropSnaps {
			return jobs.Job{}, fmt.Errorf("%w: vm %q has %d snapshot(s); they can't move to another storage and are deleted — confirm with drop_snapshots", ErrConflict, name, len(snaps))
		}
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return jobs.Job{}, err
	}
	idx := []int{0}
	for _, e := range sp.Extra {
		idx = append(idx, e.Index)
	}
	return s.Jobs.Start("move-storage", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		defer s.lock(dom)()
		if st, err := s.state(ctx, dom); err != nil || st != virt.Stopped {
			return "", fmt.Errorf("vm %q must stay shut down while its disks move", name)
		}
		p(fmt.Sprintf("copying %d disk(s) from %s to %s", len(idx), cur, target))
		devs, err := rl.Relocate(ctx, dom, target, idx, func(m string) { p(m) })
		if devs == nil && err != nil {
			return "", err
		}
		if rerr := s.redefine(ctx, dom, func(x *virt.Spec) {
			if d, ok := devs[0]; ok {
				x.DiskPath = d
			}
			for i, e := range x.Extra {
				if d, ok := devs[e.Index]; ok {
					x.Extra[i].Path = d
				}
			}
		}); rerr != nil {
			return "", fmt.Errorf("disks copied but the VM couldn't be updated: %w", rerr)
		}
		s.dropStates(dom) // memory of snapshots that are gone
		if err != nil {
			return "", err // moved; only the old disks were left
		}
		return fmt.Sprintf("%s is on %s now (%d disk(s) moved)", name, target, len(idx)), nil
	})
}
