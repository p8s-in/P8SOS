package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/ct"
	"p8sos/internal/testfake"
)

func TestContainersAPI(t *testing.T) {
	dir := t.TempDir()
	run := &testfake.Runner{Out: map[string]string{
		"zfs list -H -t snapshot -o name,p8s:label": "tank/p8s/ct-templates/debian-13@base\tDebian 13\n",
		"lxc-info -n":               "RUNNING\n",
		"zfs get -Hp -o value used": "1000\n",
	}}
	cts := &ct.Service{Run: run, Pool: "tank", MountDir: filepath.Join(dir, "ct"), LXCDir: filepath.Join(dir, "lxc"),
		MetaFile: filepath.Join(dir, "containers.json"), Bridge: "virbr0", Chown: func(string, int, int) error { return nil }}
	t.Cleanup(cts.Wait) // background first-boot setup, before the temp dir goes
	for _, n := range []string{"c1", "c2", "c3"} {
		_ = os.MkdirAll(filepath.Join(dir, "ct", n), 0o755)
	}
	m := newMultiUserCfg(t, func(s *Server) { s.CTs = cts })
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password","quota":{"vms":2,"vcpus":2,"memory_mib":2048,"disk_gb":30}}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"bob-password-1"}`, 201)
	alice := m.login("alice", "alice-password", "")
	bob := m.login("bob", "bob-password-1", "")

	if v := m.req("GET", "/api/v1/ct-templates", alice, "", 200); len(v["templates"].([]any)) != 1 {
		t.Fatalf("templates %v", v)
	}
	m.req("POST", "/api/v1/containers", alice, `{"name":"web1","template":"debian-13"}`, 409) // a VM has that name
	// alice owns "shop" (a VM, 1 cpu/1 GB/10 GB); 2 more cpus would be over her plan
	m.req("POST", "/api/v1/containers", alice, `{"name":"c1","template":"debian-13","vcpus":2}`, 403)
	c := m.req("POST", "/api/v1/containers", alice, `{"name":"c1","template":"debian-13","owner":"bob","memory_mib":512,"disk_gb":8}`, 201)
	if c["owner"] != "alice" || c["kind"] != "ct" {
		t.Fatalf("created %v", c)
	}
	m.req("POST", "/api/v1/containers", alice, `{"name":"c2","template":"debian-13"}`, 403) // 3 servers > 2
	m.req("POST", "/api/v1/vms", secret, `{"name":"c1","template":"debian-13"}`, 409)       // container has that name

	// bob can't see or touch it; admin sees it
	if v := m.req("GET", "/api/v1/containers", bob, "", 200); len(v["containers"].([]any)) != 0 {
		t.Fatal("bob sees alice's container")
	}
	m.req("GET", "/api/v1/containers/c1", bob, "", 404)
	m.req("POST", "/api/v1/containers/c1/stop", bob, "", 404)
	m.req("DELETE", "/api/v1/containers/c1", bob, "", 404)
	if v := m.req("GET", "/api/v1/containers", secret, "", 200); len(v["containers"].([]any)) != 1 {
		t.Fatal("admin list")
	}

	// her usage counts the container
	if v := m.req("GET", "/api/v1/me", alice, "", 200); v["usage"].(map[string]any)["vms"].(float64) != 2 {
		t.Fatalf("usage %v", v["usage"])
	}
	m.req("PATCH", "/api/v1/containers/c1", alice, `{"vcpus":3}`, 403) // over quota
	m.req("PATCH", "/api/v1/containers/c1", alice, `{"memory_mib":1024}`, 200)
	m.req("POST", "/api/v1/containers/c1/snapshots", alice, `{"name":"s1"}`, 201)
	m.req("PUT", "/api/v1/containers/c1/owner", alice, `{"owner":"bob"}`, 403)
	m.req("PUT", "/api/v1/containers/c1/owner", secret, `{"owner":"nobody"}`, 404)

	// console ticket works only on the terminal endpoint
	tk := m.req("POST", "/api/v1/containers/c1/console", alice, "", 201)
	if !strings.HasPrefix(tk["path"].(string), "/api/v1/terminal?ticket=") {
		t.Fatalf("ticket %v", tk)
	}
	code, _ := do(t, m.srv, "GET", "/api/v1/console?ticket="+tk["ticket"].(string), "", "")
	if code == 101 || code == 200 {
		t.Fatal("container ticket opened a VM console")
	}
	code, _ = do(t, m.srv, "GET", "/api/v1/terminal?ticket=bogus", "", "")
	if code != 401 {
		t.Fatalf("bogus ticket %d", code)
	}

	// firewall and backups follow the same access rules
	m.req("PUT", "/api/v1/containers/c1/firewall", bob, `{"enabled":true,"rules":[]}`, 404)
	m.req("PUT", "/api/v1/containers/c1/firewall", alice, `{"enabled":true,"rules":[{"proto":"tcp","ports":"x"}]}`, 400)
	m.req("PUT", "/api/v1/containers/c1/firewall", alice, `{"enabled":true,"rules":[{"proto":"tcp","ports":"22"}]}`, 200)
	if v := m.req("GET", "/api/v1/containers/c1/firewall", alice, "", 200); v["enabled"] != true {
		t.Fatalf("firewall %v", v)
	}
	m.req("GET", "/api/v1/containers/c1/backups", bob, "", 404)
	if v := m.req("GET", "/api/v1/containers/c1/backups", alice, "", 200); len(v["backups"].([]any)) != 0 {
		t.Fatalf("backups %v", v)
	}
	m.req("POST", "/api/v1/containers/c1/backups", alice, "", 409) // no backup folder in this test
	m.req("POST", "/api/v1/ct-backups/c1/20261012-100000/restore", alice, `{"name":"c9"}`, 404)

	m.req("DELETE", "/api/v1/containers/c1", alice, "", 204)
	m.req("GET", "/api/v1/containers/c1", alice, "", 404)
}
