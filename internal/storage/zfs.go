// Package storage manages VM disks as ZFS zvols cloned from template snapshots.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"p8sos/internal/execx"
)

type ZFS struct {
	Run     execx.Runner
	Dataset string            // parent dataset for VM disks, e.g. tank/p8s/vms
	Exists  func(string) bool // checks a device path; nil = os.Stat (tests fake it)
	Wait    time.Duration     // max wait for /dev/zvol/...; 0 = 30s
}

func (z *ZFS) exists(p string) bool {
	if z.Exists != nil {
		return z.Exists(p)
	}
	_, err := os.Stat(p)
	return err == nil
}

func (z *ZFS) diskDataset(vm string) string { return z.Dataset + "/" + vm + "-disk0" }

// DevicePath is the block device libvirt attaches for the VM's root disk.
func (z *ZFS) DevicePath(vm string) string { return "/dev/zvol/" + z.diskDataset(vm) }

// CloneDisk creates the VM's root disk as a copy-on-write clone of the
// template snapshot, then grows it to sizeGB. Clones are instant and use no
// space until the guest writes.
func (z *ZFS) CloneDisk(ctx context.Context, snapshot, vm string, sizeGB int) (string, error) {
	ds := z.diskDataset(vm)
	if _, err := z.Run.Run(ctx, "zfs", "clone", "-p", snapshot, ds); err != nil {
		return "", fmt.Errorf("clone disk: %w", err)
	}
	if _, err := z.Run.Run(ctx, "zfs", "set", fmt.Sprintf("volsize=%dG", sizeGB), ds); err != nil {
		_ = z.DestroyDisk(ctx, vm)
		return "", fmt.Errorf("resize disk: %w", err)
	}
	// Wait for udev to create /dev/zvol/... before libvirt tries to open it.
	dev := z.DevicePath(vm)
	limit := z.Wait
	if limit == 0 {
		limit = 30 * time.Second
	}
	for deadline := time.Now().Add(limit); !z.exists(dev); {
		if time.Now().After(deadline) || ctx.Err() != nil {
			_ = z.DestroyDisk(context.WithoutCancel(ctx), vm)
			return "", fmt.Errorf("disk device %s did not appear (udev)", dev)
		}
		_, _ = z.Run.Run(ctx, "udevadm", "trigger", "--subsystem-match=block", "--action=add")
		_, _ = z.Run.Run(ctx, "udevadm", "settle", "--timeout=5")
		time.Sleep(300 * time.Millisecond)
	}
	return dev, nil
}

// DestroyDisk removes the VM's disk and its snapshots. Missing disks are not an error.
func (z *ZFS) DestroyDisk(ctx context.Context, vm string) error {
	_, err := z.Run.Run(ctx, "zfs", "destroy", "-r", z.diskDataset(vm))
	var ee *execx.Error
	if errors.As(err, &ee) && strings.Contains(ee.Stderr, "does not exist") {
		return nil
	}
	return err
}

// SnapshotExists checks that a template snapshot is present.
func (z *ZFS) SnapshotExists(ctx context.Context, snapshot string) bool {
	_, err := z.Run.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", snapshot)
	return err == nil
}
