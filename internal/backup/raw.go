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
// (0 = root) to stdout. sizes are the disks' exact sizes. With m.Base set,
// src writes rbd diffs since that backup (rbd export-diff) instead of images.
func (s *Store) SaveRaw(ctx context.Context, m Meta, sizes map[int]int64, src func(ctx context.Context, i int) *exec.Cmd) (Meta, error) {
	if !reVM.MatchString(m.VM) || !reID.MatchString(m.ID) {
		return Meta{}, fmt.Errorf("bad backup name %q/%q", m.VM, m.ID)
	}
	m.Format, m.BaseSnap = FormatRaw, ""
	if m.Base != "" && !reID.MatchString(m.Base) {
		return Meta{}, fmt.Errorf("bad base backup %q", m.Base)
	}
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
// device of at least the disk's size; it is overwritten completely). An
// incremental one is restored with the backups it builds on: the full image
// first, then each one's changes.
func (s *Store) RestoreRaw(ctx context.Context, m Meta, devs map[int]string) error {
	if m.Format != FormatRaw {
		return fmt.Errorf("backup %s isn't a disk-image backup", m.ID)
	}
	chain, err := s.Chain(m)
	if err != nil {
		return err
	}
	for i, dev := range devs {
		if err := s.writeRawChain(ctx, chain, i, dev, false); err != nil {
			return fmt.Errorf("disk %d: %w", i, err)
		}
	}
	return nil
}

// writeRawChain writes disk i of a chain of raw backups onto path. sparse:
// path is a new file (zero blocks are skipped, the file gets the disk's size).
func (s *Store) writeRawChain(ctx context.Context, chain []Meta, i int, path string, sparse bool) error {
	full := chain[0]
	if full.Base != "" {
		return fmt.Errorf("backup %s isn't a full backup", full.ID)
	}
	conv := "conv=fsync,notrunc" // a reused disk: write everything (zero blocks too)
	if sparse {
		conv = "conv=sparse"
	}
	dd := s.cmd(ctx, "dd", "of="+path, "bs=4M", "iflag=fullblock", conv, "status=none")
	if err := pipe(s.cmd(ctx, full.Compression, "-dc", s.rawPath(full, i)), dd, nil, io.Discard); err != nil {
		return err
	}
	if sparse {
		if n := full.Sizes()[i]; n > 0 {
			if err := os.Truncate(path, n); err != nil {
				return err
			}
		}
	} else if last := chain[len(chain)-1].Sizes()[i]; len(chain) > 1 && last > full.Sizes()[i] {
		// the disk grew since the full backup: the new part starts as zeros
		// (the diffs only hold what was written there)
		if err := zeroRange(path, full.Sizes()[i], last-full.Sizes()[i]); err != nil {
			return err
		}
	}
	for _, b := range chain[1:] {
		if _, err := s.applyDiffFile(ctx, b, s.rawPath(b, i), path, sparse); err != nil {
			return fmt.Errorf("changes of backup %s: %w", b.ID, err)
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

// verifyRaw decompresses every disk and checks it is the full size; for an
// incremental one, every diff of its chain is read through to its end and
// must lead to the disk's size.
func (s *Store) verifyRaw(ctx context.Context, m Meta) error {
	chain, err := s.Chain(m)
	if err != nil {
		return err
	}
	for i, want := range m.Sizes() {
		full := chain[0]
		c := &counter{}
		cmd := s.cmd(ctx, full.Compression, "-dc", s.rawPath(full, i))
		cmd.Stdout = c
		var eb stringsBuilder
		cmd.Stderr = &eb
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("disk %d: damaged backup file %s: %v %s", i, full.ID, err, eb.String())
		}
		size := c.n
		if fw := full.Sizes()[i]; fw > 0 && c.n != fw {
			return fmt.Errorf("disk %d: %d bytes in backup %s, the disk had %d", i, c.n, full.ID, fw)
		}
		for _, b := range chain[1:] {
			info, err := s.applyDiffFile(ctx, b, s.rawPath(b, i), "", false)
			if err != nil {
				return fmt.Errorf("disk %d, backup %s: %w", i, b.ID, err)
			}
			if info.Size >= 0 {
				size = info.Size
			}
		}
		if want > 0 && size != want {
			return fmt.Errorf("disk %d: the backup leads to %d bytes, the disk had %d", i, size, want)
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

// RawToFile writes disk i of a raw backup (with the backups it builds on)
// into a new sparse file (for browsing).
func (s *Store) RawToFile(ctx context.Context, m Meta, i int, path string) error {
	chain, err := s.Chain(m)
	if err != nil {
		return err
	}
	if err := s.writeRawChain(ctx, chain, i, path, true); err != nil {
		return fmt.Errorf("disk %d: %w", i, err)
	}
	return nil
}

// zeroRange writes zeros over n bytes of path from off.
func zeroRange(path string, off, n int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	zeros := make([]byte, 1<<20)
	for n > 0 {
		k := min(n, int64(len(zeros)))
		if _, err := f.WriteAt(zeros[:k], off); err != nil {
			f.Close()
			return err
		}
		off, n = off+k, n-k
	}
	return f.Close()
}
