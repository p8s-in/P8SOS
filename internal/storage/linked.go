package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Linked clones: a new VM whose disks are ZFS clones of another VM's disks at
// a snapshot (p8s-link-…). Instant and they take no space until they differ;
// the source keeps that snapshot as long as the clone exists, so the source
// can't be deleted (or moved away) while it has linked clones. Unlink makes a
// clone independent (a full copy of its disks).

// LinkSnapPrefix names the snapshots linked clones are made from.
const LinkSnapPrefix = "p8s-link-"

// ErrLinkUnsupported: the storage can't make linked clones.
var ErrLinkUnsupported = errors.New("this storage can't make linked clones; make a full copy")

// LinkDisks makes newVM's disks clones of vm's disks idx at snapshot snap
// (taken by the caller, named LinkSnapPrefix…). newVM is put on vm's storage.
func (z *ZFS) LinkDisks(ctx context.Context, vm, snap, newVM string, idx []int) error {
	if z.Shared(vm) {
		return z.cephLink(ctx, vm, snap, newVM, idx)
	}
	parent := z.parent(vm) // a clone lives in its source's pool
	z.place.mu.Lock()
	if z.place.where == nil {
		z.place.where = map[string]string{}
	}
	z.place.where[newVM] = parent
	z.place.mu.Unlock()
	var made []string
	undo := func() {
		bg := context.WithoutCancel(ctx)
		for _, ds := range made {
			_ = z.destroy(bg, ds)
		}
		z.forget(newVM)
	}
	for _, i := range idx {
		src, dst := z.DiskDataset(vm, i), z.DiskDataset(newVM, i)
		if z.exists("/dev/zvol/" + dst) {
			undo()
			return fmt.Errorf("a disk %s already exists", dst)
		}
		if _, err := z.Run.Run(ctx, "zfs", "clone", "-o", "p8s:linked="+vm, src+"@"+snap, dst); err != nil {
			undo()
			return fmt.Errorf("link disk %d: %w", i, err)
		}
		made = append(made, dst)
		if _, err := z.waitPath(ctx, "/dev/zvol/"+dst); err != nil {
			undo()
			return err
		}
	}
	return nil
}

// vmOfDataset turns ".../p8s-web2-disk0" (or -disk1…) into "p8s-web2".
func vmOfDataset(ds string) string {
	base := ds[strings.LastIndex(ds, "/")+1:]
	if i := strings.LastIndex(base, "-disk"); i > 0 {
		return base[:i]
	}
	return base
}

// LinkedClones lists the VMs (domain names) whose disks are linked clones of vm's.
func (z *ZFS) LinkedClones(ctx context.Context, vm string) []string {
	if z.Shared(vm) {
		return z.cephLinkedClones(ctx, vm)
	}
	seen := map[string]bool{}
	for _, ds := range z.all(ctx, vm) {
		out, err := z.Run.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name,clones", "-d", "1", ds)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 || f[1] == "-" || !strings.Contains(f[0], "@"+LinkSnapPrefix) {
				continue
			}
			for _, c := range strings.Split(f[1], ",") {
				if c = strings.TrimSpace(c); c != "" {
					seen[vmOfDataset(c)] = true
				}
			}
		}
	}
	var l []string
	for n := range seen {
		if n != vm {
			l = append(l, n)
		}
	}
	sort.Strings(l)
	return l
}

// LinkedTo is the VM (domain name) vm's root disk is a linked clone of ("" = independent).
func (z *ZFS) LinkedTo(ctx context.Context, vm string) string {
	if z.Shared(vm) {
		return z.cephLinkedTo(ctx, vm)
	}
	out, err := z.Run.Run(ctx, "zfs", "get", "-H", "-o", "value", "origin", z.diskDataset(vm))
	o := strings.TrimSpace(string(out))
	if err != nil || o == "" || o == "-" || !strings.Contains(o, "@"+LinkSnapPrefix) {
		return ""
	}
	return vmOfDataset(o[:strings.Index(o, "@")])
}

// linkOrigins are the link snapshots vm's disks were cloned from.
func (z *ZFS) linkOrigins(ctx context.Context, vm string) []string {
	var l []string
	for _, ds := range z.all(ctx, vm) {
		out, err := z.Run.Run(ctx, "zfs", "get", "-H", "-o", "value", "origin", ds)
		if o := strings.TrimSpace(string(out)); err == nil && strings.Contains(o, "@"+LinkSnapPrefix) {
			l = append(l, o)
		}
	}
	return l
}

// dropOrigins deletes link snapshots no clone uses any more.
func (z *ZFS) dropOrigins(ctx context.Context, origins []string) {
	for _, o := range origins {
		_, _ = z.Run.Run(ctx, "zfs", "destroy", o) // refused while another clone uses it: fine
	}
}

// Unlink makes vm's disks independent of the VM they were linked to: each
// is copied in full (zfs send | receive) and put in place of the clone.
// The VM must be off.
func (z *ZFS) Unlink(ctx context.Context, vm string, progress func(string)) error {
	if z.Shared(vm) {
		return z.cephUnlink(ctx, vm, progress)
	}
	origins := z.linkOrigins(ctx, vm)
	if len(origins) == 0 {
		return fmt.Errorf("%s isn't a linked clone", vm)
	}
	const snap = "p8s-unlink"
	for _, ds := range z.all(ctx, vm) {
		out, _ := z.Run.Run(ctx, "zfs", "get", "-H", "-o", "value", "origin", ds)
		if o := strings.TrimSpace(string(out)); !strings.Contains(o, "@"+LinkSnapPrefix) {
			continue // this disk is its own already (e.g. a data disk added later)
		}
		tmp := ds + "-unlink"
		_ = z.destroy(ctx, tmp) // leftover of an interrupted run
		if progress != nil {
			progress("copying " + ds[strings.LastIndex(ds, "/")+1:])
		}
		_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+snap)
		if _, err := z.Run.Run(ctx, "zfs", "snapshot", ds+"@"+snap); err != nil {
			return err
		}
		if err := z.sendRecv(ctx, ds+"@"+snap, tmp, false); err != nil {
			_, _ = z.Run.Run(context.WithoutCancel(ctx), "zfs", "destroy", ds+"@"+snap)
			_ = z.destroy(context.WithoutCancel(ctx), tmp)
			return fmt.Errorf("copy %s: %w", ds, err)
		}
		if err := z.destroy(ctx, ds); err != nil {
			_ = z.destroy(context.WithoutCancel(ctx), tmp)
			return fmt.Errorf("remove the linked disk %s: %w", ds, err)
		}
		if _, err := z.Run.Run(ctx, "zfs", "rename", tmp, ds); err != nil {
			return fmt.Errorf("put the copy in place (it is kept as %s): %w", tmp, err)
		}
		_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+snap)
		_, _ = z.Run.Run(ctx, "zfs", "inherit", "p8s:linked", ds)
		if _, err := z.waitPath(ctx, "/dev/zvol/"+ds); err != nil {
			return err
		}
	}
	z.dropOrigins(ctx, origins)
	return nil
}
