package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Copying a template made from a VM to another server: its snapshots (root
// disk first, then the data disks) go in one framed stream, like a move.

// SendTemplate writes the template snapshots snaps to w.
func (z *ZFS) SendTemplate(ctx context.Context, snaps []string, w io.Writer) error {
	for _, s := range snaps {
		if !strings.Contains(s, "@") {
			return fmt.Errorf("template snapshot %q", s)
		}
		if err := z.send(ctx, w, "", s); err != nil {
			return err
		}
	}
	return nil
}

// RecvTemplate makes the template ds (and its data disks ds-disk<i> for idx)
// from a SendTemplate stream; each keeps the snapshot it was sent with.
func (z *ZFS) RecvTemplate(ctx context.Context, ds string, idx []int, r io.Reader) error {
	l := []string{ds}
	for _, i := range idx {
		l = append(l, fmt.Sprintf("%s-disk%d", ds, i))
	}
	for _, d := range l {
		if _, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", d); err == nil {
			return fmt.Errorf("%s already exists", d)
		}
	}
	if k := strings.LastIndex(ds, "/"); k > 0 {
		_, _ = z.Run.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", ds[:k])
	}
	for _, d := range l {
		if err := z.receive(ctx, r, "-o", "volmode=dev", d); err != nil {
			_ = z.DestroyTemplate(context.WithoutCancel(ctx), ds)
			return err
		}
	}
	return nil
}
