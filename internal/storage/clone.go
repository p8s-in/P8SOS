package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	return z.MakeTemplateDisk(ctx, vm, 0, snap, ds)
}

// MakeTemplateDisk copies the VM's disk i (0 = root, else a data disk) at
// snapshot snap to the local dataset ds; templates always live on local ZFS,
// also when the VM is on Ceph (its disk image is exported into a zvol).
func (z *ZFS) MakeTemplateDisk(ctx context.Context, vm string, i int, snap, ds string) (string, error) {
	if _, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", ds); err == nil {
		return "", fmt.Errorf("%s already exists", ds)
	}
	if k := strings.LastIndex(ds, "/"); k > 0 {
		_, _ = z.Run.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", ds[:k])
	}
	if z.Shared(vm) {
		if err := z.cephToZvol(ctx, z.Ceph.image(vm, i), snap, ds); err != nil {
			_ = z.destroy(context.WithoutCancel(ctx), ds)
			return "", err
		}
		return ds + "@" + snap, nil
	}
	if err := z.sendRecv(ctx, z.DiskDataset(vm, i)+"@"+snap, ds, true); err != nil {
		_ = z.destroy(context.WithoutCancel(ctx), ds)
		return "", err
	}
	return ds + "@" + snap, nil
}

// cephToZvol copies a Ceph disk image at a snapshot into a new local zvol
// (sparse: only what was written) and snapshots it with the same name.
func (z *ZFS) cephToZvol(ctx context.Context, img, snap, ds string) error {
	r := z.Ceph
	size, err := r.Size(ctx, img)
	if err != nil {
		return err
	}
	if _, err := z.Run.Run(ctx, "zfs", "create", "-s", "-V", fmt.Sprint(size), "-o", "volmode=dev", ds); err != nil {
		return fmt.Errorf("template disk: %w", err)
	}
	dev, err := z.waitPath(ctx, "/dev/zvol/"+ds)
	if err != nil {
		return err
	}
	export := StreamCmd(ctx, "rbd", "export", "--no-progress", r.spec(img)+"@"+snap, "-")
	write := StreamCmd(ctx, "dd", "of="+dev, "bs=4M", "conv=sparse,fsync", "status=none")
	var es, ew strings.Builder
	export.Stderr, write.Stderr = &es, &ew
	pr, pw := io.Pipe()
	export.Stdout, write.Stdin = pw, pr
	if err := write.Start(); err != nil {
		return err
	}
	errE := export.Run()
	pw.CloseWithError(errE)
	errW := write.Wait()
	if errE != nil {
		return fmt.Errorf("rbd export: %v %s", errE, strings.TrimSpace(es.String()))
	}
	if errW != nil {
		return fmt.Errorf("writing the template: %v %s", errW, strings.TrimSpace(ew.String()))
	}
	_, err = z.Run.Run(ctx, "zfs", "snapshot", ds+"@"+snap)
	return err
}

// DestroyTemplate deletes a template made from a VM (refused while VMs use
// it), with its data disks (ds-disk1 …).
func (z *ZFS) DestroyTemplate(ctx context.Context, ds string) error {
	if k := strings.LastIndex(ds, "/"); k > 0 {
		if out, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "volume", "-d", "1", ds[:k]); err == nil {
			for _, n := range strings.Fields(string(out)) {
				if strings.HasPrefix(n, ds+"-disk") {
					if _, err := z.Run.Run(ctx, "zfs", "destroy", "-r", n); err != nil && strings.Contains(errStderr(err), "dependent clones") {
						return fmt.Errorf("template %s: %w", ds, ErrTemplateInUse)
					}
				}
			}
		}
	}
	_, err := z.Run.Run(ctx, "zfs", "destroy", "-r", ds)
	if err != nil && strings.Contains(errStderr(err), "dependent clones") {
		return fmt.Errorf("template %s: %w", ds, ErrTemplateInUse)
	}
	if err != nil && strings.Contains(errStderr(err), "does not exist") {
		return nil
	}
	return err
}
