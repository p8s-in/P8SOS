package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/audit"
	"p8sos/internal/config"
	"p8sos/internal/ippool"
	"p8sos/internal/tunnel"
	"p8sos/internal/users"
	"p8sos/internal/vm"
)

// ownedVMs is a fake VM service where VMs have owners.
type ownedVMs struct {
	*fakeVMs
	mu  sync.Mutex
	vms map[string]vm.VM
}

func (o *ownedVMs) Get(_ context.Context, n string) (vm.VM, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.vms[n]
	if !ok {
		return vm.VM{}, fmt.Errorf("%w: vm %q", vm.ErrNotFound, n)
	}
	return v, nil
}
func (o *ownedVMs) List(context.Context) ([]vm.VM, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := []vm.VM{}
	for _, v := range o.vms {
		out = append(out, v)
	}
	return out, nil
}
func (o *ownedVMs) Create(_ context.Context, r vm.CreateRequest) (vm.VM, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if r.DiskGB == 0 {
		r.DiskGB = 4
	}
	v := vm.VM{Name: r.Name, State: "running", Owner: r.Owner, VCPUs: max(r.VCPUs, 1), MemoryMiB: max(r.MemoryMiB, 1024), DiskGB: r.DiskGB, Network: r.Network}
	o.vms[r.Name] = v
	return v, nil
}
func (o *ownedVMs) ConsoleAddr(ctx context.Context, n string) (string, error) {
	_, err := o.Get(ctx, n)
	return "vnc", err
}
func (o *ownedVMs) Delete(ctx context.Context, n string) error {
	if _, err := o.Get(ctx, n); err != nil {
		return err
	}
	o.mu.Lock()
	delete(o.vms, n)
	o.mu.Unlock()
	return nil
}
func (o *ownedVMs) Start(ctx context.Context, n string) (vm.VM, error) { return o.Get(ctx, n) }
func (o *ownedVMs) SetOwner(ctx context.Context, n, owner string) (vm.VM, error) {
	v, err := o.Get(ctx, n)
	if err == nil {
		o.mu.Lock()
		v.Owner = owner
		o.vms[n] = v
		o.mu.Unlock()
	}
	return v, err
}
func (o *ownedVMs) ListBackups(n string) ([]vm.Backup, error) {
	return []vm.Backup{
		{ID: "20261012-023000", VM: "web1", Owner: ""},
		{ID: "20261012-023000", VM: "shop", Owner: "alice", VCPUs: 1, MemoryMiB: 1024, DiskGB: 10},
	}, nil
}
func (o *ownedVMs) StartRestore(ctx context.Context, n, id, newName string) (vm.Job, error) {
	return vm.Job{ID: "jr", Kind: "restore", Target: n}, nil
}
func (o *ownedVMs) ListJobs() []vm.Job {
	return []vm.Job{{ID: "j1", Target: "web1"}, {ID: "j2", Target: "shop"}}
}

type mu struct {
	t      *testing.T
	srv    *httptest.Server
	server *Server
	vms    *ownedVMs
	us     *users.Store
	now    time.Time
	log    *audit.Log
}

func newMultiUser(t *testing.T) *mu { return newMultiUserWith(t, nil) }

func newMultiUserWith(t *testing.T, tun *tunnel.Manager) *mu {
	return newMultiUserCfg(t, func(s *Server) { s.Tunnel = tun })
}

func newMultiUserCfg(t *testing.T, cfg func(*Server)) *mu {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte(secret))
	m := &mu{t: t, now: time.Unix(1_800_000_000, 0)}
	m.vms = &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{
		"web1": {Name: "web1", State: "running", VCPUs: 2, MemoryMiB: 2048, DiskGB: 20},
		"shop": {Name: "shop", State: "running", Owner: "alice", VCPUs: 1, MemoryMiB: 1024, DiskGB: 10},
	}}
	m.us = &users.Store{Path: filepath.Join(dir, "users.json"), Now: func() time.Time { return m.now }}
	m.log = &audit.Log{Path: filepath.Join(dir, "audit.log")}
	s := &Server{VMs: m.vms, Version: "test", Tokens: []config.Token{{Name: "host", SHA256: hex.EncodeToString(sum[:])}},
		Templates: []config.Template{{Name: "debian-13", MinDiskGB: 4}},
		Users:     m.us, Sessions: &users.Sessions{}, Audit: m.log, Limiter: &users.Limiter{Max: 3}, Pools: &ippool.Store{}}
	if cfg != nil {
		cfg(s)
	}
	m.server = s
	m.srv = httptest.NewServer(s.Handler())
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mu) req(method, path, tok, body string, want int) map[string]any {
	m.t.Helper()
	code, out := do(m.t, m.srv, method, path, tok, body)
	if code != want {
		m.t.Fatalf("%s %s: %d %s (want %d)", method, path, code, out, want)
	}
	var v map[string]any
	_ = json.Unmarshal([]byte(out), &v)
	return v
}

