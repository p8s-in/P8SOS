package api

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"p8sos/internal/tasks"
	"p8sos/internal/testfake"
)

func TestBootSecurityAPI(t *testing.T) {
	run := &testfake.Runner{}
	dir := t.TempDir()
	var mu sync.Mutex
	var calls []string
	var stdin string
	m := newMultiUserCfg(t, func(s *Server) {
		s.Node = &tasks.Node{Tasks: &tasks.Manager{Dir: dir, Scripts: "/scripts", Run: run}, Run: run, Pool: "tank", DataDir: t.TempDir()}
		s.BootSec = func(_ context.Context, in string, args ...string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, strings.Join(args, " "))
			if args[0] == "json" {
				return []byte("mokutil: warning\n" + `{"uefi":true,"secure_boot":"SecureBoot disabled","mok":"none","encrypted":true,"unlock_ssh":false,"unlock_keys":[]}` + "\n"), nil
			}
			stdin = in
			return []byte("ok"), nil
		}
	})
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"ops","role":"operator","password":"operator-pass-1"}`, 201)
	boss := m.login("boss", "boss-password-1", "")
	ops := m.login("ops", "operator-pass-1", "")
	if v := m.req("GET", "/api/v1/node/boot-security", boss, "", 200); v["encrypted"] != true || v["uefi"] != true {
		t.Fatalf("%v", v)
	}
	// operators may look but not change
	m.req("POST", "/api/v1/node/boot-security/secureboot", ops, "", 403)
	m.req("POST", "/api/v1/node/boot-security/unlock-keys", boss, `{"key":"not a key"}`, 400)
	tk := m.req("POST", "/api/v1/node/boot-security/unlock-keys", boss, `{"key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabcdefghijklmnop me@pc"}`, 202)
	if tk["kind"] != "boot-unlock-add" {
		t.Fatalf("%v", tk)
	}
	if b, _ := os.ReadFile(dir + "/boot-unlock.key"); !strings.HasPrefix(string(b), "ssh-ed25519 ") {
		t.Fatalf("key file %q", b)
	}
	if !strings.Contains(strings.Join(run.Calls, "\n"), "bootsec.sh unlock add-file "+dir+"/boot-unlock.key") {
		t.Fatalf("calls %v", run.Calls)
	}
	// passphrase: your own password first
	m.req("POST", "/api/v1/node/boot-security/passphrase", boss, `{"password":"wrong","passphrase":"new disk pass"}`, 403)
	m.req("POST", "/api/v1/node/boot-security/passphrase", boss, `{"password":"boss-password-1","passphrase":"short"}`, 400)
	m.req("POST", "/api/v1/node/boot-security/passphrase", ops, `{"password":"operator-pass-1","passphrase":"new disk pass"}`, 403)
	m.req("POST", "/api/v1/node/boot-security/passphrase", boss, `{"password":"boss-password-1","passphrase":"new disk pass"}`, 200)
	mu.Lock()
	defer mu.Unlock()
	if stdin != "new disk pass\n" || calls[len(calls)-1] != "passphrase-stdin" {
		t.Fatalf("stdin %q calls %v", stdin, calls)
	}
}
