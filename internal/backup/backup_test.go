package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/testfake"
)

// fakeZFS stands in for `zfs send` / `zfs receive`: send writes a recognizable
// stream, receive stores what it got in $OUT/<dataset with / as _>.
func fakeZFS(t *testing.T) (dir string, cmd func(ctx context.Context, name string, args ...string) *exec.Cmd) {
	dir = t.TempDir()
	script := filepath.Join(dir, "zfs")
	os.WriteFile(script, []byte(`#!/bin/sh
case "$1" in
  send) [ "$2" = "tank/vms/p8s-bad-disk0@p8s-backup-20261012-023000" ] && { echo "cannot open: dataset does not exist" >&2; exit 1; }
        printf 'STREAM of %s\n' "$2"; i=0; while [ $i -lt 2000 ]; do echo "block $i of the disk"; i=$((i+1)); done ;;
  receive) for last; do :; done; cat > "$OUT/$(echo "$last" | tr / _)" ;;
esac
`), 0o755)
	return dir, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "zfs" {
			name = script
		}
		c := exec.CommandContext(ctx, name, args...)
		c.Env = append(os.Environ(), "OUT="+dir)
		return c
	}
}

func TestSaveListRestorePrune(t *testing.T) {
	for _, comp := range []string{"zstd", "gzip"} {
		if _, err := exec.LookPath(comp); err != nil {
			t.Skipf("%s not installed", comp)
		}
		t.Run(comp, func(t *testing.T) {
			out, cmd := fakeZFS(t)
			r := &testfake.Runner{}
			now := time.Date(2026, 10, 12, 2, 30, 0, 0, time.UTC)
			s := &Store{Dir: t.TempDir(), Dataset: "tank/vms", Run: r, Cmd: cmd, Compressor: comp, Now: func() time.Time { return now }}
			ctx := context.Background()

			id := s.NewID()
			m, err := s.Save(ctx, "p8s-web1", "p8s-backup-"+id, Meta{ID: id, VM: "web1", VCPUs: 2, DiskGB: 10, Template: "debian-13"})
			if err != nil {
				t.Fatal(err)
			}
			if m.ID != "20261012-023000" || m.Bytes == 0 || m.Compression != comp {
				t.Fatalf("meta %+v", m)
			}
			if _, err := os.Stat(filepath.Join(s.Dir, "web1", "web1-"+id+ext(comp)+".partial")); err == nil {
				t.Fatal("partial file left behind")
			}

			// A failing stream leaves nothing behind.
			if _, err := s.Save(ctx, "p8s-bad", "p8s-backup-"+id, Meta{ID: id, VM: "bad"}); err == nil ||
				!strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("failed send: %v", err)
			}
			if l, _ := s.List("bad"); len(l) != 0 {
				t.Fatalf("broken backup listed: %+v", l)
			}

			// Restore as a new VM: stream comes back byte for byte.
			if err := s.Restore(ctx, m, "p8s-copy1", false); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(out, "tank_vms_p8s-copy1-disk0-restore"))
			if !bytes.HasPrefix(got, []byte("STREAM of tank/vms/p8s-web1-disk0@p8s-backup-"+id)) || len(got) < 30000 {
				t.Fatalf("restored stream %d bytes: %.60q", len(got), got)
			}
			calls := strings.Join(r.Calls, "\n")
			if !strings.Contains(calls, "zfs rename tank/vms/p8s-copy1-disk0-restore tank/vms/p8s-copy1-disk0") ||
				strings.Contains(calls, "zfs destroy -r tank/vms/p8s-copy1-disk0\n") {
				t.Fatalf("new-VM restore calls:\n%s", calls)
			}
			// Restore in place replaces the current disk.
			r.Calls = nil
			if err := s.Restore(ctx, m, "p8s-web1", true); err != nil {
				t.Fatal(err)
			}
			if c := strings.Join(r.Calls, "\n"); !strings.Contains(c, "zfs destroy -r tank/vms/p8s-web1-disk0\nzfs rename") {
				t.Fatalf("in-place restore calls:\n%s", c)
			}

			// Three more backups, then keep 2.
			for i := 1; i <= 3; i++ {
				now = now.Add(time.Hour)
				id := s.NewID()
				if _, err := s.Save(ctx, "p8s-web1", "p8s-backup-"+id, Meta{ID: id, VM: "web1"}); err != nil {
					t.Fatal(err)
				}
			}
			all, _ := s.List("")
			if len(all) != 4 || all[0].ID != "20261012-053000" {
				t.Fatalf("list: %+v", all)
			}
			if n, err := s.Prune("web1", 2); err != nil || n != 2 {
				t.Fatalf("prune: %d %v", n, err)
			}
			left, _ := s.List("web1")
			if len(left) != 2 || left[1].ID != "20261012-043000" {
				t.Fatalf("after prune: %+v", left)
			}
			if err := s.Delete("web1", left[0].ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get("web1", left[0].ID); err != ErrNotFound {
				t.Fatalf("deleted backup still there: %v", err)
			}
			if _, err := s.Get("../etc", "x"); err != ErrNotFound {
				t.Fatal("path tricks must not work")
			}
		})
	}
}

