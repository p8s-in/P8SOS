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
	return fmt.Sprintf("%s/%s-disk%d", z.parent(vm), vm, i)
}

// DevicePath is the block device libvirt attaches for the VM's root disk.
func (z *ZFS) DevicePath(vm string) string { return "/dev/zvol/" + z.diskDataset(vm) }

// CloneDisk creates the VM's root disk as a copy-on-write clone of the
// template snapshot, then grows it to sizeGB. Clones are instant and use no
// space until the guest writes.
func (z *ZFS) CloneDisk(ctx context.Context, snapshot, vm string, sizeGB int) (string, error) {
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
	return z.waitPath(ctx, z.DevicePath(vm))
}

// DestroyDisk removes the VM's disk and its snapshots. Missing disks are not an error.
// Right after a VM is powered off, QEMU or udev can still hold the device open for
// a moment ("dataset is busy"), so that case is retried for a few seconds.
func (z *ZFS) DestroyDisk(ctx context.Context, vm string) error {
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
	if _, err := z.Run.Run(ctx, "zfs", "set", fmt.Sprintf("volsize=%dG", sizeGB), z.diskDataset(vm)); err != nil {
		return fmt.Errorf("resize disk: %w", err)
	}
	return nil
}

// DiskSizeGB reports the disk's current size (it changes on rollback).
func (z *ZFS) DiskSizeGB(ctx context.Context, vm string) (int, error) {
	return z.volsizeGB(ctx, z.diskDataset(vm))
}

// Snapshot is a point-in-time copy of a VM's disk.
type Snapshot struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"` // RFC 3339
	UsedBytes int64  `json:"used_bytes"` // space only this snapshot holds
}

// Snapshot takes a ZFS snapshot of the VM's disks (instant, copy-on-write).
// All disks are in one command, so ZFS takes them at the same moment.
func (z *ZFS) Snapshot(ctx context.Context, vm, name string) error {
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
	z.dropRepSnaps(ctx, vm) // ours are newer; the next copy for HA is then a full one
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
