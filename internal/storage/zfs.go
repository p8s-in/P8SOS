// Package storage manages VM disks as ZFS zvols cloned from template snapshots.
package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"p8sos/internal/execx"
)

type ZFS struct {
	Run     execx.Runner
	Dataset string // parent dataset for VM disks, e.g. tank/p8s/vms
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
	_, _ = z.Run.Run(ctx, "udevadm", "settle", "--timeout=10")
	return z.DevicePath(vm), nil
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
