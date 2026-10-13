package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/testfake"
)

func TestTasks(t *testing.T) {
	dir := t.TempDir()
	run := &testfake.Runner{Out: map[string]string{"systemctl is-active": "active\n", "journalctl": "==> importing\n"}}
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	m := &Manager{Dir: dir, Scripts: "/usr/local/share/p8s", Run: run, Now: func() time.Time { return now }}
	ctx := context.Background()
	if _, err := m.Start(ctx, Template, "../etc", "boss"); err == nil {
		t.Fatal("bad template name")
	}
	if _, err := m.Start(ctx, "rm-rf", "", "boss"); err == nil {
		t.Fatal("unknown kind")
	}
	tk, err := m.Start(ctx, Template, "ubuntu-24.04", "boss")
	if err != nil {
		t.Fatal(err)
	}
	last := run.Calls[len(run.Calls)-1]
	if !strings.HasPrefix(last, "systemd-run --unit="+tk.Unit) || !strings.HasSuffix(last, "/usr/local/share/p8s/make-template.sh ubuntu-24.04 --register") {
		t.Fatalf("ran %q", last)
	}
	now = now.Add(time.Second)
	if _, err := m.Start(ctx, StorageGrow, "", "boss"); err != ErrBusy {
		t.Fatalf("second task: %v", err)
	}
	g, ok := m.Get(ctx, tk.ID)
	if !ok || g.State != "running" || !strings.Contains(g.Log, "importing") {
		t.Fatalf("get %+v", g)
	}
	// it finishes with exit 0
	_ = os.WriteFile(filepath.Join(dir, tk.ID+".rc"), []byte("0\n"), 0o600)
	if g, _ := m.Get(ctx, tk.ID); g.State != "ok" || *g.Exit != 0 {
		t.Fatalf("done %+v", g)
	}
	// next one fails
	now = now.Add(time.Second)
	tk2, err := m.Start(ctx, OSUpgrade, "", "boss")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, tk2.ID+".rc"), []byte("100\n"), 0o600)
	l := m.List(ctx)
	if len(l) != 2 || l[0].ID != tk2.ID || l[0].State != "failed" {
		t.Fatalf("list %+v", l)
	}
	// a unit that vanished without an exit code (p8sd restart, kill) is failed after a while
	now = now.Add(time.Second)
	tk3, _ := m.Start(ctx, StorageGrow, "", "boss")
	run.Out["systemctl is-active"] = "inactive\n"
	now = now.Add(time.Minute)
	m2 := &Manager{Dir: dir, Scripts: m.Scripts, Run: run, Now: m.Now}
	if g, _ := m2.Get(ctx, tk3.ID); g.State != "failed" {
		t.Fatalf("vanished %+v", g)
	}
}

func TestZFSCacheArg(t *testing.T) {
	m := &Manager{Scripts: "/s"}
	if c, err := m.command(ZFSCache, "2048"); err != nil || strings.Join(c, " ") != "/bin/bash /s/tune.sh --arc-max 2048" {
		t.Fatal(c, err)
	}
	if _, err := m.command(ZFSCache, "auto"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "100", "1;reboot", "-1"} {
		if _, err := m.command(ZFSCache, bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
