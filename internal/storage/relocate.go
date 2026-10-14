package storage

import (
	"context"
	"fmt"
	"strings"
)

// Relocate copies a (stopped) VM's disks to another storage on this server —
// local ZFS ↔ a network storage pool ↔ Ceph — block by block, then deletes
// the old ones. Disk snapshots stay behind and are deleted with them (they
// can't be carried across storage kinds). idx are the disks (0 = root).
// progress gets "disk N: copying" messages.
func (z *ZFS) Relocate(ctx context.Context, vm, target string, idx []int, progress func(string)) (map[int]string, error) {
	if target == z.StorageOf(vm) {
		return nil, fmt.Errorf("the disks are on %s already", target)
	}
	oldParent, oldShared := z.parent(vm), z.Shared(vm)
	type disk struct {
		i     int
		bytes int64
		dev   string
	}
	var old []disk
	for _, i := range idx {
		n, err := z.DiskBytes(ctx, vm, i)
		if err != nil {
			return nil, fmt.Errorf("size of disk %d: %w", i, err)
		}
		var dev string
		if i == 0 {
			dev, err = z.WaitDevice(ctx, vm)
		} else {
			dev, err = z.WaitExtra(ctx, vm, i)
		}
		if err != nil {
			return nil, fmt.Errorf("disk %d: %w", i, err)
		}
		old = append(old, disk{i, n, dev})
	}
	if err := z.Place(vm, target); err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	back := func() { z.setParent(vm, oldParent) }
	made := map[int]string{}
	undo := func() {
		for i := range made {
			s := z.Shared(vm)
			if s {
				_ = z.Ceph.Remove(bg, z.Ceph.image(vm, i))
			} else {
				_ = z.destroy(bg, z.DiskDataset(vm, i))
			}
		}
		back()
	}
	for _, d := range old {
		dev, err := z.CreateSized(ctx, vm, d.i, d.bytes)
		if err != nil {
			undo()
			return nil, err
		}
		made[d.i] = dev
		progress(fmt.Sprintf("disk %d: copying %s", d.i, human(d.bytes)))
		// only the used blocks are written: the new disk reads as zeros already
		if _, err := z.Run.Run(ctx, "qemu-img", "convert", "-n", "--target-is-zero", "-W", "-f", "raw", "-O", "raw", d.dev, dev); err != nil {
			undo()
			return nil, fmt.Errorf("copying disk %d: %w", d.i, err)
		}
	}
	// the copies are complete: delete the old disks (and their snapshots)
	progress("removing the old disks")
	var left []string
	for _, d := range old {
		if oldShared {
			img := fmt.Sprintf("%s-disk%d", vm, d.i)
			if err := z.Ceph.Remove(bg, img); err != nil {
				left = append(left, z.Ceph.spec(img))
			}
		} else if err := z.destroy(bg, fmt.Sprintf("%s/%s-disk%d", oldParent, vm, d.i)); err != nil {
			left = append(left, fmt.Sprintf("%s/%s-disk%d", oldParent, vm, d.i))
		}
	}
	if len(left) > 0 {
		return made, fmt.Errorf("moved, but the old disks couldn't be deleted (delete them by hand): %s", strings.Join(left, ", "))
	}
	return made, nil
}

func (z *ZFS) setParent(vm, ds string) {
	z.place.mu.Lock()
	defer z.place.mu.Unlock()
	if z.place.where == nil {
		z.place.where = map[string]string{}
	}
	z.place.where[vm] = ds
}

func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%d MB", b>>20)
	}
	return fmt.Sprintf("%d bytes", b)
}
