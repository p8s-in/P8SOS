package api

import (
	"os"
	"path/filepath"
	"testing"

	"p8sos/internal/creds"
	"p8sos/internal/ct"
	"p8sos/internal/testfake"
)

func TestLogins(t *testing.T) {
	dir := t.TempDir()
	run := &testfake.Runner{Out: map[string]string{
		"zfs list -H -t snapshot -o name,p8s:label": "tank/p8s/ct-templates/debian-13@base\tDebian 13\n",
		"lxc-info -n":               "RUNNING\n",
		"zfs get -Hp -o value used": "1000\n",
	}}
	cts := &ct.Service{Run: run, Pool: "tank", MountDir: filepath.Join(dir, "ct"), LXCDir: filepath.Join(dir, "lxc"),
		MetaFile: filepath.Join(dir, "containers.json"), Bridge: "virbr0", Chown: func(string, int, int) error { return nil }}
	t.Cleanup(cts.Wait) // background first-boot setup, before the temp dir goes
	_ = os.MkdirAll(filepath.Join(dir, "ct", "c1", "etc"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "ct", "c1", "etc/shadow"), []byte("root:*:19000:0:99999:7:::\n"), 0o640)
	store := &creds.Store{File: filepath.Join(dir, "credentials.json")}
	m := newMultiUserCfg(t, func(s *Server) { s.CTs, s.Creds, s.AutoPasswords = cts, store, true })
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password","quota":{"vms":5,"vcpus":8,"memory_mib":8192,"disk_gb":100}}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"bob-password-1"}`, 201)
	alice := m.login("alice", "alice-password", "")
	bob := m.login("bob", "bob-password-1", "")

	// a VM made without a password gets one; it is in the answer and at /login
	v := m.req("POST", "/api/v1/vms", alice, `{"name":"v1","template":"debian-13"}`, 201)
	lg, _ := v["login"].(map[string]any)
	pw, _ := lg["password"].(string)
	if v["name"] != "v1" || lg["user"] != "root" || len(pw) != PasswordLength || lg["ssh_password"] != true {
		t.Fatalf("create %v", v)
	}
	if l := m.req("GET", "/api/v1/vms/v1/login", alice, "", 200); l["password"] != pw || l["known"] != true {
		t.Fatalf("login %v", l)
	}
	m.req("GET", "/api/v1/vms/v1/login", bob, "", 404) // not his
	if l := m.req("GET", "/api/v1/vms/v1/login", secret, "", 200); l["password"] != pw {
		t.Fatal("admin can't see it")
	}
	// a password the user chose is kept as given
	m.req("POST", "/api/v1/vms", alice, `{"name":"v2","template":"debian-13","root_password":"my-own-pass","ssh_password_auth":false}`, 201)
	if l := m.req("GET", "/api/v1/vms/v2/login", alice, "", 200); l["password"] != "my-own-pass" || l["ssh_password"] != false {
		t.Fatalf("own %v", l)
	}
	// ISO installs set passwords in the installer
	if l := m.req("GET", "/api/v1/vms/web1/login", secret, "", 200); l["known"] != false {
		t.Fatalf("unknown %v", l)
	}

	// containers too
	c := m.req("POST", "/api/v1/containers", alice, `{"name":"c1","template":"debian-13"}`, 201)
	cl, _ := c["login"].(map[string]any)
	if c["name"] != "c1" || cl["user"] != "root" || len(cl["password"].(string)) != PasswordLength {
		t.Fatalf("ct create %v", c)
	}
	if l := m.req("GET", "/api/v1/containers/c1/login", alice, "", 200); l["password"] != cl["password"] {
		t.Fatalf("ct login %v", l)
	}
	m.req("GET", "/api/v1/containers/c1/login", bob, "", 404)

	// a new random password through the API is returned and remembered
	np := m.req("POST", "/api/v1/vms/web1/password", secret, `{"generate":true}`, 200)
	if np["user"] != "root" || len(np["password"].(string)) != PasswordLength {
		t.Fatalf("generate %v", np)
	}
	if l := m.req("GET", "/api/v1/vms/web1/login", secret, "", 200); l["password"] != np["password"] {
		t.Fatalf("after generate %v", l)
	}
	// deleting forgets it
	m.req("DELETE", "/api/v1/vms/v1", alice, "", 204)
	if _, ok := store.Get("vm", "v1"); ok {
		t.Fatal("kept after delete")
	}
}