func (m *mu) login(user, pw, otp string) string {
	m.t.Helper()
	v := m.req("POST", "/api/v1/login", "", fmt.Sprintf(`{"user":%q,"password":%q,"otp":%q}`, user, pw, otp), 200)
	return v["token"].(string)
}

func TestMultiUser(t *testing.T) {
	m := newMultiUser(t)
	if v := m.req("GET", "/api/v1/auth", "", "", 200); v["accounts"] != false {
		t.Fatal("no accounts yet")
	}
	m.req("POST", "/api/v1/login", "", `{"user":"alice","password":"x"}`, 401)

	// the host's API token is an admin and makes accounts
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password","quota":{"vms":2,"vcpus":2,"memory_mib":2048,"disk_gb":20}}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"bob-password-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"bob-password-1"}`, 409)
	if v := m.req("GET", "/api/v1/auth", "", "", 200); v["accounts"] != true {
		t.Fatal("accounts exist now")
	}

	alice := m.login("alice", "alice-password", "")
	bob := m.login("bob", "bob-password-1", "")
	boss := m.login("boss", "boss-password-1", "")

	// alice sees only her VM; bob sees nothing; the admin sees all
	if v := m.req("GET", "/api/v1/vms", alice, "", 200); len(v["vms"].([]any)) != 1 {
		t.Fatalf("alice sees %v", v["vms"])
	}
	if v := m.req("GET", "/api/v1/vms", bob, "", 200); len(v["vms"].([]any)) != 0 {
		t.Fatalf("bob sees %v", v["vms"])
	}
	if v := m.req("GET", "/api/v1/vms", boss, "", 200); len(v["vms"].([]any)) != 2 {
		t.Fatalf("admin sees %v", v["vms"])
	}
	// someone else's VM looks like no VM
	for _, p := range []string{"GET /api/v1/vms/shop", "POST /api/v1/vms/shop/start", "POST /api/v1/vms/shop/console",
		"DELETE /api/v1/vms/shop", "GET /api/v1/vms/shop/snapshots", "GET /api/v1/vms/shop/screenshot"} {
		f := strings.SplitN(p, " ", 2)
		m.req(f[0], f[1], bob, "", 404)
	}
	m.req("GET", "/api/v1/vms/web1", alice, "", 404) // admin-owned
	m.req("PUT", "/api/v1/vms/shop/firewall", bob, `{"enabled":true,"rules":[]}`, 404)
	m.req("POST", "/api/v1/vms/shop/start", alice, "", 200)
	m.req("POST", "/api/v1/vms/shop/console", alice, "", 201)

	// admin-only things
	for _, p := range []string{"GET /api/v1/host", "GET /api/v1/host/metrics", "GET /api/v1/users", "DELETE /api/v1/isos/x.iso"} {
		f := strings.SplitN(p, " ", 2)
		m.req(f[0], f[1], alice, "", 403)
	}
	m.req("PUT", "/api/v1/vms/shop/owner", alice, `{"owner":"alice"}`, 403)

	// quota: alice has 1 of 2 VMs, 1 of 2 CPUs
	m.req("POST", "/api/v1/vms", alice, `{"name":"big","template":"debian-13","vcpus":2}`, 403)
	m.req("POST", "/api/v1/vms", alice, `{"name":"lan1","template":"debian-13","network":"lan"}`, 403)
	v := m.req("POST", "/api/v1/vms", alice, `{"name":"blog","template":"debian-13","owner":"bob"}`, 201)
	if v["owner"] != "alice" {
		t.Fatalf("customer VM owner %v", v["owner"])
	}
	m.req("POST", "/api/v1/vms", alice, `{"name":"third","template":"debian-13"}`, 403) // VMs 3 of 2
	me := m.req("GET", "/api/v1/me", alice, "", 200)
	if u := me["usage"].(map[string]any); u["vms"] != 2.0 || u["vcpus"] != 2.0 {
		t.Fatalf("usage %v", u)
	}
	// resize over the plan
	m.req("PATCH", "/api/v1/vms/shop", alice, `{"memory_mib":4096}`, 403)

	// admin creates for a customer and moves VMs
	v = m.req("POST", "/api/v1/vms", boss, `{"name":"forbob","template":"debian-13","owner":"bob"}`, 201)
	if v["owner"] != "bob" {
		t.Fatalf("admin-made VM owner %v", v["owner"])
	}
	m.req("POST", "/api/v1/vms", boss, `{"name":"x","template":"debian-13","owner":"nobody"}`, 404)
	m.req("PUT", "/api/v1/vms/web1/owner", boss, `{"owner":"bob"}`, 200)
	if v := m.req("GET", "/api/v1/vms", bob, "", 200); len(v["vms"].([]any)) != 2 {
		t.Fatalf("bob after transfer sees %v", v["vms"])
	}

	// backups and jobs by owner
	if v := m.req("GET", "/api/v1/backups", alice, "", 200); len(v["backups"].([]any)) != 1 {
		t.Fatalf("alice backups %v", v["backups"])
	}
	m.req("DELETE", "/api/v1/backups/web1/20261012-023000", alice, "", 404)
	m.req("POST", "/api/v1/backups/shop/20261012-023000/restore", alice, "", 202)
	m.req("POST", "/api/v1/backups/shop/20261012-023000/restore", bob, "", 404)
	if v := m.req("GET", "/api/v1/jobs", alice, "", 200); len(v["jobs"].([]any)) != 1 {
		t.Fatalf("alice jobs %v", v["jobs"])
	}

	// users can't be removed while they own VMs; the last admin is protected
	m.req("DELETE", "/api/v1/users/bob", boss, "", 409)
	m.req("PATCH", "/api/v1/users/boss", boss, `{"role":"customer"}`, 409)
	m.req("DELETE", "/api/v1/users/boss", secret, "", 409)

	// password change signs out other sessions
	alice2 := m.login("alice", "alice-password", "")
	m.req("POST", "/api/v1/me/password", alice, `{"old_password":"wrong","new_password":"alice-new-pass"}`, 403)
	m.req("POST", "/api/v1/me/password", alice, `{"old_password":"alice-password","new_password":"alice-new-pass"}`, 204)
	m.req("GET", "/api/v1/vms", alice, "", 200)
	m.req("GET", "/api/v1/vms", alice2, "", 401)

	// 2FA
	sv := m.req("POST", "/api/v1/me/2fa/setup", alice, "", 200)
	sec := sv["secret"].(string)
	code, _ := users.TOTPCode(sec, m.now)
	m.req("POST", "/api/v1/me/2fa/enable", alice, `{"code":"123456"}`, 400)
	m.req("POST", "/api/v1/me/2fa/enable", alice, fmt.Sprintf(`{"code":%q}`, code), 204)
	if v := m.req("POST", "/api/v1/login", "", `{"user":"alice","password":"alice-new-pass"}`, 401); v["error"].(map[string]any)["code"] != "otp_required" {
		t.Fatalf("want otp_required: %v", v)
	}
	m.now = m.now.Add(30 * time.Second)
	code, _ = users.TOTPCode(sec, m.now)
	m.login("alice", "alice-new-pass", code)
	m.req("POST", "/api/v1/me/2fa/setup", secret, "", 400) // API token has no account

	// disabling signs the user out at once
	m.req("PATCH", "/api/v1/users/bob", boss, `{"disabled":true}`, 200)
	m.req("GET", "/api/v1/vms", bob, "", 401)
	m.req("POST", "/api/v1/login", "", `{"user":"bob","password":"bob-password-1"}`, 401)

	// logout
	m.req("POST", "/api/v1/logout", boss, "", 204)
	m.req("GET", "/api/v1/vms", boss, "", 401)

	// lockout after repeated failures (limit 3 in this test)
	for i := 0; i < 3; i++ {
		m.req("POST", "/api/v1/login", "", `{"user":"boss","password":"bad-bad-bad"}`, 401)
	}
	m.req("POST", "/api/v1/login", "", `{"user":"boss","password":"boss-password-1"}`, 429)

	// audit: changes are recorded with who did them
	ents, _ := m.log.Recent(500, "")
	var sawCreate, sawFail bool
	for _, e := range ents {
		if e.User == "alice" && e.Action == "POST /api/v1/vms" && e.Status == 201 {
			sawCreate = true
		}
		if e.User == "boss" && e.Detail == "sign-in failed" {
			sawFail = true
		}
	}
	if !sawCreate || !sawFail {
		t.Fatalf("audit missing entries (create %v, failed sign-in %v)", sawCreate, sawFail)
	}
	if v := m.req("GET", "/api/v1/audit?limit=5", secret, "", 200); len(v["entries"].([]any)) != 5 {
		t.Fatal("audit limit")
	}
}