// chainZFS logs every zfs send/receive command line to $OUT/log.
func chainZFS(t *testing.T) (string, func(ctx context.Context, name string, args ...string) *exec.Cmd) {
	dir := t.TempDir()
	script := filepath.Join(dir, "zfs")
	os.WriteFile(script, []byte(`#!/bin/sh
echo "$*" >> "$OUT/log"
case "$1" in
  send) printf 'STREAM %s\n' "$*" ;;
  receive) cat > /dev/null; case "$*" in *broken*) echo "cannot receive: invalid stream (checksum mismatch)" >&2; exit 1;; esac ;;
esac
`), 0o755)
	return dir, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "zfs" {
			name = script
		}
		c := exec.CommandContext(ctx, name, args...)
		c.Env = append(os.Environ(), "OUT="+dir)
		return c
	}
}

func TestIncrementalChain(t *testing.T) {
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not installed")
	}
	out, cmd := chainZFS(t)
	r := &testfake.Runner{}
	s := &Store{Dir: t.TempDir(), Dataset: "tank/vms", Run: r, Cmd: cmd, Compressor: "gzip"}
	ctx := context.Background()
	save := func(id, base string) Meta {
		t.Helper()
		m := Meta{ID: id, VM: "db1", Base: base}
		if base != "" {
			m.BaseSnap = "p8s-backup-" + base
		}
		got, err := s.Save(ctx, "p8s-db1", "p8s-backup-"+id, m)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	save("20261001-020000", "")
	save("20261002-020000", "20261001-020000")
	m3 := save("20261003-020000", "20261002-020000")
	log, _ := os.ReadFile(filepath.Join(out, "log"))
	if !strings.Contains(string(log), "send -i @p8s-backup-20261002-020000 tank/vms/p8s-db1-disk0@p8s-backup-20261003-020000") {
		t.Fatalf("incremental send missing:\n%s", log)
	}
	if _, err := s.Save(ctx, "p8s-db1", "p8s-backup-x", Meta{ID: "20261004-020000", VM: "db1", Base: "20261003-020000"}); err == nil {
		t.Fatal("base without its snapshot accepted")
	}
	chain, err := s.Chain(m3)
	if err != nil || len(chain) != 3 || chain[0].ID != "20261001-020000" {
		t.Fatalf("chain %v %v", chain, err)
	}
	// restore receives the full one, then each change set on top
	os.Remove(filepath.Join(out, "log"))
	if err := s.Restore(ctx, m3, "p8s-db1", true); err != nil {
		t.Fatal(err)
	}
	log, _ = os.ReadFile(filepath.Join(out, "log"))
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "receive -u -o volmode=dev tank/vms/p8s-db1-disk0-restore") ||
		!strings.Contains(lines[2], "receive -u -F tank/vms/p8s-db1-disk0-restore") {
		t.Fatalf("receives:\n%s", log)
	}
	calls := strings.Join(r.Calls, "\n")
	if !strings.Contains(calls, "zfs destroy tank/vms/p8s-db1-disk0-restore@p8s-backup-20261001-020000") ||
		!strings.Contains(calls, "zfs rename tank/vms/p8s-db1-disk0-restore tank/vms/p8s-db1-disk0") {
		t.Fatalf("calls %s", calls)
	}
	// the middle one can't go while the newest builds on it
	if err := s.Delete("db1", "20261002-020000"); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete base: %v", err)
	}
	// prune to 1: the chain it needs stays
	if n, err := s.Prune("db1", 1); err != nil || n != 0 {
		t.Fatalf("prune %d %v", n, err)
	}
	// a new full backup: prune to 1 removes the whole old chain
	save("20261008-020000", "")
	if n, err := s.Prune("db1", 1); err != nil || n != 3 {
		t.Fatalf("prune after full %d %v", n, err)
	}
	// restore test
	m, _ := s.Get("db1", "20261008-020000")
	if v, err := s.Verify(ctx, m); err != nil || v.VerifiedAt == "" {
		t.Fatalf("verify %v %+v", err, v)
	}
	if m, _ := s.Get("db1", "20261008-020000"); m.VerifiedAt == "" {
		t.Fatal("verify result not saved")
	}
	if !strings.Contains(strings.Join(r.Calls, "\n"), "zfs destroy -r tank/vms/p8s-verify-db1-disk0") {
		t.Fatal("scratch disk not removed")
	}
	// a missing base is reported clearly
	os.Remove(filepath.Join(s.Dir, "db1", "db1-20261008-020000.json"))
	save("20261009-020000", "")
	broken := Meta{ID: "20261010-020000", VM: "db1", Base: "20261099-020000", Compression: "gzip"}
	if _, err := s.Chain(broken); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing base: %v", err)
	}
}
