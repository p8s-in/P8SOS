package vm

import (
	"context"
	"fmt"
	"os/exec"

	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

// Backups of VMs on Ceph: Ceph snapshots of every image, exported whole
// (rbd export) into compressed disk-image backups. They restore onto any
// storage — Ceph again, or this server's ZFS pool.

// imageDisks is what raw backups need from the disk backend (storage.ZFS).
type imageDisks interface {
	DiskDataset(vm string, i int) string // Ceph: pool/image
	DiskBytes(ctx context.Context, vm string, i int) (int64, error)
	CreateSized(ctx context.Context, vm string, i int, bytes int64) (string, error)
}

// imageCmd builds streaming commands (tests replace it).
var imageCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func (s *Service) runCephBackup(ctx context.Context, name string, keep int, p jobs.Progress) (string, error) {
	sd, err := s.snapDisks()
	if err != nil {
		return "", err
	}
	id, ok := s.Disks.(imageDisks)
	if !ok {
		return "", errCephBackup
	}
	dom := Prefix + name
	bid := s.Backups.NewID()
	snap := backupSnapPrefix + bid
	p("taking a snapshot")
	unlock := s.lock(dom)
	v, err := s.get(ctx, dom)
	if err != nil {
		unlock()
		return "", err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		unlock()
		return "", err
	}
	consistent := v.State == virt.Stopped
	if v.State == virt.Running {
		consistent = s.HV.FSFreeze(ctx, dom) == nil
	}
	err = sd.Snapshot(ctx, dom, snap)
	if consistent && v.State == virt.Running {
		s.logErr("thaw "+dom, s.HV.FSThaw(context.WithoutCancel(ctx), dom))
	}
	unlock()
	if err != nil {
		return "", err
	}
	defer func() { s.logErr("remove backup snapshot", sd.DestroySnapshot(context.WithoutCancel(ctx), dom, snap)) }()
	idx := []int{0}
	for _, e := range sp.Extra {
		idx = append(idx, e.Index)
	}
	sizes := map[int]int64{}
	for _, i := range idx {
		n, err := id.DiskBytes(ctx, dom, i)
		if err != nil {
			return "", fmt.Errorf("size of disk %d: %w", i, err)
		}
		sizes[i] = n
	}
	p(fmt.Sprintf("copying %d disk image(s) from Ceph", len(idx)))
	m, err := s.Backups.SaveRaw(ctx, Backup{
		ID: bid, VM: name, Template: v.Template, User: v.User, Network: v.Network, VLAN: v.VLAN, OSType: v.OSType,
		VCPUs: v.VCPUs, MemoryMiB: v.MemoryMiB, DiskGB: v.DiskGB, Consistent: consistent, Owner: v.Owner,
		Pool: poolOf(v), PublicIP: publicIPOf(v),
		DiskBus: sp.DiskBus, NICModel: sp.NICModel, Firmware: sp.Firmware, Extra: backupExtras(sp),
	}, sizes, func(ctx context.Context, i int) *exec.Cmd {
		return imageCmd(ctx, "rbd", "export", "--no-progress", id.DiskDataset(dom, i)+"@"+snap, "-")
	})
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("backup %s of %s saved (%d disk image(s) from Ceph, %s)", m.ID, name, len(idx), humanBytes(m.TotalBytes()))
	if keep > 0 {
		if n, err := s.Backups.Prune(name, keep); err != nil {
			s.logErr("prune backups", err)
		} else if n > 0 {
			msg += fmt.Sprintf(", %d old one(s) removed", n)
		}
	}
	if s.AfterBackup != nil {
		s.AfterBackup("vm", name, m.ID)
	}
	return msg, nil
}

// restoreRawDisks writes a raw backup onto dom's disks: in place (the VM's
// current disks, grown if the backup's are bigger) or onto new ones made on
// storage. Returns the root and data disk paths.
func (s *Service) restoreRawDisks(ctx context.Context, dom string, m Backup, inPlace bool, storage string) (string, []virt.ExtraDisk, error) {
	id, ok := s.Disks.(imageDisks)
	if !ok {
		return "", nil, fmt.Errorf("%w: this server can't restore disk-image backups", ErrConflict)
	}
	sizes := m.Sizes()
	devs := map[int]string{}
	if inPlace {
		sp, err := s.spec(ctx, dom)
		if err != nil {
			return "", nil, err
		}
		have := map[int]string{0: sp.DiskPath}
		for _, e := range sp.Extra {
			have[e.Index] = e.Path
		}
		for i, n := range sizes {
			cur, err := id.DiskBytes(ctx, dom, i)
			if err != nil || have[i] == "" {
				return "", nil, fmt.Errorf("disk %d of the backup isn't on the VM any more; restore it as a new VM", i)
			}
			if cur < n {
				return "", nil, fmt.Errorf("disk %d is smaller now than in the backup; restore it as a new VM", i)
			}
			devs[i] = have[i]
		}
	} else {
		if err := s.place(dom, storage); err != nil {
			return "", nil, err
		}
		for i, n := range sizes {
			d, err := id.CreateSized(ctx, dom, i, n)
			if err != nil {
				s.logErr("rollback restored disks", s.Disks.DestroyDisk(context.WithoutCancel(ctx), dom))
				return "", nil, err
			}
			devs[i] = d
		}
	}
	if err := s.Backups.RestoreRaw(ctx, m, devs); err != nil {
		if !inPlace {
			s.logErr("rollback restored disks", s.Disks.DestroyDisk(context.WithoutCancel(ctx), dom))
		}
		return "", nil, err
	}
	var extra []virt.ExtraDisk
	for _, e := range m.Extra {
		extra = append(extra, virt.ExtraDisk{Index: e.Index, SizeGB: int((sizes[e.Index] + 1<<30 - 1) >> 30), Path: devs[e.Index]})
	}
	return devs[0], extra, nil
}

// rawStorage is where a raw backup restored as a new VM goes: Ceph when this
// server has it (where it came from), else the local pool.
func (s *Service) rawStorage() string {
	for _, st := range s.Storages() {
		if st == SharedStorage {
			return st
		}
	}
	return "local"
}
