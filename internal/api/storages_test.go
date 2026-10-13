package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/config"
	"p8sos/internal/offsite"
	"p8sos/internal/storages"
	"p8sos/internal/testfake"
)

func TestStoragesAPI(t *testing.T) {
	dir := t.TempDir()
	r := &testfake.Runner{Out: map[string]string{
		"iscsiadm -m discovery": "10.0.0.20:3260,1 iqn.2005-10.org.freenas.ctl:p8s\n",
	}, Fail: map[string]string{"zpool import": "no pools"}}
	m := &storages.Manager{Run: r, File: filepath.Join(dir, "s.json"), MountRoot: filepath.Join(dir, "mnt"),
		Fstab: filepath.Join(dir, "fstab"), Mounts: filepath.Join(dir, "mounts"), ByPath: dir, Wait: 1}
	sum := sha256.Sum256([]byte(secret))
	s := &Server{VMs: &fakeVMs{}, Version: "test", Storages: m,
		Offsite: &offsite.Manager{File: filepath.Join(dir, "offsite.json"), BackupDir: dir, KeyFile: filepath.Join(dir, "k")},
		Tokens:  []config.Token{{Name: "t", SHA256: hex.EncodeToString(sum[:])}}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	os.WriteFile(filepath.Join(dir, "ip-10.0.0.20:3260-iscsi-iqn.2005-10.org.freenas.ctl:p8s-lun-0"), nil, 0o600)
	for _, c := range []struct {
		method, path, body string
		code               int
		contains           string
	}{
		{"GET", "/api/v1/storages", "", 200, `"storages":[]`},
		{"POST", "/api/v1/storages", `{"name":"nas","type":"nfs","server":"10.0.0.5","export":"/mnt/tank/p8s","backups":true}`, 201, "copied there"},
		{"POST", "/api/v1/storages", `{"name":"nas","type":"nfs","server":"10.0.0.5","export":"/x"}`, 409, "exists"},
		{"POST", "/api/v1/storages", `{"name":"Bad","type":"nfs","server":"10.0.0.5","export":"/x"}`, 400, "name"},
		{"POST", "/api/v1/storages/discover", `{"portal":"10.0.0.20"}`, 200, `"targets":["iqn.2005-10.org.freenas.ctl:p8s"]`},
		{"POST", "/api/v1/storages", `{"name":"san","type":"iscsi","portal":"10.0.0.20","target":"iqn.2005-10.org.freenas.ctl:p8s","lun":0}`, 409, "confirm"},
		{"POST", "/api/v1/storages", `{"name":"san","type":"iscsi","portal":"10.0.0.20","target":"iqn.2005-10.org.freenas.ctl:p8s","lun":0,"format":true}`, 201, "Create"},
		{"GET", "/api/v1/storages", "", 200, `"name":"san"`},
		{"DELETE", "/api/v1/storages/nope", "", 404, "no such storage"},
		{"DELETE", "/api/v1/storages/nas", "", 204, ""},
		{"DELETE", "/api/v1/storages/san", "", 204, ""},
	} {
		code, body := do(t, srv, c.method, c.path, secret, c.body)
		if code != c.code || !strings.Contains(body, c.contains) {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
	if v, _ := s.Offsite.Targets(); len(v) != 0 {
		t.Fatalf("NAS backup target should go with the storage: %+v", v)
	}
}
