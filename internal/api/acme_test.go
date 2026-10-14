package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/tasks"
	"p8sos/internal/testfake"
	"p8sos/internal/tlsutil"
)

func TestACMEAPI(t *testing.T) {
	run := &testfake.Runner{Out: map[string]string{"systemctl is-active": "active\n"}}
	dir, etc := t.TempDir(), t.TempDir()
	m := newMultiUserCfg(t, func(s *Server) {
		s.Node = &tasks.Node{Tasks: &tasks.Manager{Dir: dir, Scripts: "/scripts", Run: run}, Run: run, Pool: "tank", DataDir: t.TempDir()}
		s.CFIni = filepath.Join(etc, "cloudflare.ini")
		s.CertInfo = func() (tlsutil.ACMEInfo, bool) { return tlsutil.ACMEInfo{Names: []string{"p8s.example.com"}}, true }
	})
	if v := m.req("GET", "/api/v1/node/acme", secret, "", 200); v["cert"] == nil {
		t.Fatalf("%v", v)
	}
	m.req("POST", "/api/v1/node/acme", secret, `{"domain":"localhost"}`, 400)
	m.req("POST", "/api/v1/node/acme", secret, `{"domain":"p8s.example.com","method":"cloudflare"}`, 400) // no token yet
	m.req("POST", "/api/v1/node/acme", secret, `{"domain":"p8s.example.com","method":"ftp"}`, 400)
	m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"acme"}`, 400) // only through /node/acme
	m.req("POST", "/api/v1/node/acme", secret, `{"domain":"P8S.Example.com","email":"me@example.com","method":"cloudflare","cf_token":"abcdefghijklmnopqrstuvwxyz0123456789ABCD"}`, 202)
	if b, _ := os.ReadFile(filepath.Join(etc, "cloudflare.ini")); !strings.Contains(string(b), "dns_cloudflare_api_token = abcdef") {
		t.Fatalf("token file %q", b)
	}
	if v := m.req("GET", "/api/v1/node/acme", secret, "", 200); v["request"].(map[string]any)["domain"] != "p8s.example.com" {
		t.Fatalf("%v", v)
	}
	if !strings.Contains(strings.Join(run.Calls, "\n"), "acme.sh issue "+filepath.Join(dir, "acme.json")) {
		t.Fatalf("task not started: %v", run.Calls)
	}
}
