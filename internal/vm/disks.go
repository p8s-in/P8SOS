package vm

import (
	"context"
	"fmt"
	"sort"

	"p8sos/internal/virt"
)

// Disk is a data disk next to the VM's root disk.
type Disk struct {
	Index  int    `json:"index"`   // 1-8; stays the same while the disk exists
	SizeGB int    `json:"size_gb"` //
	Device string `json:"device"`  // name inside a Linux guest: vdb, vdc ... (sdf ... on SATA/SCSI)
}

// ExtraGB is the size of the VM's data disks together.
func (v VM) ExtraGB() int {
	n := 0
	for _, d := range v.Disks {
		n += d.SizeGB
	}
	return n
}

// extraDisks is the data-disk part of the disk backend (ZFS).
type extraDisks interface {
	CreateExtra(ctx context.Context, vm string, i, sizeGB int) (string, error)
	DestroyExtra(ctx context.Context, vm string, i int) error
	ResizeExtra(ctx context.Context, vm string, i, sizeGB int) error
}

// diskAttacher plugs a disk into a running VM (virsh attach-device --live).
type diskAttacher interface {
	AttachDisk(ctx context.Context, name string, diskXML []byte) error
}

func (s *Service) extras() (extraDisks, error) {
	ed, ok := s.Disks.(extraDisks)
	if !ok {
		return nil, fmt.Errorf("%w: this disk backend can't add disks", ErrInvalid)
	}
	return ed, nil
}

func (s *Service) spec(ctx context.Context, dom string) (virt.Spec, error) {
	b, err := s.HV.XML(ctx, dom)
	if err != nil {
		return virt.Spec{}, err
	}
	return virt.ParseDomainXML(b)
}

func extraAt(sp virt.Spec, index int) int {
	for i, e := range sp.Extra {
		if e.Index == index {
			return i
		}
	}
	return -1
}

// AddDisk gives the VM another (empty) disk. A running VM gets it at once
// when its disks are virtio or SCSI; SATA (Windows default) needs a restart.
// The note says what the user still has to do.
func (s *Service) AddDisk(ctx context.Context, name string, sizeGB int) (VM, string, error) {
	ed, err := s.extras()
	if err != nil {
		return VM{}, "", err
	}
	if sizeGB < 1 || sizeGB > s.Limits.MaxDiskGB {
		return VM{}, "", fmt.Errorf("%w: size_gb must be 1-%d", ErrInvalid, s.Limits.MaxDiskGB)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	if len(sp.Extra) >= virt.MaxExtraDisks {
		return VM{}, "", fmt.Errorf("%w: vm %q already has %d extra disks (the most)", ErrConflict, name, len(sp.Extra))
	}
	idx := 1
	for extraAt(sp, idx) >= 0 {
		idx++
	}
	_ = ed.DestroyExtra(ctx, dom, idx) // a leftover from an earlier failed attempt
	dev, err := ed.CreateExtra(ctx, dom, idx, sizeGB)
	if err != nil {
		return VM{}, "", err
	}
	e := virt.ExtraDisk{Index: idx, Path: dev, SizeGB: sizeGB}
	sp.Extra = append(sp.Extra, e)
	sort.Slice(sp.Extra, func(i, j int) bool { return sp.Extra[i].Index < sp.Extra[j].Index })
	x, err := virt.DomainXML(sp)
	if err == nil {
		err = s.HV.Define(ctx, x)
	}
	if err != nil {
		s.logErr("drop new disk", ed.DestroyExtra(context.WithoutCancel(ctx), dom, idx))
		return VM{}, "", err
	}
	note, target := "", virt.ExtraTarget(sp, idx)
	if st == virt.Running {
		att, ok := s.HV.(diskAttacher)
		switch {
		case !virt.HotPlug(sp) || !ok:
			note = fmt.Sprintf("the disk appears after the VM is shut down and started again (%s disks can't be added while it runs)", busName(sp))
		default:
			dx, err := virt.ExtraDiskXML(sp, e)
			if err == nil {
				err = att.AttachDisk(ctx, dom, dx)
			}
			if err != nil {
				note = "couldn't plug the disk in while the VM runs; it appears after a shut down and start: " + err.Error()
			}
		}
	}
	if note == "" {
		note = fmt.Sprintf("new empty disk %s (%d GB): partition and format it inside the VM", target, sizeGB)
		if sp.OSType == "windows" || sp.OSType == "windows11" {
			note = fmt.Sprintf("new empty %d GB disk: set it up in Windows under Disk Management", sizeGB)
		}
	}
	v, err := s.get(ctx, dom)
	return v, note, err
}

func busName(sp virt.Spec) string {
	if virt.ExtraTarget(sp, 1) == "vdb" {
		return "virtio"
	}
	if sp.DiskBus == "scsi" {
		return "SCSI"
	}
	return "SATA"
}

// ResizeDisk grows data disk index of a stopped VM.
func (s *Service) ResizeDisk(ctx context.Context, name string, index, sizeGB int) (VM, error) {
	ed, err := s.extras()
	if err != nil {
		return VM{}, err
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	i := extraAt(sp, index)
	if i < 0 {
		return VM{}, fmt.Errorf("%w: vm %q has no disk %d", ErrNotFound, name, index)
	}
	cur := sp.Extra[i].SizeGB
	if sizeGB < cur {
		return VM{}, fmt.Errorf("%w: disks can only grow (disk %d is %d GB): shrinking would destroy data", ErrInvalid, index, cur)
	}
	if sizeGB > s.Limits.MaxDiskGB {
		return VM{}, fmt.Errorf("%w: size_gb must be at most %d", ErrInvalid, s.Limits.MaxDiskGB)
	}
	if st != virt.Stopped {
		return VM{}, fmt.Errorf("%w: stop vm %q before growing a disk (it is %s)", ErrConflict, name, st)
	}
	if sizeGB > cur {
		if err := ed.ResizeExtra(ctx, dom, index, sizeGB); err != nil {
			return VM{}, err
		}
		sp.Extra[i].SizeGB = sizeGB
		x, err := virt.DomainXML(sp)
		if err == nil {
			err = s.HV.Define(ctx, x)
		}
		if err != nil {
			return VM{}, err
		}
	}
	return s.get(ctx, dom)
}

// RemoveDisk deletes data disk index of a stopped VM, with everything on it.
func (s *Service) RemoveDisk(ctx context.Context, name string, index int) (VM, error) {
	ed, err := s.extras()
	if err != nil {
		return VM{}, err
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	i := extraAt(sp, index)
	if i < 0 {
		return VM{}, fmt.Errorf("%w: vm %q has no disk %d", ErrNotFound, name, index)
	}
	if st != virt.Stopped {
		return VM{}, fmt.Errorf("%w: stop vm %q before removing a disk (it is %s)", ErrConflict, name, st)
	}
	sp.Extra = append(sp.Extra[:i:i], sp.Extra[i+1:]...)
	x, err := virt.DomainXML(sp)
	if err == nil {
		err = s.HV.Define(ctx, x)
	}
	if err != nil {
		return VM{}, err
	}
	if err := ed.DestroyExtra(ctx, dom, index); err != nil {
		return VM{}, fmt.Errorf("disk %d is detached but its data couldn't be deleted: %w", index, err)
	}
	return s.get(ctx, dom)
}
