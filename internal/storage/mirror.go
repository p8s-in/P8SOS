package storage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// P8S Mirror, the server that keeps the mirror: the VM's disks arrive here
// live, write by write, from the VM's QEMU over the cluster tunnel (NBD).
// The copy lives where high-availability copies live (<dataset>/replica/
// <vm>-diskN), so failover and coming back work like for copies. While a
// full sync runs the copy is half old, half new: a "last good" snapshot taken
// before the sync is what failover rolls back to in that case.

// MirrorGoodSnap marks the copy's state before a sync started.
const MirrorGoodSnap = "p8s-mirror-good"

// MirrorDisk is one disk of a mirrored VM (its exact size).
type MirrorDisk struct {
	Index int   `json:"index"`
	Bytes int64 `json:"bytes"`
}

// MirrorUnit is the systemd unit that serves a VM's mirror volumes.
func MirrorUnit(vm string) string { return "p8s-mirror-" + vm }

// PrepareMirror makes (or keeps) the copy's volumes for disks, snapshots the
// ones that exist (their last good state), shows them as block devices and
// returns their paths (in the order of disks).
func (z *ZFS) PrepareMirror(ctx context.Context, vm string, disks []MirrorDisk) ([]string, error) {
	if _, err := z.Run.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", z.Dataset+"/replica"); err != nil && !strings.Contains(err.Error(), "already exists") {
		return nil, fmt.Errorf("replica dataset: %w", err)
	}
	want := map[int]bool{}
	for _, d := range disks {
		want[d.Index] = true
	}
	for _, i := range z.replicaDisks(ctx, vm) { // a data disk the VM doesn't have any more
		if !want[i] {
			_, _ = z.Run.Run(ctx, "zfs", "destroy", "-r", z.replicaDisk(vm, i))
		}
	}
	var paths []string
	for _, d := range disks {
		ds := z.replicaDisk(vm, d.Index)
		out, err := z.Run.Run(ctx, "zfs", "get", "-H", "-p", "-o", "value", "volsize", ds)
		if err == nil {
			have, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if have > d.Bytes {
				// the disk can't have shrunk: start over with this one
				if _, err := z.Run.Run(ctx, "zfs", "destroy", "-r", ds); err != nil {
					return nil, err
				}
				err = fmt.Errorf("recreate")
			} else {
				_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+MirrorGoodSnap)
				if _, err := z.Run.Run(ctx, "zfs", "snapshot", ds+"@"+MirrorGoodSnap); err != nil {
					return nil, fmt.Errorf("snapshot of the copy: %w", err)
				}
				if have < d.Bytes {
					if _, err := z.Run.Run(ctx, "zfs", "set", fmt.Sprintf("volsize=%d", d.Bytes), ds); err != nil {
						return nil, fmt.Errorf("grow the copy of disk %d: %w", d.Index, err)
					}
				}
			}
		}
		if err != nil { // a new copy: sparse, so it takes only what is written
			if _, err := z.Run.Run(ctx, "zfs", "create", "-s", "-V", strconv.FormatInt(d.Bytes, 10), "-o", "volmode=dev", ds); err != nil {
				return nil, fmt.Errorf("make the copy of disk %d: %w", d.Index, err)
			}
		}
		if _, err := z.Run.Run(ctx, "zfs", "set", "volmode=dev", ds); err != nil {
			return nil, err
		}
		p, err := z.waitPath(ctx, "/dev/zvol/"+ds)
		if err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// MirrorSynced drops the "last good" snapshots: the copy is up to date now.
func (z *ZFS) MirrorSynced(ctx context.Context, vm string) {
	for _, i := range z.replicaDisks(ctx, vm) {
		_, _ = z.Run.Run(ctx, "zfs", "destroy", z.replicaDisk(vm, i)+"@"+MirrorGoodSnap)
	}
}

// MirrorRollback puts the copy back to its last good state (a sync was cut
// short). It reports whether it did.
func (z *ZFS) MirrorRollback(ctx context.Context, vm string) (bool, error) {
	rolled := false
	for _, i := range z.replicaDisks(ctx, vm) {
		ds := z.replicaDisk(vm, i)
		if !z.hasSnap(ctx, ds, MirrorGoodSnap) {
			continue
		}
		if _, err := z.Run.Run(ctx, "zfs", "rollback", "-r", ds+"@"+MirrorGoodSnap); err != nil {
			return rolled, fmt.Errorf("go back to the last good copy of disk %d: %w", i, err)
		}
		_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+MirrorGoodSnap)
		rolled = true
	}
	return rolled, nil
}

// MirrorExportArgs is the qemu-storage-daemon command line that serves the
// copy's volumes (paths, in disk order) on host:port, one NBD export per
// disk ("disk<index>").
func MirrorExportArgs(host string, port int, disks []MirrorDisk, paths []string) []string {
	var a []string
	for k, d := range disks {
		a = append(a, "--blockdev", fmt.Sprintf("driver=host_device,node-name=d%d,filename=%s,cache.direct=on", d.Index, paths[k]))
	}
	a = append(a, "--nbd-server", fmt.Sprintf("addr.type=inet,addr.host=%s,addr.port=%d,max-connections=%d", host, port, 2*len(disks)))
	for _, d := range disks {
		a = append(a, "--export", fmt.Sprintf("type=nbd,id=e%d,node-name=d%d,name=disk%d,writable=on", d.Index, d.Index, d.Index))
	}
	return a
}

// StartMirrorExport (re)starts the service that serves the copy's volumes.
func (z *ZFS) StartMirrorExport(ctx context.Context, vm, host string, port int, disks []MirrorDisk, paths []string) error {
	z.StopMirrorExport(ctx, vm)
	args := append([]string{"--unit=" + MirrorUnit(vm), "--collect", "--quiet", "--property=Restart=on-failure",
		"--description=P8S Mirror of " + vm, "qemu-storage-daemon"}, MirrorExportArgs(host, port, disks, paths)...)
	if _, err := z.Run.Run(ctx, "systemd-run", args...); err != nil {
		return fmt.Errorf("serve the mirror: %w", err)
	}
	return nil
}

// StopMirrorExport stops serving the copy (before it is used or deleted).
func (z *ZFS) StopMirrorExport(ctx context.Context, vm string) {
	_, _ = z.Run.Run(ctx, "systemctl", "stop", MirrorUnit(vm))
	_, _ = z.Run.Run(ctx, "systemctl", "reset-failed", MirrorUnit(vm))
}

// MirrorExportUp says whether the copy is being served.
func (z *ZFS) MirrorExportUp(ctx context.Context, vm string) bool {
	_, err := z.Run.Run(ctx, "systemctl", "is-active", "--quiet", MirrorUnit(vm))
	return err == nil
}

// HideMirror makes the copy's volumes invisible to the host again (volmode=none).
func (z *ZFS) HideMirror(ctx context.Context, vm string) {
	for _, i := range z.replicaDisks(ctx, vm) {
		_, _ = z.Run.Run(ctx, "zfs", "set", "volmode=none", z.replicaDisk(vm, i))
	}
}
