package ct

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/backup"
	"p8sos/internal/jobs"
)

func TestBackupRestore(t *testing.T) {
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not installed")
	}
	s, run := newSvc(t)
	ctx := context.Background()
	out := t.TempDir()
	script := filepath.Join(out, "zfs")
	os.WriteFile(script, []byte(`#!/bin/sh
case "$1" in
  send) printf 'STREAM of %s\n' "$2" ;;
  receive) for last; do :; done; cat > "$OUT/$(echo "$last" | tr / _)" ;;
esac
`), 0o755)
	var calls []string
	s.Backups = &backup.Store{Dir: t.TempDir(), Run: run, Compressor: "gzip",
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if name == "nsenter" && len(args) > 2 && args[2] == "zfs" {
				name, args = script, args[3:]
			}
			c := exec.CommandContext(ctx, name, args...)
			c.Env = append(os.Environ(), "OUT="+out)
			return c
		}}
	s.Jobs = &jobs.Manager{}
	if _, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", DiskGB: 10, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	j, err := s.StartBackup(ctx, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	s.Jobs.Wait()
	if j, _ = s.Jobs.Get(j.ID); j.State != jobs.Done {
		t.Fatalf("backup job %+v", j)
	}
	if l := last(run, "lxc-freeze"); l == "" {
		t.Fatal("running container was not frozen")
	}
	if l := last(run, "nsenter --mount=/proc/1/ns/mnt -- zfs destroy tank/p8s/ct/web@p8s-backup-"); l == "" {
		t.Fatal("backup snapshot not removed")
	}
	if !strings.HasPrefix(calls[0], "nsenter --mount=/proc/1/ns/mnt -- zfs send tank/p8s/ct/web@p8s-backup-") {
		t.Fatalf("send: %q", calls[0])
	}
	l, err := s.ListBackups("web")
	if err != nil || len(l) != 1 || l[0].Owner != "alice" || l[0].DiskGB != 10 || !l[0].Consistent {
		t.Fatalf("list %+v %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(s.Backups.Dir, "_ct", "web")); err != nil {
		t.Fatal("not under _ct:", err)
	}
	// taken names are refused
	if _, err := s.StartRestore(ctx, "web", l[0].ID, "web", nil); err == nil {
		t.Fatal("restored over a live container")
	}
	if _, err := s.StartRestore(ctx, "web", l[0].ID, "vm1", func(n string) bool { return n == "vm1" }); err == nil {
		t.Fatal("restored over a VM name")
	}
	_ = os.MkdirAll(filepath.Join(s.MountDir, "web2", "etc"), 0o755) // what "zfs mount" would show
	j, err = s.StartRestore(ctx, "web", l[0].ID, "web2", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Jobs.Wait()
	if j, _ = s.Jobs.Get(j.ID); j.State != jobs.Done || !strings.Contains(j.Message, "restored") {
		t.Fatalf("restore job %+v", j)
	}
	got, _ := os.ReadFile(filepath.Join(out, "tank_p8s_ct_web2-restore"))
	if !strings.HasPrefix(string(got), "STREAM of tank/p8s/ct/web@") {
		t.Fatalf("received %q", got)
	}
	if l := last(run, "nsenter --mount=/proc/1/ns/mnt -- zfs rename"); l != "nsenter --mount=/proc/1/ns/mnt -- zfs rename tank/p8s/ct/web2-restore tank/p8s/ct/web2" {
		t.Fatalf("rename %q", l)
	}
	if l := last(run, "nsenter --mount=/proc/1/ns/mnt -- zfs set"); !strings.Contains(l, "refquota=10G") || !strings.HasSuffix(l, "tank/p8s/ct/web2") {
		t.Fatalf("set %q", l)
	}
	c, err := s.Get(ctx, "web2")
	if err != nil || c.Owner != "alice" || c.DiskGB != 10 {
		t.Fatalf("restored %+v %v", c, err)
	}
	if h, _ := os.ReadFile(filepath.Join(s.MountDir, "web2", "etc/hostname")); string(h) != "web2\n" {
		t.Fatalf("hostname %q", h)
	}
	if err := s.DeleteBackup("web", l[0].ID); err != nil {
		t.Fatal(err)
	}
}
