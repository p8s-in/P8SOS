package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/tasks"
	"p8sos/internal/testfake"
)

func TestNodeAPI(t *testing.T) {
	run := &testfake.Runner{Out: map[string]string{
		"zpool list": "tank\t193273528320\t21474836480\t171798691840\tONLINE\n",
		"/bin/bash /scripts/make-template.sh --list": "  debian-13      Debian 13 (trixie)\n  ubuntu-24.04   Ubuntu 24.04 LTS\n",
		"systemctl is-active":                        "active\n",
	}}
	m := newMultiUserCfg(t, func(s *Server) {
		s.Node = &tasks.Node{Tasks: &tasks.Manager{Dir: t.TempDir(), Scripts: "/scripts", Run: run}, Run: run, Pool: "tank",
			DataDir: t.TempDir(), Templates: func() []string { return []string{"debian-13"} }, LAN: func() bool { return false }}
	})
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	m.req("GET", "/api/v1/node", alice, "", 403)
	m.req("POST", "/api/v1/node/tasks", alice, `{"kind":"reboot"}`, 403)
	v := m.req("GET", "/api/v1/node", secret, "", 200)
	st := v["storage"].(map[string]any)
	if st["health"] != "ONLINE" || st["size"].(float64) != 193273528320 {
		t.Fatalf("storage %v", st)
	}
	cat := v["catalog"].([]any)
	if len(cat) != 2 || cat[0].(map[string]any)["installed"] != true || cat[1].(map[string]any)["installed"] != false {
		t.Fatalf("catalog %v", cat)
	}
	m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"template","arg":"debian-13"}`, 400) // already there
	m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"template","arg":"windows"}`, 400)
	m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"format-disk"}`, 400)
	tk := m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"template","arg":"ubuntu-24.04"}`, 202)
	m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"storage-grow"}`, 409) // one at a time
	g := m.req("GET", "/api/v1/node/tasks/"+tk["id"].(string), secret, "", 200)
	if g["state"] != "running" || g["by"] != "api-token:host" {
		t.Fatalf("task %v", g)
	}
	if v := m.req("GET", "/api/v1/node", secret, "", 200); v["running"] == nil {
		t.Fatal("running task not shown")
	}
}

func TestNodeCustomTemplates(t *testing.T) {
	run := &testfake.Runner{Out: map[string]string{
		"zpool list": "tank\t193273528320\t21474836480\t171798691840\tONLINE\n",
		"/bin/bash /scripts/make-template.sh --list":         "  debian-13      Debian 13 (trixie)\n  ubuntu-24.04   Ubuntu 24.04 LTS\n",
		"/bin/bash /scripts/make-ct-template.sh --list":      "  debian-13      Debian 13 (trixie)\n",
		"/bin/bash /scripts/make-ct-template.sh --available": "debian/trixie Debian trixie\nkali/current Kali current\nbad line\n",
		"systemctl is-active":                                "active\n",
	}}
	dir := t.TempDir()
	m := newMultiUserCfg(t, func(s *Server) {
		s.Node = &tasks.Node{Tasks: &tasks.Manager{Dir: dir, Scripts: "/scripts", Run: run}, Run: run, Pool: "tank",
			DataDir: t.TempDir(), Templates: func() []string { return []string{"debian-13"} },
			CTTemplates: func() []string { return []string{"debian-13"} }, LAN: func() bool { return false }}
	})
	v := m.req("GET", "/api/v1/node/ct-images", secret, "", 200)
	im := v["images"].([]any)
	if len(im) != 2 || im[0].(map[string]any)["installed"] != true || im[1].(map[string]any)["id"] != "kali/current" {
		t.Fatalf("images %v", im)
	}
	m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"ct-image","arg":"debian/trixie"}`, 400) // added already
	m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"ct-image","arg":"nope/1"}`, 400)
	m.req("POST", "/api/v1/node/tasks", secret, `{"kind":"template-url","arg":"x"}`, 400) // only via /templates/custom
	for _, bad := range []string{
		`{"name":"Bad Name","url":"https://example.com/a.qcow2"}`,
		`{"name":"mine","url":"ftp://example.com/a.qcow2"}`,
		`{"name":"mine","url":"https://example.com/a b.qcow2"}`,
		`{"name":"mine","url":"https://example.com/a.qcow2","checksum":"md5:abc"}`,
		`{"name":"mine","url":"https://example.com/a.qcow2","login":"Root!"}`,
		`{"name":"debian-13","url":"https://example.com/a.qcow2"}`,
		`{"name":"ubuntu-24.04","url":"https://example.com/a.qcow2"}`,
	} {
		m.req("POST", "/api/v1/node/templates/custom", secret, bad, 400)
	}
	tk := m.req("POST", "/api/v1/node/templates/custom", secret,
		`{"name":"kali","label":"Kali 2026","url":"https://kali.download/cloud/kali.qcow2.xz","login":"kali","checksum":"https://kali.download/cloud/SHA256SUMS"}`, 202)
	if tk["kind"] != "template-url" || tk["arg"] != "kali" {
		t.Fatalf("task %v", tk)
	}
	b, err := os.ReadFile(filepath.Join(dir, "template-kali.json"))
	if err != nil || !strings.Contains(string(b), `"url":"https://kali.download/cloud/kali.qcow2.xz"`) || !strings.Contains(string(b), `"login":"kali"`) {
		t.Fatalf("spec %s %v", b, err)
	}
	found := false
	for _, c := range run.Calls {
		found = found || strings.Contains(c, "make-template.sh --spec "+filepath.Join(dir, "template-kali.json")+" --register")
	}
	if !found {
		t.Fatalf("task command not run: %v", run.Calls)
	}
}
