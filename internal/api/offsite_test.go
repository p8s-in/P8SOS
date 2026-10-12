package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"p8sos/internal/backup"
	"p8sos/internal/jobs"
	"p8sos/internal/offsite"
)

func TestOffsiteAPI(t *testing.T) {
	dir := t.TempDir()
	js := &jobs.Manager{}
	off := &offsite.Manager{File: filepath.Join(dir, "offsite.json"), BackupDir: filepath.Join(dir, "backups"), KeyFile: filepath.Join(dir, "k"), Jobs: js}
	m := newMultiUserCfg(t, func(s *Server) { s.Offsite = off })
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	m.req("GET", "/api/v1/offsite", alice, "", 403)
	m.req("PUT", "/api/v1/offsite/targets/usb", alice, `{"type":"dir","path":"/mnt/x"}`, 403)

	usb := filepath.Join(dir, "usb")
	os.MkdirAll(usb, 0o700)
	m.req("PUT", "/api/v1/offsite/targets/usb", secret, `{"type":"dir","path":"relative"}`, 400)
	v := m.req("PUT", "/api/v1/offsite/targets/usb", secret, fmt.Sprintf(`{"type":"dir","path":%q,"enabled":true,"keep":3,"passphrase":"a long passphrase"}`, usb), 200)
	if v["passphrase"] != nil || v["has_passphrase"] != true || v["keep"] != 3.0 {
		t.Fatalf("saved %v", v)
	}
	m.req("POST", "/api/v1/offsite/targets/usb/test", secret, "", 200)
	m.req("POST", "/api/v1/offsite/targets/nope/test", secret, "", 404)

	// a local backup, copied by "sync", listed at the target
	st := &backup.Store{Dir: off.BackupDir}
	bm := backup.Meta{ID: "20261013-023000", VM: "web1", Compression: "zstd", CreatedAt: "2026-10-13T02:30:00Z"}
	data, meta := st.Files(bm)
	os.MkdirAll(filepath.Dir(data), 0o700)
	os.WriteFile(data, []byte("disk"), 0o600)
	b, _ := json.Marshal(bm)
	os.WriteFile(meta, b, 0o600)
	m.req("POST", "/api/v1/offsite/targets/usb/sync", secret, "", 202)
	js.Wait()
	l := m.req("GET", "/api/v1/offsite/targets/usb/backups", secret, "", 200)["backups"].([]any)
	if len(l) != 1 || l[0].(map[string]any)["encrypted"] != true || l[0].(map[string]any)["local"] != true {
		t.Fatalf("remote %v", l)
	}
	g := m.req("GET", "/api/v1/offsite", secret, "", 200)
	tg := g["targets"].([]any)[0].(map[string]any)
	if tg["status"].(map[string]any)["last_ok"] == nil {
		t.Fatalf("status %v", tg)
	}
	// fetch back once the local copy is gone
	m.req("POST", "/api/v1/offsite/targets/usb/fetch", secret, `{"kind":"vm","guest":"web1","id":"20261013-023000"}`, 202) // still local: the job refuses
	js.Wait()
	os.RemoveAll(filepath.Dir(data))
	m.req("POST", "/api/v1/offsite/targets/usb/fetch", secret, `{"kind":"vm","guest":"web1","id":"20261013-023000"}`, 202)
	js.Wait()
	if b, err := os.ReadFile(data); err != nil || string(b) != "disk" {
		t.Fatalf("fetched %q %v", b, err)
	}
	m.req("DELETE", "/api/v1/offsite/targets/usb", secret, "", 204)
}
