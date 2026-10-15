// Package storage manages VM disks as ZFS zvols cloned from template snapshots.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"p8sos/internal/execx"
)

type ZFS struct {
	Run     execx.Runner
	Dataset string            // parent dataset for VM disks, e.g. tank/p8s/vms
	Exists  func(string) bool // checks a device path; nil = os.Stat (tests fake it)
	Wait    time.Duration     // max wait for /dev/zvol/...; 0 = 30s
	// BusyRetry is the pause between destroy attempts while the disk is busy; 0 = 1s.
	BusyRetry time.Duration
	// Pools are other storages for VM disks: name -> parent dataset (nil = none).
	Pools func() map[string]string
	// Ceph is the Ceph pool for VM disks (storage "ceph"); nil = none.
	Ceph *RBD

	place placement
}

func (z *ZFS) exists(p string) bool {
	if z.Exists != nil {
		return z.Exists(p)
	}
	_, err := os.Stat(p)
	return err == nil
}

func (z *ZFS) diskDataset(vm string) string { return z.parent(vm) + "/" + vm + "-disk0" }

// DiskDataset is the dataset of the VM's disk i (0 = root).
func (z *ZFS) DiskDataset(vm string, i int) string {
	if z.Shared(vm) {
		return z.Ceph.spec(z.Ceph.image(vm, i))
	}
	return fmt.Sprintf("%s/%s-disk%d", z.parent(vm), vm, i)
}

// DevicePath is the block device libvirt attaches for the VM's root disk.
func (z *ZFS) DevicePath(vm string) string {
	if z.Shared(vm) {
		return z.Ceph.Dev(z.Ceph.image(vm, 0))
	}
	return "/dev/zvol/" + z.diskDataset(vm)
}

// CloneDisk creates the VM's root disk as a copy-on-write clone of the
// template snapshot, then grows it to sizeGB. Clones are instant and use no
// space until the guest writes.
func (z *ZFS) CloneDisk(ctx context.Context, snapshot, vm string, sizeGB int) (string, error) {
	return z.cloneAt(ctx, snapshot, vm, 0, sizeGB)
}

// CloneExtra makes the VM's data disk i from a template's data disk snapshot.
func (z *ZFS) CloneExtra(ctx context.Context, snapshot, vm string, i, sizeGB int) (string, error) {
	return z.cloneAt(ctx, snapshot, vm, i, sizeGB)
}

func (z *ZFS) cloneAt(ctx context.Context, snapshot, vm string, i, sizeGB int) (string, error) {
	if z.Shared(vm) {
		return z.cephClone(ctx, snapshot, vm, i, sizeGB)
	}
	if i > 0 {
		ds := z.DiskDataset(vm, i)
		if poolOf(snapshot) != poolOf(ds) {
			if err := z.copyTemplate(ctx, snapshot, ds); err != nil {
				_ = z.destroy(context.WithoutCancel(ctx), ds)
				return "", fmt.Errorf("copy template disk to %s: %w", poolOf(ds), err)
			}
		} else if _, err := z.Run.Run(ctx, "zfs", "clone", "-p", "-o", "volmode=dev", snapshot, ds); err != nil {
			return "", fmt.Errorf("clone data disk: %w", err)
		}
		if err := z.ResizeExtra(ctx, vm, i, sizeGB); err != nil {
			_ = z.destroy(context.WithoutCancel(ctx), ds)
			return "", err
		}
		return z.WaitExtra(ctx, vm, i)
	}
	ds := z.diskDataset(vm)
	if poolOf(snapshot) != poolOf(ds) { // network storage: a full copy
		if err := z.copyTemplate(ctx, snapshot, ds); err != nil {
			_ = z.DestroyDisk(context.WithoutCancel(ctx), vm)
			return "", fmt.Errorf("copy template to %s: %w", poolOf(ds), err)
		}
	} else if _, err := z.Run.Run(ctx, "zfs", "clone", "-p", snapshot, ds); err != nil {
		return "", fmt.Errorf("clone disk: %w", err)
	}
	if err := z.ResizeDisk(ctx, vm, sizeGB); err != nil {
		_ = z.DestroyDisk(ctx, vm)
		return "", fmt.Errorf("resize disk: %w", err)
	}
	dev, err := z.WaitDevice(ctx, vm)
	if err != nil {
		_ = z.DestroyDisk(context.WithoutCancel(ctx), vm)
		return "", err
	}
	return dev, nil
}

