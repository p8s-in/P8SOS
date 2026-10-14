package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"p8sos/internal/virt"
)

// Memory snapshots ("include RAM", like Proxmox's vmstate): the running VM's
// memory and device state is saved next to the disk snapshot, so rolling
// back resumes the VM exactly where it was — open programs and all.
//
// How: virsh save (the VM pauses while its RAM is written and stops) → ZFS
// snapshot of the disks at that same instant → virsh restore of the same
// file (it runs on). The file is kept as the snapshot's memory. The pause is
// about 1 s per GB of RAM, plus a moment.

// StateSaver is the hypervisor part memory snapshots need (virt.Virsh).
type StateSaver interface {
	SaveState(ctx context.Context, name, file string) error
	RestoreState(ctx context.Context, file string) error
}

// stateRoom is kept free next to the memory files after a save.
const stateRoom = 512 << 20

func (s *Service) stateDir(dom string) string {
	if s.StateDir == "" {
		return ""
	}
	return filepath.Join(s.StateDir, dom)
}

func (s *Service) stateFile(dom, snap string) string {
	d := s.stateDir(dom)
	if d == "" {
		return ""
	}
	return filepath.Join(d, snap+".state")
}

// ramOf reports the saved memory of a snapshot (size; 0 = none).
func (s *Service) ramOf(dom, snap string) int64 {
	f := s.stateFile(dom, snap)
	if f == "" {
		return 0
	}
	st, err := os.Stat(f)
	if err != nil || !st.Mode().IsRegular() {
		return 0
	}
	return st.Size()
}

func (s *Service) markRAM(dom string, snaps []Snapshot) {
	for i := range snaps {
		if n := s.ramOf(dom, snaps[i].Name); n > 0 {
			snaps[i].RAM, snaps[i].RAMBytes = true, n
		}
	}
}

func (s *Service) dropState(dom, snap string) {
	if f := s.stateFile(dom, snap); f != "" {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logErr("remove memory snapshot", err)
		}
	}
}

func (s *Service) dropStates(dom string) {
	if d := s.stateDir(dom); d != "" {
		s.logErr("remove memory snapshots", os.RemoveAll(d))
	}
}

// freeBytes is the free space of the file system holding dir.
var freeBytes = func(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// CreateSnapshotRAM snapshots a running VM's disks together with its memory.
func (s *Service) CreateSnapshotRAM(ctx context.Context, name, snap string) (Snapshot, error) {
	ss, ok := s.HV.(StateSaver)
	if !ok || s.StateDir == "" {
		return Snapshot{}, fmt.Errorf("%w: memory snapshots aren't available on this server", ErrInvalid)
	}
	return s.createSnapshot(ctx, name, snap, ss)
}

// saveRAMAndSnap is the RAM part of createSnapshot (caller holds the lock,
// the VM is running, the name is checked).
func (s *Service) saveRAMAndSnap(ctx context.Context, dom, snap string, ss StateSaver, diskSnap func() error) error {
	spec, err := s.spec(ctx, dom)
	if err != nil {
		return err
	}
	if len(spec.PCI) > 0 {
		return fmt.Errorf("%w: a VM with PCI devices can't save its RAM (the device's state can't be saved); take a normal snapshot", ErrConflict)
	}
	dir := s.stateDir(dom)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	need := int64(spec.MemoryMiB)<<20 + stateRoom
	if free, err := freeBytes(dir); err == nil && free < need {
		return fmt.Errorf("%w: saving the RAM needs about %d MB free in %s (%d MB free); take a snapshot without RAM",
			ErrConflict, need>>20, s.StateDir, free>>20)
	}
	final := s.stateFile(dom, snap)
	part := final + ".part"
	_ = os.Remove(part)
	bg := context.WithoutCancel(ctx) // once paused, always finish and resume
	if err := ss.SaveState(ctx, dom, part); err != nil {
		_ = os.Remove(part)
		// a failed save leaves the VM running (or paused: then resume it)
		if st, _ := s.HV.State(bg, dom); st == virt.Stopped {
			s.logErr("start after failed save", s.HV.Start(bg, dom))
		}
		return fmt.Errorf("saving the VM's memory: %w", err)
	}
	snapErr := diskSnap()
	if snapErr == nil {
		snapErr = os.Rename(part, final)
	}
	// run on from the file in every case
	if err := ss.RestoreState(bg, part2(final, part, snapErr)); err != nil {
		if snapErr != nil {
			_ = os.Remove(part)
		}
		return fmt.Errorf("the snapshot is taken but the VM couldn't continue from its saved memory (%v): start it", err)
	}
	if snapErr != nil {
		_ = os.Remove(part)
		return snapErr
	}
	return nil
}

// part2 is the file the VM resumes from: the kept state, or the part file
// when the disk snapshot failed.
func part2(final, part string, snapErr error) string {
	if snapErr != nil {
		return part
	}
	return final
}
