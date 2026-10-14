package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Raw backups: whole disk images, for disks ZFS can't send (VMs on Ceph).
// Each disk is read by a command the caller gives (rbd export … -) and
// stored compressed; restoring writes the image onto a new or existing disk.

// SaveRaw stores m's disks: src(i) is the command that writes disk i
// (0 = root) to stdout. sizes are the disks' exact sizes.
func (s *Store) SaveRaw(ctx context.Context, m Meta, sizes map[int]int64, src func(ctx context.Context, i int) *exec.Cmd) (Meta, error) {
	if !reVM.MatchString(m.VM) || !reID.MatchString(m.ID) {
		return Meta{}, fmt.Errorf("bad backup name %q/%q", m.VM, m.ID)
	}
	m.Format, m.Base, m.BaseSnap = FormatRaw, "", ""
	m.Compression = s.compressor()
	_, metaPath := s.paths(m.VM, m.ID, m.Compression)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		return Meta{}, err
	}
	var done []string
	fail := func(err error) (Meta, error) {
		for _, f := range done {
			os.Remove(f)
		}
		return Meta{}, err
	}
	write := func(i int) (int64, error) {
		path := s.rawPath(m, i)
		tmp := path + ".partial"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return 0, err
		}
		if err := pipe(src(ctx, i), s.cmd(ctx, m.Compression, compArgs(m.Compression)...), nil, f); err != nil {
			f.Close()
			os.Remove(tmp)
			return 0, fmt.Errorf("disk %d: %w", i, err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(tmp)
			return 0, err
		}
		f.Close()
		st, err := os.Stat(tmp)
		if err != nil {
			return 0, err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return 0, err
		}
		done = append(done, path)
		return st.Size(), nil
	}
	for k, e := range m.Extra {
		n, err := write(e.Index)
		if err != nil {
			return fail(err)
		}
		m.Extra[k].Bytes, m.Extra[k].ImageBytes = n, sizes[e.Index]
	}
	n, err := write(0)
	if err != nil {
		return fail(err)
	}
	m.Bytes, m.ImageBytes = n, sizes[0]
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := s.writeMeta(m); err != nil {
		return fail(err)
	}
	return m, nil
}

func compArgs(comp string) []string {
	if comp == "zstd" {
		return []string{"-q", "-c", "-T0", "-3"}
	}
	return []string{"-c"}
}

// RestoreRaw writes a raw backup's disks onto devices (disk index → block
// device of at least the disk's size; it should read as zeros or be
// overwritten completely: zero blocks are skipped).
func (s *Store) RestoreRaw(ctx context.Context, m Meta, devs map[int]string) error {
	if m.Format != FormatRaw {
		return fmt.Errorf("backup %s isn't a disk-image backup", m.ID)
	}
	for i, dev := range devs {
		file := s.rawPath(m, i)
		// conv=sparse would leave old data in zero blocks of a reused disk; write everything
		dd := s.cmd(ctx, "dd", "of="+dev, "bs=4M", "iflag=fullblock", "conv=fsync,notrunc", "status=none")
		if err := pipe(s.cmd(ctx, m.Compression, "-dc", file), dd, nil, io.Discard); err != nil {
			return fmt.Errorf("disk %d: %w", i, err)
		}
	}
	return nil
}

// Sizes are the disks' sizes in a raw backup.
func (m Meta) Sizes() map[int]int64 {
	out := map[int]int64{0: m.ImageBytes}
	for _, e := range m.Extra {
		out[e.Index] = e.ImageBytes
	}
	return out
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// verifyRaw decompresses every disk and checks it is the full size.
func (s *Store) verifyRaw(ctx context.Context, m Meta) error {
	for i, want := range m.Sizes() {
		c := &counter{}
		cmd := s.cmd(ctx, m.Compression, "-dc", s.rawPath(m, i))
		cmd.Stdout = c
		var eb stringsBuilder
		cmd.Stderr = &eb
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("disk %d: damaged backup file: %v %s", i, err, eb.String())
		}
		if want > 0 && c.n != want {
			return fmt.Errorf("disk %d: %d bytes in the backup, the disk had %d", i, c.n, want)
		}
	}
	return nil
}

type stringsBuilder struct{ b []byte }

func (w *stringsBuilder) Write(p []byte) (int, error) {
	if len(w.b) < 4096 {
		w.b = append(w.b, p...)
	}
	return len(p), nil
}
func (w *stringsBuilder) String() string { return string(w.b) }

// ReceiveDisk restores disk i of a ZFS-stream backup (with the backups it
// builds on) into dataset ds as a device (for browsing its files).
func (s *Store) ReceiveDisk(ctx context.Context, m Meta, i int, ds string) error {
	chain, err := s.Chain(m)
	if err != nil {
		return err
	}
	return s.receiveChain(ctx, chain, i, ds, []string{"volmode=dev", "readonly=on"})
}

// RawToFile writes disk i of a raw backup into a sparse file (for browsing).
func (s *Store) RawToFile(ctx context.Context, m Meta, i int, path string) error {
	dd := s.cmd(ctx, "dd", "of="+path, "bs=4M", "iflag=fullblock", "conv=sparse", "status=none")
	if err := pipe(s.cmd(ctx, m.Compression, "-dc", s.rawPath(m, i)), dd, nil, io.Discard); err != nil {
		return fmt.Errorf("disk %d: %w", i, err)
	}
	return nil
}