// CreateDisk makes an empty disk for a VM installed from an ISO.
func (z *ZFS) CreateDisk(ctx context.Context, vm string, sizeGB int) (string, error) {
	if z.Shared(vm) {
		return z.cephCreate(ctx, vm, 0, int64(sizeGB)<<30)
	}
	if _, err := z.Run.Run(ctx, "zfs", "create", "-p", "-s", "-V", fmt.Sprintf("%dG", sizeGB), "-o", "volmode=dev", z.diskDataset(vm)); err != nil {
		return "", fmt.Errorf("create disk: %w", err)
	}
	dev, err := z.WaitDevice(ctx, vm)
	if err != nil {
		_ = z.DestroyDisk(context.WithoutCancel(ctx), vm)
		return "", err
	}
	return dev, nil
}

// WaitDevice waits for udev to create /dev/zvol/... for the VM's disk, so
// libvirt can open it, and returns that path.
func (z *ZFS) WaitDevice(ctx context.Context, vm string) (string, error) {
	if z.Shared(vm) {
		return z.Ceph.Map(ctx, z.Ceph.image(vm, 0))
	}
	return z.waitPath(ctx, z.DevicePath(vm))
}

// DestroyDisk removes the VM's disk and its snapshots. Missing disks are not an error.
// Right after a VM is powered off, QEMU or udev can still hold the device open for
// a moment ("dataset is busy"), so that case is retried for a few seconds.
func (z *ZFS) DestroyDisk(ctx context.Context, vm string) error {
	if z.Shared(vm) {
		return z.cephDestroy(ctx, vm)
	}
	for _, i := range z.Extras(ctx, vm) { // data disks go with the VM
		if err := z.DestroyExtra(ctx, vm, i); err != nil {
			return err
		}
	}
	if err := z.destroy(ctx, z.diskDataset(vm)); err != nil {
		return err
	}
	z.forget(vm)
	return nil
}

func errStderr(err error) string {
	var ee *execx.Error
	if errors.As(err, &ee) {
		return ee.Stderr
	}
	return ""
}

func asExec(err error, ee **execx.Error) bool { return errors.As(err, ee) }

// SnapshotExists checks that a template snapshot is present.
func (z *ZFS) SnapshotExists(ctx context.Context, snapshot string) bool {
	_, err := z.Run.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", snapshot)
	return err == nil
}

// ResizeDisk sets the disk's size. Callers must only grow disks.
func (z *ZFS) ResizeDisk(ctx context.Context, vm string, sizeGB int) error {
	if z.Shared(vm) {
		return z.Ceph.Grow(ctx, z.Ceph.image(vm, 0), sizeGB)
	}
	if _, err := z.Run.Run(ctx, "zfs", "set", fmt.Sprintf("volsize=%dG", sizeGB), z.diskDataset(vm)); err != nil {
		return fmt.Errorf("resize disk: %w", err)
	}
	return nil
}

// DiskSizeGB reports the disk's current size (it changes on rollback).
func (z *ZFS) DiskSizeGB(ctx context.Context, vm string) (int, error) {
	if z.Shared(vm) {
		return z.cephSizeGB(ctx, z.Ceph.image(vm, 0))
	}
	return z.volsizeGB(ctx, z.diskDataset(vm))
}

// Snapshot is a point-in-time copy of a VM's disk.
type Snapshot struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"` // RFC 3339
	UsedBytes int64  `json:"used_bytes"` // space only this snapshot holds
	// RAM: the VM's memory was saved with it (rollback resumes the VM where
	// it was); RAMBytes is the size of that saved state. Filled by package vm.
	RAM      bool  `json:"ram,omitempty"`
	RAMBytes int64 `json:"ram_bytes,omitempty"`
}

// Snapshot takes a ZFS snapshot of the VM's disks (instant, copy-on-write).
// All disks are in one command, so ZFS takes them at the same moment.
func (z *ZFS) Snapshot(ctx context.Context, vm, name string) error {
	if z.Shared(vm) { // one image after the other (Ceph has no single command for several)
		for _, img := range z.cephImages(ctx, vm) {
			if _, err := z.Ceph.rbd(ctx, "snap", "create", z.Ceph.spec(img)+"@"+name); err != nil {
				return fmt.Errorf("snapshot %s: %w", img, err)
			}
		}
		return nil
	}
	args := []string{"snapshot"}
	for _, ds := range z.all(ctx, vm) {
		args = append(args, ds+"@"+name)
	}
	if _, err := z.Run.Run(ctx, "zfs", args...); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}

