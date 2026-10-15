package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VMs with data disks move and are copied (high availability) with all their
// disks: one stream with each disk's ZFS stream framed in turn (root disk
// first, then the data disks in the order given). Every disk is snapshotted
// at the same instant, so the copy is consistent across disks.

// frameWriter cuts a stream into chunks: 4-byte big-endian length + data; a
// zero length ends it.
type frameWriter struct{ w io.Writer }

func (f frameWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := p
		if len(c) > 1<<20 {
			c = c[:1<<20]
		}
		var h [4]byte
		binary.BigEndian.PutUint32(h[:], uint32(len(c)))
		if _, err := f.w.Write(h[:]); err != nil {
			return n, err
		}
		m, err := f.w.Write(c)
		n += m
		if err != nil {
			return n, err
		}
		p = p[len(c):]
	}
	return n, nil
}

func (f frameWriter) Close() error {
	_, err := f.w.Write([]byte{0, 0, 0, 0})
	return err
}

// frameReader reads one framed stream (io.EOF at its end marker).
type frameReader struct {
	r    io.Reader
	left uint32
	done bool
}

func (f *frameReader) Read(p []byte) (int, error) {
	for f.left == 0 {
		if f.done {
			return 0, io.EOF
		}
		var h [4]byte
		if _, err := io.ReadFull(f.r, h[:]); err != nil {
			return 0, fmt.Errorf("disk stream cut short: %w", err)
		}
		f.left = binary.BigEndian.Uint32(h[:])
		if f.left == 0 {
			f.done = true
			return 0, io.EOF
		}
		if f.left > 1<<20 {
			return 0, errors.New("bad disk stream frame")
		}
	}
	if uint32(len(p)) > f.left {
		p = p[:f.left]
	}
	n, err := f.r.Read(p)
	f.left -= uint32(n)
	if err == io.EOF && f.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// drain reads what is left of the framed stream (a receive that stopped early).
func (f *frameReader) drain() error {
	_, err := io.Copy(io.Discard, f)
	return err
}

// disks is the datasets of disk 0 and the given data disks, in that order.
func (z *ZFS) disks(vm string, idx []int) []string {
	l := []string{z.diskDataset(vm)}
	for _, i := range idx {
		l = append(l, z.DiskDataset(vm, i))
	}
	return l
}

// snapAll snapshots every dataset at the same instant.
func (z *ZFS) snapAll(ctx context.Context, ds []string, snap string) error {
	args := []string{"snapshot"}
	for _, d := range ds {
		args = append(args, d+"@"+snap)
	}
	_, err := z.Run.Run(ctx, "zfs", args...)
	return err
}

func (z *ZFS) send(ctx context.Context, w io.Writer, base, snap string) error {
	args := []string{"send", "-c"}
	if base != "" {
		args = append(args, "-i", "@"+base)
	}
	cmd := StreamCmd(ctx, "zfs", append(args, snap)...)
	var stderr strings.Builder
	fw := frameWriter{w}
	cmd.Stdout, cmd.Stderr = fw, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("zfs send %s: %v %s", snap, err, strings.TrimSpace(stderr.String()))
	}
	return fw.Close()
}

