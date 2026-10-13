package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Replicas for high availability: a VM's disk is copied to another server
// every few minutes (zfs send, incremental after the first time). The copy
// lives under <Dataset>/replica/ and has no /dev device (volmode=none), so
// the host never looks inside it. Our snapshots are called p8s-rep-<time>;
// they are hidden from the snapshot list.

const RepPrefix = "p8s-rep-"

func (z *ZFS) replicaDataset(vm string) string { return z.Dataset + "/replica/" + vm + "-disk0" }

// hidden snapshots are ours (moves, replicas), not the person's.
func hidden(name string) bool { return strings.HasPrefix(name, "p8s-") }

// repSnaps lists our replica snapshots of a dataset, oldest first.
func (z *ZFS) repSnaps(ctx context.Context, ds string) ([]string, error) {
	out, err := z.Run.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-s", "createtxg", "-d", "1", ds)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	var l []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, s, ok := strings.Cut(strings.TrimSpace(line), "@"); ok && strings.HasPrefix(s, RepPrefix) {
			l = append(l, s)
		}
	}
	return l, nil
}

// ReplicaSnaps are the replica snapshots this server holds of a VM (oldest first).
func (z *ZFS) ReplicaSnaps(ctx context.Context, vm string) ([]string, error) {
	return z.repSnaps(ctx, z.replicaDataset(vm))
}

// HasReplica says whether a copy of the VM's disk is here.
func (z *ZFS) HasReplica(ctx context.Context, vm string) bool {
	_, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", z.replicaDataset(vm))
	return err == nil
}

// ReplicaBytes is the space the copy takes.
func (z *ZFS) ReplicaBytes(ctx context.Context, vm string) int64 {
	out, err := z.Run.Run(ctx, "zfs", "get", "-H", "-p", "-o", "value", "used", z.replicaDataset(vm))
	if err != nil {
		return 0
	}
	var n int64
	fmt.Sscan(strings.TrimSpace(string(out)), &n)
	return n
}

// SendReplica snapshots the VM's disk (it may be running: the copy is like
// after a power cut) and writes the change since base to w (everything when
// base is ""). Our older snapshots, except base, are removed first. It
// returns the new snapshot's name through name() before the data is sent.
func (z *ZFS) SendReplica(ctx context.Context, vm, base string, name func(snap string) error, w io.Writer) error {
	ds := z.diskDataset(vm)
	old, err := z.repSnaps(ctx, ds)
	if err != nil {
		return err
	}
	for _, s := range old {
		if s != base {
			_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+s)
		}
	}
	snap := fmt.Sprintf("%s%d", RepPrefix, time.Now().UnixNano())
	if _, err := z.Run.Run(ctx, "zfs", "snapshot", ds+"@"+snap); err != nil {
		return fmt.Errorf("snapshot for the copy: %w", err)
	}
	if err := name(snap); err != nil {
		return err
	}
	args := []string{"send", "-c"}
	if base != "" {
		args = append(args, "-i", "@"+base)
	}
	cmd := StreamCmd(ctx, "zfs", append(args, ds+"@"+snap)...)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("zfs send: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RecvReplica stores a SendReplica stream. With base "" the old copy (if any)
// is replaced; otherwise the copy goes back to base first (anything newer,
// e.g. from before a failover, is dropped) and the change is applied.
// Afterwards only the newest snapshot is kept.
func (z *ZFS) RecvReplica(ctx context.Context, vm, base string, r io.Reader) error {
	ds := z.replicaDataset(vm)
	if base == "" {
		if err := z.DropReplica(ctx, vm); err != nil {
			return err
		}
		if _, err := z.Run.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", z.Dataset+"/replica"); err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("replica dataset: %w", err)
		}
	} else {
		if _, err := z.Run.Run(ctx, "zfs", "rollback", "-r", ds+"@"+base); err != nil {
			return fmt.Errorf("copy has no snapshot %s: %w", base, err)
		}
	}
	args := []string{"receive", "-u", "-o", "volmode=none"}
	if base != "" {
		args = append(args, "-F")
	}
	cmd := StreamCmd(ctx, "zfs", append(args, ds)...)
	var stderr strings.Builder
	cmd.Stdin, cmd.Stderr = r, &stderr
	if err := cmd.Run(); err != nil {
		if base == "" {
			_ = z.DropReplica(context.WithoutCancel(ctx), vm)
		}
		return fmt.Errorf("zfs receive: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	snaps, _ := z.repSnaps(ctx, ds)
	for i := 0; i+1 < len(snaps); i++ {
		_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+snaps[i])
	}
	return nil
}

// DropReplica deletes the copy (missing is fine).
func (z *ZFS) DropReplica(ctx context.Context, vm string) error {
	_, err := z.Run.Run(ctx, "zfs", "destroy", "-r", z.replicaDataset(vm))
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		return nil
	}
	return err
}

// PromoteReplica turns the copy into the VM's disk (failover) and returns its device.
func (z *ZFS) PromoteReplica(ctx context.Context, vm string) (string, error) {
	if _, err := z.Run.Run(ctx, "zfs", "rename", z.replicaDataset(vm), z.diskDataset(vm)); err != nil {
		return "", fmt.Errorf("use the copy: %w", err)
	}
	if _, err := z.Run.Run(ctx, "zfs", "set", "volmode=dev", z.diskDataset(vm)); err != nil {
		return "", err
	}
	return z.WaitDevice(ctx, vm)
}

// RetireDisk turns a VM's disk back into a copy (the VM runs elsewhere now);
// the next replication brings it up to date from the other server.
func (z *ZFS) RetireDisk(ctx context.Context, vm string) error {
	if err := z.DropReplica(ctx, vm); err != nil {
		return err
	}
	if _, err := z.Run.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", z.Dataset+"/replica"); err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	if _, err := z.Run.Run(ctx, "zfs", "set", "volmode=none", z.diskDataset(vm)); err != nil {
		return err
	}
	_, _ = z.Run.Run(ctx, "udevadm", "settle", "--timeout=5")
	if _, err := z.Run.Run(ctx, "zfs", "rename", z.diskDataset(vm), z.replicaDataset(vm)); err != nil {
		return fmt.Errorf("keep the old disk as the copy: %w", err)
	}
	return nil
}

// dropRepSnaps removes our replica snapshots of the VM's disk (before a
// rollback: ZFS can only go back past them by deleting them).
func (z *ZFS) dropRepSnaps(ctx context.Context, vm string) {
	snaps, _ := z.repSnaps(ctx, z.diskDataset(vm))
	for _, s := range snaps {
		_, _ = z.Run.Run(ctx, "zfs", "destroy", z.diskDataset(vm)+"@"+s)
	}
}

// DiskRepSnaps are our replica snapshots of the VM's own disk (oldest first).
func (z *ZFS) DiskRepSnaps(ctx context.Context, vm string) ([]string, error) {
	return z.repSnaps(ctx, z.diskDataset(vm))
}
