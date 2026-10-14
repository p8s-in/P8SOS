package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Clones and templates made from VMs. A clone is a full, independent copy
// of every disk of a VM at a snapshot (zfs send | receive; on Ceph `rbd cp`),
// on the same storage. A template is a copy of a VM's root disk in the
// templates dataset; new VMs are instant ZFS clones of it, like the built-in ones.

// CloneDisks copies disks idx (0 = root) of vm at snapshot snap to newVM.
// newVM must already be placed (Place) on the same storage as vm.
func (z *ZFS) CloneDisks(ctx context.Context, vm, snap, newVM string, idx []int) error {
	var made []int
	undo := func() {
		bg := context.WithoutCancel(ctx)
		for _, i := range made {
			if z.Shared(newVM) {
				_ = z.Ceph.Remove(bg, z.Ceph.image(newVM, i))
			} else {
				_ = z.destroy(bg, z.DiskDataset(newVM, i))
			}
		}
		z.forget(newVM)
	}
	for _, i := range idx {
		if z.Shared(vm) {
			r := z.Ceph
			if _, err := r.rbd(ctx, "cp", "--no-progress", r.spec(r.image(vm, i))+"@"+snap, r.spec(r.image(newVM, i))); err != nil {
				undo()
				return fmt.Errorf("copy disk %d: %w", i, err)
			}
			made = append(made, i)
			if _, err := r.Map(ctx, r.image(newVM, i)); err != nil {
				undo()
				return err
			}
			continue
		}
		dst := z.DiskDataset(newVM, i)
		if z.exists("/dev/zvol/" + dst) {
			undo()
			return fmt.Errorf("a disk %s already exists", dst)
		}
		if err := z.sendRecv(ctx, z.DiskDataset(vm, i)+"@"+snap, dst, false); err != nil {
			undo()
			return fmt.Errorf("copy disk %d: %w", i, err)
		}
		made = append(made, i)
		if _, err := z.waitPath(ctx, "/dev/zvol/"+dst); err != nil {
			undo()
			return err
		}
	}
	return nil
}

// ErrTemplateInUse means VMs were made from the template (they are ZFS clones of it).
var ErrTemplateInUse = errors.New("VMs made from it still use it")

// MakeTemplate copies the VM's root disk at snapshot snap to dataset ds and
// returns the template snapshot (ds@snap) new VMs are cloned from.
func (z *ZFS) MakeTemplate(ctx context.Context, vm, snap, ds string) (string, error) {
	if z.Shared(vm) {
		return "", fmt.Errorf("templates are made from VMs on local storage (this one is on Ceph)")
	}
	if _, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", ds); err == nil {
		return "", fmt.Errorf("%s already exists", ds)
	}
	if i := strings.LastIndex(ds, "/"); i > 0 {
		_, _ = z.Run.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", ds[:i])
	}
	if err := z.sendRecv(ctx, z.DiskDataset(vm, 0)+"@"+snap, ds, true); err != nil {
		_ = z.destroy(context.WithoutCancel(ctx), ds)
		return "", err
	}
	return ds + "@" + snap, nil
}

// DestroyTemplate deletes a template made from a VM (refused while VMs use it).
func (z *ZFS) DestroyTemplate(ctx context.Context, ds string) error {
	_, err := z.Run.Run(ctx, "zfs", "destroy", "-r", ds)
	if err != nil && strings.Contains(errStderr(err), "dependent clones") {
		return fmt.Errorf("template %s: %w", ds, ErrTemplateInUse)
	}
	if err != nil && strings.Contains(errStderr(err), "does not exist") {
		return nil
	}
	return err
}