func (z *ZFS) receive(ctx context.Context, r io.Reader, args ...string) error {
	fr := &frameReader{r: r}
	cmd := StreamCmd(ctx, "zfs", append([]string{"receive"}, args...)...)
	var stderr strings.Builder
	cmd.Stdin, cmd.Stderr = fr, &stderr
	err := cmd.Run()
	if derr := fr.drain(); err == nil && derr != nil {
		err = derr
	}
	if err != nil {
		return fmt.Errorf("zfs receive %s: %v %s", args[len(args)-1], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SendDisks streams the root disk and the data disks idx of a stopped VM (moves).
func (z *ZFS) SendDisks(ctx context.Context, vm string, idx []int, w io.Writer) error {
	ds := z.disks(vm, idx)
	for _, d := range ds {
		_, _ = z.Run.Run(ctx, "zfs", "destroy", d+"@"+moveSnap)
	}
	if err := z.snapAll(ctx, ds, moveSnap); err != nil {
		return fmt.Errorf("snapshot for the move: %w", err)
	}
	defer func() {
		for _, d := range ds {
			_, _ = z.Run.Run(context.WithoutCancel(ctx), "zfs", "destroy", d+"@"+moveSnap)
		}
	}()
	for _, d := range ds {
		if err := z.send(ctx, w, "", d+"@"+moveSnap); err != nil {
			return err
		}
	}
	return nil
}

// RecvDisks makes the VM's disks from a SendDisks stream; it returns the
// root disk's device and each data disk's device by index.
func (z *ZFS) RecvDisks(ctx context.Context, vm string, idx []int, r io.Reader) (string, map[int]string, error) {
	fail := func(err error) (string, map[int]string, error) {
		_ = z.DestroyDisk(context.WithoutCancel(ctx), vm)
		return "", nil, err
	}
	for _, d := range z.disks(vm, idx) {
		if err := z.receive(ctx, r, "-o", "volmode=dev", d); err != nil {
			return fail(err)
		}
		_, _ = z.Run.Run(ctx, "zfs", "destroy", d+"@"+moveSnap)
	}
	root, err := z.WaitDevice(ctx, vm)
	if err != nil {
		return fail(err)
	}
	extra := map[int]string{}
	for _, i := range idx {
		p, err := z.WaitExtra(ctx, vm, i)
		if err != nil {
			return fail(err)
		}
		extra[i] = p
	}
	return root, extra, nil
}

// --- copies (high availability) -------------------------------------------

func (z *ZFS) replicaDisk(vm string, i int) string {
	return fmt.Sprintf("%s/replica/%s-disk%d", z.Dataset, vm, i)
}

// replicaDisks lists the disk indexes of the VM's copy held here.
func (z *ZFS) replicaDisks(ctx context.Context, vm string) []int {
	out, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "volume", "-d", "1", z.Dataset+"/replica")
	if err != nil {
		return nil
	}
	prefix := z.Dataset + "/replica/" + vm + "-disk"
	var l []int
	for _, name := range strings.Fields(string(out)) {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(rest); err == nil && n >= 0 && n <= 8 && strconv.Itoa(n) == rest {
			l = append(l, n)
		}
	}
	sort.Ints(l)
	return l
}

func (z *ZFS) hasSnap(ctx context.Context, ds, snap string) bool {
	_, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "snapshot", ds+"@"+snap)
	return err == nil
}

// SendReplicaDisks is SendReplica for a VM with data disks idx: every disk
// is snapshotted at once; a disk that has base gets only the change, a disk
// added since gets a full copy. header gets the snapshot name and, per disk
// (root first), the base used ("" = full).
func (z *ZFS) SendReplicaDisks(ctx context.Context, vm string, idx []int, base string, header func(snap string, bases []string) error, w io.Writer) error {
	ds := z.disks(vm, idx)
	bases := make([]string, len(ds))
	for k, d := range ds {
		old, _ := z.repSnaps(ctx, d)
		for _, s := range old {
			if s != base {
				_, _ = z.Run.Run(ctx, "zfs", "destroy", d+"@"+s)
			}
		}
		if base != "" && z.hasSnap(ctx, d, base) {
			bases[k] = base
		}
	}
	snap := fmt.Sprintf("%s%d", RepPrefix, time.Now().UnixNano())
	if err := z.snapAll(ctx, ds, snap); err != nil {
		return fmt.Errorf("snapshot for the copy: %w", err)
	}
	if err := header(snap, bases); err != nil {
		return err
	}
	for k, d := range ds {
		if err := z.send(ctx, w, bases[k], d+"@"+snap); err != nil {
			return err
		}
	}
	return nil
}

// RecvReplicaDisks stores a SendReplicaDisks stream: disk k goes back to
// bases[k] and gets the change, or is replaced by a full copy (base "").
// Copy disks the VM no longer has are dropped.
func (z *ZFS) RecvReplicaDisks(ctx context.Context, vm string, idx []int, bases []string, r io.Reader) error {
	all := append([]int{0}, idx...)
	if len(bases) != len(all) {
		return fmt.Errorf("copy header: %d bases for %d disks", len(bases), len(all))
	}
	want := map[int]bool{}
	for _, i := range all {
		want[i] = true
	}
	for _, i := range z.replicaDisks(ctx, vm) {
		if !want[i] {
			_, _ = z.Run.Run(ctx, "zfs", "destroy", "-r", z.replicaDisk(vm, i))
		}
	}
	if _, err := z.Run.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", z.Dataset+"/replica"); err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("replica dataset: %w", err)
	}
	for k, i := range all {
		ds := z.replicaDisk(vm, i)
		args := []string{"-u", "-o", "volmode=none"}
		if bases[k] == "" {
			if _, err := z.Run.Run(ctx, "zfs", "destroy", "-r", ds); err != nil && !strings.Contains(err.Error(), "does not exist") {
				return err
			}
		} else {
			if _, err := z.Run.Run(ctx, "zfs", "rollback", "-r", ds+"@"+bases[k]); err != nil {
				return fmt.Errorf("copy has no snapshot %s: %w", bases[k], err)
			}
			args = append(args, "-F")
		}
		if err := z.receive(ctx, r, append(args, ds)...); err != nil {
			return err
		}
		snaps, _ := z.repSnaps(ctx, ds)
		for j := 0; j+1 < len(snaps); j++ {
			_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+snaps[j])
		}
	}
	return nil
}

// ReplicaSnapsDisks are the copy snapshots every one of the copy's disks has.
func (z *ZFS) ReplicaSnapsDisks(ctx context.Context, vm string, idx []int) ([]string, error) {
	first, err := z.repSnaps(ctx, z.replicaDisk(vm, 0))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range first {
		ok := true
		for _, i := range idx {
			if !z.hasSnap(ctx, z.replicaDisk(vm, i), s) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
}
