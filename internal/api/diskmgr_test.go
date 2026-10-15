package api

import (
	"context"
	"strings"
	"testing"

	"p8sos/internal/diskmgr"
)

type diskRun struct{ cmds []string }

func (f *diskRun) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, c)
	if name == "lsblk" && args[0] == "-J" {
		return []byte(`{"blockdevices":[{"name":"sdb","path":"/dev/sdb","type":"disk","size":500107862016,"model":"X","serial":"S2","tran":"sata","rota":false,"rm":false,"mountpoints":[null]},
{"name":"sda","path":"/dev/sda","type":"disk","size":1000204886016,"model":"HDD","serial":"W1","tran":"sata","rota":true,"rm":false,"mountpoints":[null],"children":[{"name":"sda1","path":"/dev/sda1","type":"part","fstype":"ext4","label":"data","mountpoints":[null]}]}]}`), nil
	}
	return nil, nil
}

func TestDiskMgrAPI(t *testing.T) {
	run := &diskRun{}
	dm := &diskmgr.Manager{Run: run, DataDir: t.TempDir(), ByID: func() map[string]string { return nil }}
	m := newMultiUserCfg(t, func(s *Server) { s.DiskMgr = dm })
	m.req("POST", "/api/v1/users", secret, `{"name":"ops","role":"operator","password":"operator-pass-1"}`, 201)
	ops := m.login("ops", "operator-pass-1", "")
	m.req("GET", "/api/v1/node/disks?smart=0", ops, "", 200)
	m.req("POST", "/api/v1/node/disks/sdb/wipe", ops, `{}`, 403)
	v := m.req("GET", "/api/v1/node/disks?smart=0", secret, "", 200)
	if l := v["disks"].([]any); len(l) != 2 || l[0].(map[string]any)["usage"] != "data" {
		t.Fatalf("%v", v)
	}
	r := m.req("POST", "/api/v1/node/disks/sda/wipe", secret, `{}`, 409)
	if r["error"].(map[string]any)["code"] != "confirm" {
		t.Fatalf("%v", r)
	}
	m.req("POST", "/api/v1/node/disks/sda/protect", secret, `{"on":true}`, 200)
	m.req("POST", "/api/v1/node/disks/sda/wipe", secret, `{"confirm":"sda"}`, 409)
	m.req("POST", "/api/v1/node/disks/nope/wipe", secret, `{}`, 404)
	m.req("POST", "/api/v1/node/pools", secret, `{"name":"fast","layout":"single","disks":["sda"]}`, 409)
	p := m.req("POST", "/api/v1/node/pools", secret, `{"name":"fast","layout":"single","disks":["sdb"]}`, 200)
	if p["pool"].(map[string]any)["pool"] != "p8s-fast" {
		t.Fatalf("%v", p)
	}
	m.req("DELETE", "/api/v1/node/pools/fast", secret, "", 409)
	m.req("DELETE", "/api/v1/node/pools/fast?confirm=fast", secret, "", 200)
	for _, c := range run.cmds {
		if strings.HasPrefix(c, "wipefs") && strings.Contains(c, "sda") {
			t.Fatal("touched sda")
		}
	}
}
