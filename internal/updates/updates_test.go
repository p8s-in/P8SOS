package updates

import (
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/license"
	"p8sos/internal/licsrv"
	"p8sos/internal/testfake"
)

func TestUpdateFlow(t *testing.T) {
	dir := t.TempDir()
	pubB64, err := licsrv.GenerateKey(filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	key, _ := licsrv.LoadKey(filepath.Join(dir, "k"))
	trusted := license.ParsePublicKeys(pubB64)
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	st := &licsrv.Store{Dir: filepath.Join(dir, "ls"), Key: key, Now: func() time.Time { return now }}
	cfg, _ := licsrv.LoadConfig(filepath.Join(dir, "none.json"))
	ts := httptest.NewServer((&licsrv.Server{Cfg: cfg, Store: st}).Handler())
	defer ts.Close()

	src := filepath.Join(dir, "src")
	_ = os.MkdirAll(filepath.Join(src, "cmd"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "update.sh"), []byte("#!/bin/bash\necho hi\n"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "cmd", "x.go"), []byte("package x"), 0o644)

	const server = "P8S-AAAA-0000-0000-0001"
	info := Channel{Allowed: []string{"testing"}, Server: server}
	run := &testfake.Runner{Out: map[string]string{}}
	m := &Manager{Dir: filepath.Join(dir, "upd"), Server: ts.URL, Current: "0.5.0", Info: func() Channel { return info },
		Keys: trusted, Run: run, Now: func() time.Time { return now }}

	// nothing published yet
	if err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(context.Background()); s.Update || s.Channel != "testing" {
		t.Fatalf("empty: %+v", s)
	}
	if _, err := m.Install(context.Background()); err != ErrNone {
		t.Fatalf("install with nothing: %v", err)
	}

	// 0.6.0 on testing
	if _, err := st.AddRelease(src, "0.6.0", "subscriptions", false); err != nil {
		t.Fatal(err)
	}
	_ = m.Check(context.Background())
	s := m.Status(context.Background())
	if !s.Update || s.Available.Version != "0.6.0" {
		t.Fatalf("available: %+v", s)
	}

	// stable is refused without a subscription
	if err := m.SetChannel("stable"); err == nil {
		t.Fatal("stable without a subscription")
	}

	unit, err := m.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	last := run.Calls[len(run.Calls)-1]
	if !strings.HasPrefix(last, "systemd-run --unit="+unit) || !strings.HasSuffix(last, "src-0.6.0/update.sh") {
		t.Fatalf("ran %q", last)
	}
	if b, _ := os.ReadFile(filepath.Join(m.Dir, "src-0.6.0", "VERSION")); string(b) != "0.6.0\n" {
		t.Fatal("not unpacked")
	}
	run.Out["systemctl is-active"] = "active\n"
	run.Out["journalctl"] = "building p8sd\n"
	if s := m.Status(context.Background()); s.Install == nil || s.Install.State != "running" || !strings.Contains(s.Install.Log, "building") {
		t.Fatalf("running: %+v", s.Install)
	}
	if _, err := m.Install(context.Background()); err != ErrBusy {
		t.Fatalf("second install: %v", err)
	}
	// p8sd restarted on the new version
	run.Out["systemctl is-active"] = "inactive\n"
	m2 := &Manager{Dir: m.Dir, Server: ts.URL, Current: "0.6.0", Info: m.Info, Keys: trusted, Run: run}
	if s := m2.Status(context.Background()); s.Install.State != "done" || s.Update {
		t.Fatalf("done: %+v", s)
	}
	// ...or it failed and the old one is still running
	if s := m.Status(context.Background()); s.Install.State != "failed" {
		t.Fatalf("failed: %+v", s.Install)
	}

	// a release signed by someone else is not offered
	_, other, _ := ed25519.GenerateKey(nil)
	evil := &licsrv.Store{Dir: filepath.Join(dir, "evil"), Key: other, Now: st.Now}
	if _, err := evil.AddRelease(src, "0.6.1", "", false); err != nil {
		t.Fatal(err)
	}
	es := httptest.NewServer((&licsrv.Server{Cfg: cfg, Store: evil}).Handler())
	defer es.Close()
	me := &Manager{Dir: filepath.Join(dir, "upd-evil"), Server: es.URL, Current: "0.5.0", Info: m.Info, Keys: trusted, Run: run}
	if err := me.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("unsigned release: %v", err)
	}
	if _, err := me.Install(context.Background()); err == nil {
		t.Fatal("installed an unsigned release")
	}

	// stable with a Standard subscription
	lic, err := st.Issue(license.Standard, []string{server}, "Acme", "", 12, "")
	if err != nil {
		t.Fatal(err)
	}
	info = Channel{Allowed: []string{"stable", "testing"}, License: lic.License.ID, Server: server}
	if err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(context.Background()); s.Channel != "stable" || s.Available != nil {
		t.Fatalf("stable before promote: %+v", s)
	}
	if _, err := st.Promote("0.6.0"); err != nil {
		t.Fatal(err)
	}
	_ = m.Check(context.Background())
	if s := m.Status(context.Background()); s.Available == nil || s.Available.Version != "0.6.0" {
		t.Fatalf("stable after promote: %+v", s)
	}
	// a Home server pretending to have the licence of another server
	info = Channel{Allowed: []string{"stable"}, License: lic.License.ID, Server: "P8S-BBBB-0000-0000-0002"}
	if err := m.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "subscription") {
		t.Fatalf("borrowed licence: %v", err)
	}
}
