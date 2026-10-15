package backup

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Incremental backups of Ceph disks are `rbd export-diff` streams: only the
// blocks that changed since the previous backup's snapshot. Restoring writes
// the full image of the chain's first backup, then applies each diff in turn.
// The diff format is read here (not with `rbd import-diff`) so a backup can be
// restored onto any disk: a Ceph image, a ZFS volume or a plain file.
//
// Format (v1; v2 adds a little-endian u64 record length after each tag):
//
//	"rbd diff v1\n", then records:
//	'f' u32 len + name   from snapshot
//	't' u32 len + name   to snapshot
//	's' u64              image size
//	'w' u64 off, u64 len, data
//	'z' u64 off, u64 len  zeroed
//	'e'                  end

// diffTarget is where a diff is applied.
type diffTarget interface {
	io.WriterAt
}

// DiffInfo is what a diff stream says about itself.
type DiffInfo struct {
	From, To string
	Size     int64 // image size after the diff (-1 = not given)
	Written  int64 // bytes of changed data
}

var errDiffFormat = errors.New("not an rbd diff stream")

// ApplyRBDDiff reads an rbd diff from r and writes its changes to dst (nil =
// only check the stream). zero zeroes a range of dst (nil = write zeros).
func ApplyRBDDiff(r io.Reader, dst diffTarget, zero func(off, n int64) error) (DiffInfo, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	info := DiffInfo{Size: -1}
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return info, fmt.Errorf("%w: %v", errDiffFormat, err)
	}
	var v2 bool
	switch string(hdr) {
	case "rbd diff v1\n":
	case "rbd diff v2\n":
		v2 = true
	default:
		return info, errDiffFormat
	}
	u32 := func() (uint32, error) {
		var b [4]byte
		_, err := io.ReadFull(br, b[:])
		return binary.LittleEndian.Uint32(b[:]), err
	}
	u64 := func() (int64, error) {
		var b [8]byte
		_, err := io.ReadFull(br, b[:])
		v := binary.LittleEndian.Uint64(b[:])
		if v > 1<<62 {
			return 0, fmt.Errorf("%w: bad number", errDiffFormat)
		}
		return int64(v), err
	}
	str := func() (string, error) {
		n, err := u32()
		if err != nil {
			return "", err
		}
		if n > 4096 {
			return "", fmt.Errorf("%w: bad name", errDiffFormat)
		}
		b := make([]byte, n)
		_, err = io.ReadFull(br, b)
		return string(b), err
	}
	buf := make([]byte, 1<<20)
	zeros := make([]byte, 1<<20)
	for {
		tag, err := br.ReadByte()
		if err != nil {
			return info, fmt.Errorf("diff stream ends early (no end record): %v", err)
		}
		if v2 && tag != 'e' {
			if _, err := u64(); err != nil { // record length; the fields say it too
				return info, err
			}
		}
		switch tag {
		case 'f', 't':
			s, err := str()
			if err != nil {
				return info, err
			}
			if tag == 'f' {
				info.From = s
			} else {
				info.To = s
			}
		case 's':
			if info.Size, err = u64(); err != nil {
				return info, err
			}
		case 'w', 'z':
			off, err := u64()
			if err != nil {
				return info, err
			}
			n, err := u64()
			if err != nil {
				return info, err
			}
			if info.Size >= 0 && off+n > info.Size {
				return info, fmt.Errorf("%w: change past the end of the disk", errDiffFormat)
			}
			if tag == 'z' {
				if dst == nil {
					continue
				}
				if zero != nil {
					if err := zero(off, n); err != nil {
						return info, err
					}
					continue
				}
				for n > 0 {
					k := min(n, int64(len(zeros)))
					if _, err := dst.WriteAt(zeros[:k], off); err != nil {
						return info, err
					}
					off, n = off+k, n-k
				}
				continue
			}
			info.Written += n
			for n > 0 {
				k := min(n, int64(len(buf)))
				if _, err := io.ReadFull(br, buf[:k]); err != nil {
					return info, fmt.Errorf("diff stream ends early: %v", err)
				}
				if dst != nil {
					if _, err := dst.WriteAt(buf[:k], off); err != nil {
						return info, err
					}
				}
				off, n = off+k, n-k
			}
		case 'e':
			return info, nil
		default:
			return info, fmt.Errorf("%w: unknown record %q", errDiffFormat, tag)
		}
	}
}

// applyDiffFile decompresses one diff file of a backup onto path.
func (s *Store) applyDiffFile(ctx context.Context, m Meta, file, path string, sparse bool) (DiffInfo, error) {
	in, err := os.Open(file)
	if err != nil {
		return DiffInfo{}, err
	}
	defer in.Close()
	dec := s.cmd(ctx, m.Compression, "-dc")
	dec.Stdin = in
	out, err := dec.StdoutPipe()
	if err != nil {
		return DiffInfo{}, err
	}
	var eb stringsBuilder
	dec.Stderr = &eb
	if err := dec.Start(); err != nil {
		return DiffInfo{}, err
	}
	var dst diffTarget
	var zero func(off, n int64) error
	var f *os.File
	if path != "" {
		f, err = os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			_ = dec.Process.Kill()
			_ = dec.Wait()
			return DiffInfo{}, err
		}
		dst = f
		if sparse {
			zero = func(off, n int64) error { return punchHole(f, off, n) }
		}
	}
	info, aerr := ApplyRBDDiff(out, dst, zero)
	if aerr != nil {
		_ = dec.Process.Kill()
	} else {
		_, _ = io.Copy(io.Discard, out)
	}
	werr := dec.Wait()
	if f != nil {
		if err := f.Sync(); err != nil && aerr == nil {
			aerr = err
		}
		if err := f.Close(); err != nil && aerr == nil {
			aerr = err
		}
		if aerr == nil && sparse && info.Size > 0 {
			aerr = os.Truncate(path, info.Size)
		}
	}
	if aerr != nil {
		return info, aerr
	}
	if werr != nil {
		return info, fmt.Errorf("damaged backup file: %v %s", werr, eb.String())
	}
	return info, nil
}

// punchHole zeroes a range of a sparse file by freeing its blocks.
func punchHole(f *os.File, off, n int64) error {
	const punch, keepSize = 0x02, 0x01
	if err := syscall.Fallocate(int(f.Fd()), punch|keepSize, off, n); err == nil {
		return nil
	}
	zeros := make([]byte, 1<<20)
	for n > 0 {
		k := min(n, int64(len(zeros)))
		if _, err := f.WriteAt(zeros[:k], off); err != nil {
			return err
		}
		off, n = off+k, n-k
	}
	return nil
}
