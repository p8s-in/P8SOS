package storage

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"p8sos/internal/execx"
)

// Data disks next to a VM's root disk are zvols <vm>-disk1 ... <vm>-disk8
// beside <vm>-disk0. Snapshots, rollbacks and deletes cover all of them.

func (z *ZFS) extraDataset(vm string, i int) string {
	return z.DiskDataset(vm, i)
}

// ExtraPath is the block device of data disk i.
func (z *ZFS) ExtraPath(vm string, i int) string {
	if z.Shared(vm) {
		return z.Ceph.Dev(z.Ceph.image(vm, i))
	}
	return "/dev/zvol/" + z.extraDataset(vm, i)
}

// Extras lists the indexes of the VM's data disks that exist.
func (z *ZFS) Extras(ctx context.Context, vm string) []int {
	if z.Shared(vm) {
		return z.cephExtras(ctx, vm)
	}
	p := z.parent(vm)
	out, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "volume", "-d", "1", p)
	if err != nil {
		return nil
	}
	return parseExtras(string(out), p+"/"+vm+"-disk")
}

func parseExtras(out, prefix string) []int {
	var l []int
	for _, name := range strings.Fields(out) {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(rest); err == nil && n >= 1 && n <= 8 && strconv.Itoa(n) == rest {
			l = append(l, n)
		}
	}
	sort.Ints(l)
	return l
}

// all is every dataset of the VM's disks (root first).
func (z *ZFS) all(ctx context.Context, vm string) []string {
	l := []string{z.diskDataset(vm)}
	for _, i := range z.Extras(ctx, vm) {
		l = append(l, z.extraDataset(vm, i))
	}
	return l
}

// CreateExtra makes empty data disk i and returns its device.
func (z *ZFS) CreateExtra(ctx context.Context, vm string, i, sizeGB int) (string, error) {
	if z.Shared(vm) {
		return z.cephCreate(ctx, vm, i, int64(sizeGB)<<30)
	}
	ds := z.extraDataset(vm, i)
	if _, err := z.Run.Run(ctx, "zfs", "create", "-p", "-s", "-V", fmt.Sprintf("%dG", sizeGB), "-o", "volmode=dev", ds); err != nil {
		return "", fmt.Errorf("create disk %d: %w", i, err)
	}
	dev, err := z.waitPath(ctx, z.ExtraPath(vm, i))
	if err != nil {
		_ = z.DestroyExtra(context.WithoutCancel(ctx), vm, i)
		return "", err
	}
	return dev, nil
}

// DestroyExtra removes data disk i (missing = fine), retrying while it is busy.
func (z *ZFS) DestroyExtra(ctx context.Context, vm string, i int) error {
	if z.Shared(vm) {
		return z.Ceph.Remove(ctx, z.Ceph.image(vm, i))
	}
	return z.destroy(ctx, z.extraDataset(vm, i))
}

// ResizeExtra grows data disk i.
func (z *ZFS) ResizeExtra(ctx context.Context, vm string, i, sizeGB int) error {
	if z.Shared(vm) {
		return z.Ceph.Grow(ctx, z.Ceph.image(vm, i), sizeGB)
	}
	if _, err := z.Run.Run(ctx, "zfs", "set", fmt.Sprintf("volsize=%dG", sizeGB), z.extraDataset(vm, i)); err != nil {
		return fmt.Errorf("resize disk %d: %w", i, err)
	}
	return nil
}

// ExtraSizeGB is data disk i's size.
func (z *ZFS) ExtraSizeGB(ctx context.Context, vm string, i int) (int, error) {
	if z.Shared(vm) {
		return z.cephSizeGB(ctx, z.Ceph.image(vm, i))
	}
	return z.volsizeGB(ctx, z.extraDataset(vm, i))
}

func (z *ZFS) volsizeGB(ctx context.Context, ds string) (int, error) {
	out, err := z.Run.Run(ctx, "zfs", "get", "-H", "-p", "-o", "value", "volsize", ds)
	if err != nil {
		return 0, err
	}
	b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("volsize %q: %w", out, err)
	}
	return int(b >> 30), nil
}

func (z *ZFS) waitPath(ctx context.Context, dev string) (string, error) {
	limit := z.Wait
	if limit == 0 {
		limit = 30 * time.Second
	}
	for deadline := time.Now().Add(limit); !z.exists(dev); {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("disk device %s did not appear (udev)", dev)
		}
		_, _ = z.Run.Run(ctx, "udevadm", "trigger", "--subsystem-match=block", "--action=add")
		_, _ = z.Run.Run(ctx, "udevadm", "settle", "--timeout=5")
		time.Sleep(300 * time.Millisecond)
	}
	return dev, nil
}

// destroy removes a dataset and its snapshots; missing is fine. Right after a
// VM powers off, QEMU or udev can hold the device for a moment ("busy").
func (z *ZFS) destroy(ctx context.Context, ds string) error {
	pause := z.BusyRetry
	if pause == 0 {
		pause = time.Second
	}
	for attempt := 1; ; attempt++ {
		_, err := z.Run.Run(ctx, "zfs", "destroy", "-r", ds)
		var ee *execx.Error
		if !asExec(err, &ee) {
			return err
		}
		if strings.Contains(ee.Stderr, "does not exist") {
			return nil
		}
		if !strings.Contains(ee.Stderr, "busy") || attempt == 15 || ctx.Err() != nil {
			return err
		}
		_, _ = z.Run.Run(ctx, "udevadm", "settle", "--timeout=5")
		time.Sleep(pause)
	}
}

// WaitExtra waits for data disk i's device (after a restore) and returns it.
func (z *ZFS) WaitExtra(ctx context.Context, vm string, i int) (string, error) {
	if z.Shared(vm) {
		return z.Ceph.Map(ctx, z.Ceph.image(vm, i))
	}
	return z.waitPath(ctx, z.ExtraPath(vm, i))
}

// DiskBytes is the exact size of the VM's disk i (0 = root), for a live move:
// the copy on the other server must be just as big.
func (z *ZFS) DiskBytes(ctx context.Context, vm string, i int) (int64, error) {
	if z.Shared(vm) {
		return z.Ceph.Size(ctx, z.Ceph.image(vm, i))
	}
	out, err := z.Run.Run(ctx, "zfs", "get", "-H", "-p", "-o", "value", "volsize", z.DiskDataset(vm, i))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// CreateSized makes an empty disk i (0 = root) of exactly bytes and returns its device.
func (z *ZFS) CreateSized(ctx context.Context, vm string, i int, bytes int64) (string, error) {
	if z.Shared(vm) {
		return z.cephCreate(ctx, vm, i, bytes)
	}
	ds := z.DiskDataset(vm, i)
	if _, err := z.Run.Run(ctx, "zfs", "create", "-p", "-s", "-V", strconv.FormatInt(bytes, 10), "-o", "volmode=dev", ds); err != nil {
		return "", fmt.Errorf("create disk %d: %w", i, err)
	}
	dev, err := z.waitPath(ctx, "/dev/zvol/"+ds)
	if err != nil {
		_ = z.destroy(context.WithoutCancel(ctx), ds)
		return "", err
	}
	return dev, nil
}
