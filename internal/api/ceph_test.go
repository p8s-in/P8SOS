package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/ceph"
	"p8sos/internal/config"
	"p8sos/internal/installer"
	"p8sos/internal/testfake"
)

func TestCephAPI(t *testing.T) {
	dir := t.TempDir()
	installed := false
	m := &ceph.Manager{Run: &testfake.Runner{}, Etc: filepath.Join(dir, "etc"), Lib: filepath.Join(dir, "lib"), Name: "n1",
		Exists: func(p string) bool { return strings.HasPrefix(p, "/usr/") && installed },
		Addrs:  func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("192.168.1.11/24")} },
		Disks: func(context.Context) ([]installer.Disk, error) {
			return []installer.Disk{{Name: "sdb", Path: "/dev/sdb", Size: 5e11, Contents: []string{}}}, nil
		}}
	sum := sha256.Sum256([]byte(secret))
	s := &Server{VMs: &fakeVMs{}, Version: "test", CephLocal: m, Ceph: &ceph.Cluster{Local: m},
		Tokens: []config.Token{{Name: "t", SHA256: hex.EncodeToString(sum[:])}}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, c := range []struct {
		method, path, body string
		code               int
		contains           string
	}{
		{"GET", "/api/v1/ceph", "", 200, `"installed":false`},
		{"GET", "/api/v1/ceph", "", 200, `Install Ceph on this server`},
		{"POST", "/api/v1/ceph/init", `{"network":"192.168.1.0/24"}`, 409, "isn't installed"},
		{"POST", "/api/v1/ceph/mons", `{"node":""}`, 409, "set Ceph up first"},
		{"POST", "/api/v1/ceph/install", `{"node":"nope"}`, 400, "no server"},
		{"GET", "/api/v1/ceph/node", "", 200, `"name":"sdb"`},
		{"POST", "/api/v1/ceph/node/bogus", `{}`, 400, "unknown step"},
		{"POST", "/api/v1/ceph/node/config", `{"conf":"x","admin_keyring":"y","bootstrap_osd":"z"}`, 400, "not a Ceph configuration"},
		{"DELETE", "/api/v1/ceph/osds/x", "", 400, "bad OSD"},
		{"POST", "/api/v1/ceph/pool", `{}`, 409, "set Ceph up first"},
	} {
		code, body := do(t, srv, c.method, c.path, secret, c.body)
		if code != c.code || !strings.Contains(body, c.contains) {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
	installed = true
	if code, body := do(t, srv, "POST", "/api/v1/ceph/init", secret, `{"network":"10.9.0.0/16"}`); code != 400 || !strings.Contains(body, "no address") {
		t.Errorf("init on a network the server isn't in: %d %s", code, body)
	}
	if code, body := do(t, srv, "POST", "/api/v1/ceph/install", secret, `{}`); code != 409 || !strings.Contains(body, "already installed") {
		t.Errorf("install twice: %d %s", code, body)
	}
	// a worker answers for itself only
	s.Ceph = nil
	if code, body := do(t, srv, "GET", "/api/v1/ceph", secret, ""); code != 409 || !strings.Contains(body, "master") {
		t.Errorf("worker: %d %s", code, body)
	}
	if code, _ := do(t, srv, "GET", "/api/v1/ceph/node", secret, ""); code != 200 {
		t.Errorf("worker node info: %d", code)
	}
}
