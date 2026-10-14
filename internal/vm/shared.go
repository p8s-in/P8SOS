package vm

import (
	"context"
	"fmt"
	"time"

	"p8sos/internal/virt"
)

// VMs on shared storage (Ceph): every server sees the same disks, so moving
// one or starting it elsewhere after a failure copies no disk at all. The
// storage keeps a disk open ("mapped") on the server where the VM is defined
// and never deletes a disk another server has open.

// sharedDisks is the storage part for that (storage.ZFS with Ceph).
type sharedDisks interface {
	Shared(vm string) bool
	Release(ctx context.Context, vm string) error
}

// SharedStorage is the storage name of shared VM disks.
const SharedStorage = "ceph"

func (s *Service) shared(dom string) bool {
	sd, ok := s.Disks.(sharedDisks)
	return ok && sd.Shared(dom)
}

// release closes a shared VM's disks on this server (they stay in the pool).
func (s *Service) release(ctx context.Context, dom string) error {
	if sd, ok := s.Disks.(sharedDisks); ok {
		return sd.Release(ctx, dom)
	}
	return nil
}

// dropLeftover removes disks left by an interrupted create, move or import.
// Shared disks are never removed this way: a disk of that name in the pool
// may belong to a VM on another server.
func (s *Service) dropLeftover(ctx context.Context, dom string) error {
	if s.shared(dom) {
		return nil
	}
	return s.Disks.DestroyDisk(ctx, dom)
}

// attachShared opens a shared VM's disks here (root, then data disks) and
// returns the root disk and the data disks with their devices.
func (s *Service) attachShared(ctx context.Context, h MoveHeader) (string, []virt.ExtraDisk, error) {
	dom := Prefix + h.Name
	if why := s.checkStorage(h.Shared); why != "" {
		return "", nil, fmt.Errorf("%w: %s", ErrConflict, why)
	}
	if err := s.place(dom, h.Shared); err != nil {
		return "", nil, err
	}
	dw, ok := s.Disks.(deviceWaiter)
	if !ok {
		return "", nil, fmt.Errorf("%w: this server can't open shared disks", ErrConflict)
	}
	disk, err := dw.WaitDevice(ctx, dom)
	if err != nil {
		return "", nil, err
	}
	var extra []virt.ExtraDisk
	for _, d := range h.Extra {
		ew, ok := s.Disks.(extraWaiter)
		if !ok {
			return "", nil, fmt.Errorf("%w: this server can't open data disks", ErrConflict)
		}
		p, err := ew.WaitExtra(ctx, dom, d.Index)
		if err != nil {
			s.logErr("close shared disks", s.release(context.WithoutCancel(ctx), dom))
			return "", nil, err
		}
		extra = append(extra, virt.ExtraDisk{Index: d.Index, Path: p, SizeGB: d.SizeGB})
	}
	return disk, extra, nil
}

func repSnapNow() string { return fmt.Sprintf("p8s-rep-%d", time.Now().UnixNano()) }
