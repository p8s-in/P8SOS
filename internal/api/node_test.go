package api

import (
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