// Snapshots lists the VM disk's snapshots, oldest first.
func (z *ZFS) Snapshots(ctx context.Context, vm string) ([]Snapshot, error) {
	if z.Shared(vm) {
		return z.cephSnapshots(ctx, vm)
	}
	out, err := z.Run.Run(ctx, "zfs", "list", "-H", "-p", "-t", "snapshot", "-o", "name,creation,used",
		"-s", "createtxg", "-d", "1", z.diskDataset(vm))
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	all, l := ParseSnapshots(string(out)), []Snapshot{}
	for _, sn := range all {
		if !hidden(sn.Name) {
			l = append(l, sn)
		}
	}
	return l, nil
}

// ParseSnapshots reads `zfs list -H -p -o name,creation,used` output.
func ParseSnapshots(out string) []Snapshot {
	snaps := []Snapshot{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 3 {
			continue
		}
		_, name, ok := strings.Cut(f[0], "@")
		if !ok {
			continue
		}
		created, _ := strconv.ParseInt(f[1], 10, 64)
		used, _ := strconv.ParseInt(f[2], 10, 64)
		snaps = append(snaps, Snapshot{Name: name, CreatedAt: time.Unix(created, 0).UTC().Format(time.RFC3339), UsedBytes: used})
	}
	return snaps
}

// Rollback returns the disk to a snapshot. destroyNewer also deletes snapshots
// taken after it (ZFS can only roll back to the latest one).
func (z *ZFS) Rollback(ctx context.Context, vm, name string, destroyNewer bool) error {
	if z.Shared(vm) {
		return z.cephRollback(ctx, vm, name, destroyNewer)
	}
	z.dropRepSnaps(ctx, vm) // ours are newer; the next copy for HA is then a full one
	z.dropBackupSnaps(ctx, vm)
	args := []string{"rollback"}
	if destroyNewer {
		args = append(args, "-r")
	}
	if _, err := z.Run.Run(ctx, "zfs", append(args, z.diskDataset(vm)+"@"+name)...); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	// data disks: those that existed at the snapshot have it too (one added
	// later has not, and keeps its data)
	for _, i := range z.Extras(ctx, vm) {
		ds := z.extraDataset(vm, i)
		if _, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "snapshot", ds+"@"+name); err != nil {
			continue
		}
		if _, err := z.Run.Run(ctx, "zfs", append(args, ds+"@"+name)...); err != nil {
			return fmt.Errorf("rollback disk %d: %w", i, err)
		}
	}
	return nil
}

// DestroySnapshot deletes one snapshot.
func (z *ZFS) DestroySnapshot(ctx context.Context, vm, name string) error {
	if z.Shared(vm) {
		return z.cephDestroySnap(ctx, vm, name)
	}
	if _, err := z.Run.Run(ctx, "zfs", "destroy", z.diskDataset(vm)+"@"+name); err != nil {
		return fmt.Errorf("delete snapshot: %w", err)
	}
	for _, i := range z.Extras(ctx, vm) {
		if _, err := z.Run.Run(ctx, "zfs", "destroy", z.extraDataset(vm, i)+"@"+name); err != nil &&
			!strings.Contains(errStderr(err), "does not exist") && !strings.Contains(errStderr(err), "could not find") {
			return fmt.Errorf("delete snapshot of disk %d: %w", i, err)
		}
	}
	return nil
}

// HasSnapshot says whether every disk of the VM has snapshot name.
func (z *ZFS) HasSnapshot(ctx context.Context, vm, name string) bool {
	if z.Shared(vm) {
		for _, img := range z.cephImages(ctx, vm) {
			if !z.Ceph.HasImageSnap(ctx, img, name) {
				return false
			}
		}
		return true
	}
	for _, ds := range z.all(ctx, vm) {
		if _, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "snapshot", ds+"@"+name); err != nil {
			return false
		}
	}
	return true
}

// dropBackupSnaps removes the kept backup snapshots (p8s-backup-*) of the
// VM's disks; the next backup is then a full one.
func (z *ZFS) dropBackupSnaps(ctx context.Context, vm string) {
	if z.Shared(vm) {
		return
	}
	for _, ds := range z.all(ctx, vm) {
		out, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "snapshot", "-d", "1", ds)
		if err != nil {
			continue
		}
		for _, n := range strings.Fields(string(out)) {
			if _, sn, ok := strings.Cut(n, "@"); ok && strings.HasPrefix(sn, "p8s-backup-") {
				_, _ = z.Run.Run(ctx, "zfs", "destroy", n)
			}
		}
	}
}
