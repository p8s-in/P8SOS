package storage

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Moving a VM's disk to another server: SendDisk streams a snapshot of the
// disk (zfs send, blocks as stored: compressed and sparse), RecvDisk makes
// the disk from such a stream. Snapshots the person made stay behind.

const moveSnap = "p8s-move"

// StreamCmd builds streaming commands; nil = exec.CommandContext (tests replace it).
var StreamCmd = exec.CommandContext

// SendDisk writes the VM's disk (it must not be running) to w.
func (z *ZFS) SendDisk(ctx context.Context, vm string, w io.Writer) error {
	ds := z.diskDataset(vm)
	snap := ds + "@" + moveSnap
	_, _ = z.Run.Run(ctx, "zfs", "destroy", snap) // leftover of an interrupted move
	if _, err := z.Run.Run(ctx, "zfs", "snapshot", snap); err != nil {
		return fmt.Errorf("snapshot for the move: %w", err)
	}
	defer z.Run.Run(context.WithoutCancel(ctx), "zfs", "destroy", snap)
	cmd := StreamCmd(ctx, "zfs", "send", "-c", snap)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("zfs send: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RecvDisk makes the VM's disk from a SendDisk stream and returns its device.
func (z *ZFS) RecvDisk(ctx context.Context, vm string, r io.Reader) (string, error) {
	ds := z.diskDataset(vm)
	cmd := StreamCmd(ctx, "zfs", "receive", "-o", "volmode=dev", ds)
	var stderr strings.Builder
	cmd.Stdin, cmd.Stderr = r, &stderr
	if err := cmd.Run(); err != nil {
		_ = z.DestroyDisk(context.WithoutCancel(ctx), vm)
		return "", fmt.Errorf("zfs receive: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+moveSnap)
	dev, err := z.WaitDevice(ctx, vm)
	if err != nil {
		_ = z.DestroyDisk(context.WithoutCancel(ctx), vm)
		return "", err
	}
	return dev, nil
}
