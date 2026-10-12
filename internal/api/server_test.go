package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"p8sos/internal/config"
	"p8sos/internal/metrics"
	"p8sos/internal/vm"
)

type fakeVMs struct {
	created vm.CreateRequest
	sched   vm.Schedule
}

func (f *fakeVMs) Create(_ context.Context, r vm.CreateRequest) (vm.VM, error) {
	if r.Name == "dup" {
		return vm.VM{}, fmt.Errorf("%w: dup", vm.ErrExists)
	}
	if r.Name == "" {
		return vm.VM{}, fmt.Errorf("%w: name required", vm.ErrInvalid)
	}
	f.created = r
	return vm.VM{Name: r.Name, State: "running"}, nil
}
func (f *fakeVMs) Get(_ context.Context, n string) (vm.VM, error) {
	if n != "web1" {
		return vm.VM{}, fmt.Errorf("%w: %s", vm.ErrNotFound, n)
	}
	return vm.VM{Name: n, State: "running"}, nil
}
func (f *fakeVMs) List(context.Context) ([]vm.VM, error)              { return []vm.VM{{Name: "web1"}}, nil }
func (f *fakeVMs) Start(ctx context.Context, n string) (vm.VM, error) { return f.Get(ctx, n) }
func (f *fakeVMs) Stop(ctx context.Context, n string, force bool) (vm.VM, error) {
	if !force {
		return vm.VM{}, fmt.Errorf("boom")
	}
	return f.Get(ctx, n)
}
func (f *fakeVMs) Reboot(ctx context.Context, n string) (vm.VM, error) { return f.Get(ctx, n) }
func (f *fakeVMs) Delete(ctx context.Context, n string) error          { _, err := f.Get(ctx, n); return err }

func (f *fakeVMs) Resize(ctx context.Context, n string, r vm.ResizeRequest) (vm.VM, error) {
	if r.DiskGB == 1 {
		return vm.VM{}, fmt.Errorf("%w: disk_gb can only grow", vm.ErrInvalid)
	}
	v, err := f.Get(ctx, n)
	v.VCPUs = r.VCPUs
	return v, err
}
func (f *fakeVMs) Snapshots(ctx context.Context, n string) ([]vm.Snapshot, error) {
	_, err := f.Get(ctx, n)
	return []vm.Snapshot{{Name: "s1"}}, err
}
func (f *fakeVMs) CreateSnapshot(ctx context.Context, n, snap string) (vm.Snapshot, error) {
	if snap == "" {
		snap = "snap-auto"
	}
	_, err := f.Get(ctx, n)
	return vm.Snapshot{Name: snap}, err
}
func (f *fakeVMs) Rollback(ctx context.Context, n, snap string, destroyNewer bool) (vm.VM, error) {
	if !destroyNewer {
		return vm.VM{}, fmt.Errorf("%w: newer snapshots exist", vm.ErrConflict)
	}
	return f.Get(ctx, n)
}
func (f *fakeVMs) DeleteSnapshot(ctx context.Context, n, snap string) error {
	if snap != "s1" {
		return fmt.Errorf("%w: snapshot", vm.ErrNotFound)
	}
	return nil
}

func (f *fakeVMs) ConsoleAddr(ctx context.Context, n string) (string, error) {
	_, err := f.Get(ctx, n)
	return "vnc-of-" + n, err
}
func (f *fakeVMs) SetPassword(ctx context.Context, n, user, pw string) error {
	if len(pw) < 8 {
		return fmt.Errorf("%w: password too short", vm.ErrInvalid)
	}
	_, err := f.Get(ctx, n)
	return err
}

func (f *fakeVMs) StartBackup(ctx context.Context, n string) (vm.Job, error) {
	_, err := f.Get(ctx, n)
	return vm.Job{ID: "j1", Kind: "backup", Target: n, State: "running"}, err
}
func (f *fakeVMs) ListBackups(n string) ([]vm.Backup, error) {
	return []vm.Backup{{ID: "20261012-023000", VM: "web1"}}, nil
}
func (f *fakeVMs) DeleteBackup(n, id string) error {
	if id != "20261012-023000" {
		return fmt.Errorf("%w: backup", vm.ErrNotFound)
	}
	return nil
}
func (f *fakeVMs) StartRestore(ctx context.Context, n, id, newName string) (vm.Job, error) {
	if newName == "" {
		return vm.Job{}, fmt.Errorf("%w: stop vm first", vm.ErrConflict)
	}
	return vm.Job{ID: "j2", Kind: "restore", Target: newName}, nil
}
func (f *fakeVMs) GetSchedule(n string) (vm.Schedule, error) { return f.sched, nil }
func (f *fakeVMs) SetSchedule(n string, s vm.Schedule) error {
	if s.Keep > 365 {
		return fmt.Errorf("%w: keep", vm.ErrInvalid)
	}
	f.sched = s
	return nil
}
func (f *fakeVMs) Job(id string) (vm.Job, bool) { return vm.Job{ID: id, State: "done"}, id == "j1" }
func (f *fakeVMs) ListJobs() []vm.Job           { return []vm.Job{{ID: "j1"}} }

func (f *fakeVMs) ListISOs() ([]vm.ISO, error) { return []vm.ISO{{Name: "debian.iso", Bytes: 5}}, nil }
func (f *fakeVMs) SaveISO(name string, r io.Reader) (vm.ISO, error) {
	b, _ := io.ReadAll(r)
	if string(b) != "ISODATA" {
		return vm.ISO{}, fmt.Errorf("%w: not an iso", vm.ErrInvalid)
	}
	return vm.ISO{Name: name, Bytes: int64(len(b))}, nil
}
func (f *fakeVMs) StartISODownload(ctx context.Context, url, name, sum string) (vm.Job, error) {
	return vm.Job{ID: "j3", Kind: "download", Target: "iso:" + name}, nil
}
func (f *fakeVMs) DeleteISO(ctx context.Context, name string) error {
	if name == "inuse.iso" {
		return fmt.Errorf("%w: in use", vm.ErrConflict)
	}
	return nil
}
func (f *fakeVMs) Screenshot(ctx context.Context, n string) ([]byte, error) {
	_, err := f.Get(ctx, n)
	return []byte("\x89PNGfake"), err
}
func (f *fakeVMs) EjectISO(ctx context.Context, n string) (vm.VM, error) { return f.Get(ctx, n) }

type fakeMetrics struct{}

func (fakeMetrics) VM(string) []metrics.Point { return []metrics.Point{{T: 1, CPU: 12.5}} }
func (fakeMetrics) Host() []metrics.HostPoint {
	return []metrics.HostPoint{{T: 1, CPU: 3, MemUsedMiB: 100, MemTotalMiB: 8000}}
}

const secret = "p8s_testtoken"

func newTestServer() (*httptest.Server, *fakeVMs) {
	sum := sha256.Sum256([]byte(secret))
	f := &fakeVMs{}
	s := &Server{VMs: f, Version: "test", BackupKeep: 7, BackupTime: "02:30", Metrics: fakeMetrics{},
		Tokens:    []config.Token{{Name: "t", SHA256: hex.EncodeToString(sum[:])}},
		Templates: []config.Template{{Name: "debian-13", MinDiskGB: 4}, {Name: "ubuntu-24.04", MinDiskGB: 4, Label: "Ubuntu 24.04 LTS", DefaultUser: "ubuntu"}}}
	return httptest.NewServer(s.Handler()), f
}

func do(t *testing.T, srv *httptest.Server, method, path, tok, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAPI(t *testing.T) {
	srv, f := newTestServer()
	defer srv.Close()

	cases := []struct {
		method, path, tok, body string
		code                    int
		contains                string
	}{
		{"GET", "/api/v1/health", "", "", 200, `"status":"ok"`},
		{"GET", "/api/v1/vms", "", "", 401, "unauthorized"},
		{"GET", "/api/v1/vms", "wrong", "", 401, "unauthorized"},
		{"GET", "/api/v1/vms", secret, "", 200, `"name":"web1"`},
		{"GET", "/api/v1/templates", secret, "", 200, `"debian-13"`},
		{"GET", "/api/v1/templates", secret, "", 200, `"label":"Ubuntu 24.04 LTS","min_disk_gb":4,"os_variant":"","default_user":"ubuntu"`},
		{"GET", "/api/v1/templates", secret, "", 200, `"name":"debian-13","label":"debian-13","min_disk_gb":4,"os_variant":"","default_user":"debian"`},
		{"POST", "/api/v1/vms", secret, `{"name":"api1","template":"debian-13","vcpus":2,"ssh_keys":["ssh-ed25519 AAAA"]}`, 201, `"name":"api1"`},
		{"POST", "/api/v1/vms", secret, `{"name":"dup","template":"debian-13"}`, 409, `"exists"`},
		{"POST", "/api/v1/vms", secret, `{"template":"debian-13"}`, 400, `"invalid"`},
		{"POST", "/api/v1/vms", secret, `{"name":"x","bogus":1}`, 400, "unknown field"},
		{"POST", "/api/v1/vms", secret, `{not json`, 400, "bad_request"},
		{"GET", "/api/v1/vms/web1", secret, "", 200, `"state":"running"`},
		{"GET", "/api/v1/vms/nope", secret, "", 404, "not_found"},
		{"POST", "/api/v1/vms/web1/start", secret, "", 200, "web1"},
		{"POST", "/api/v1/vms/web1/stop", secret, `{"force":true}`, 202, "web1"},
		{"POST", "/api/v1/vms/web1/stop", secret, "", 500, "host error: boom"},
		{"POST", "/api/v1/vms/web1/reboot", secret, "", 202, "web1"},
		{"POST", "/api/v1/vms/nope/reboot", secret, "", 404, "not_found"},
		{"DELETE", "/api/v1/vms/web1", secret, "", 204, ""},
		{"DELETE", "/api/v1/vms/nope", secret, "", 404, "not_found"},
		{"PUT", "/api/v1/vms/web1", secret, "", 405, ""},
		{"PATCH", "/api/v1/vms/web1", secret, `{"vcpus":4}`, 200, `"vcpus":4`},
		{"PATCH", "/api/v1/vms/web1", secret, `{"disk_gb":1}`, 400, "only grow"},
		{"PATCH", "/api/v1/vms/web1", secret, `{"cpus":4}`, 400, "unknown field"},
		{"GET", "/api/v1/vms/web1/snapshots", secret, "", 200, `"name":"s1"`},
		{"GET", "/api/v1/vms/nope/snapshots", secret, "", 404, "not_found"},
		{"POST", "/api/v1/vms/web1/snapshots", secret, `{"name":"before-upgrade"}`, 201, `"before-upgrade"`},
		{"POST", "/api/v1/vms/web1/snapshots", secret, "", 201, `"snap-auto"`},
		{"POST", "/api/v1/vms/web1/snapshots/s1/rollback", secret, "", 409, "conflict"},
		{"POST", "/api/v1/vms/web1/snapshots/s1/rollback", secret, `{"destroy_newer":true}`, 200, "web1"},
		{"DELETE", "/api/v1/vms/web1/snapshots/s1", secret, "", 204, ""},
		{"DELETE", "/api/v1/vms/web1/snapshots/zz", secret, "", 404, "not_found"},
		{"GET", "/api/v1/vms/web1/snapshots", "", "", 401, "unauthorized"},
		{"POST", "/api/v1/vms/web1/password", secret, `{"password":"longenough"}`, 204, ""},
		{"POST", "/api/v1/vms/web1/password", secret, `{"password":"short"}`, 400, "too short"},
		{"POST", "/api/v1/vms/nope/console", secret, "", 404, "not_found"},
		{"POST", "/api/v1/vms/web1/console", "", "", 401, "unauthorized"},
		{"GET", "/api/v1/console?ticket=bogus", "", "", 401, "ticket"},
		{"POST", "/api/v1/vms/web1/backups", secret, "", 202, `"kind":"backup"`},
		{"POST", "/api/v1/vms/nope/backups", secret, "", 404, "not_found"},
		{"GET", "/api/v1/vms/web1/backups", secret, "", 200, `"20261012-023000"`},
		{"GET", "/api/v1/backups", secret, "", 200, `"vm":"web1"`},
		{"DELETE", "/api/v1/backups/web1/20261012-023000", secret, "", 204, ""},
		{"DELETE", "/api/v1/backups/web1/20990101-000000", secret, "", 404, "not_found"},
		{"POST", "/api/v1/backups/web1/20261012-023000/restore", secret, "", 409, "stop vm"},
		{"POST", "/api/v1/backups/web1/20261012-023000/restore", secret, `{"name":"web1-copy"}`, 202, `"target":"web1-copy"`},
		{"GET", "/api/v1/vms/web1/backup-schedule", secret, "", 200, `"daily":false,"keep":7,"at":"02:30"`},
		{"PUT", "/api/v1/vms/web1/backup-schedule", secret, `{"daily":true}`, 200, `"daily":true,"keep":7`},
		{"PUT", "/api/v1/vms/web1/backup-schedule", secret, `{"daily":true,"keep":999}`, 400, "keep"},
		{"PUT", "/api/v1/vms/nope/backup-schedule", secret, `{"daily":true}`, 404, "not_found"},
		{"GET", "/api/v1/jobs", secret, "", 200, `"id":"j1"`},
		{"GET", "/api/v1/jobs/j1", secret, "", 200, `"state":"done"`},
		{"GET", "/api/v1/jobs/zz", secret, "", 404, "not_found"},
		{"GET", "/api/v1/vms/web1/metrics", secret, "", 200, `"points":[{"t":1,"cpu":12.5`},
		{"GET", "/api/v1/vms/nope/metrics", secret, "", 404, "not_found"},
		{"GET", "/api/v1/host/metrics", secret, "", 200, `"mem_total_mib":8000`},
		{"GET", "/api/v1/isos", secret, "", 200, `"name":"debian.iso"`},
		{"PUT", "/api/v1/isos/new.iso", secret, "ISODATA", 201, `"bytes":7`},
		{"PUT", "/api/v1/isos/new.iso", secret, "junk", 400, "not an iso"},
		{"PUT", "/api/v1/isos/new.iso", "", "ISODATA", 401, "unauthorized"},
		{"POST", "/api/v1/isos/download", secret, `{"url":"https://example.org/a.iso","name":"a.iso"}`, 202, `"kind":"download"`},
		{"DELETE", "/api/v1/isos/inuse.iso", secret, "", 409, "in use"},
		{"DELETE", "/api/v1/isos/old.iso", secret, "", 204, ""},
		{"POST", "/api/v1/vms/web1/eject", secret, "", 200, "web1"},
		{"GET", "/api/v1/vms/web1/screenshot", secret, "", 200, "PNGfake"},
		{"GET", "/api/v1/vms/nope/screenshot", secret, "", 404, "not_found"},
	}
	for _, c := range cases {
		code, body := do(t, srv, c.method, c.path, c.tok, c.body)
		if code != c.code || !strings.Contains(body, c.contains) {
			t.Errorf("%s %s: got %d %s; want %d containing %q", c.method, c.path, code, body, c.code, c.contains)
		}
	}
	if f.created.VCPUs != 2 || len(f.created.SSHKeys) != 1 {
		t.Errorf("create request not passed through: %+v", f.created)
	}
}

func TestHostAndUI(t *testing.T) {
	sum := sha256.Sum256([]byte(secret))
	s := &Server{VMs: &fakeVMs{}, Tokens: []config.Token{{Name: "t", SHA256: hex.EncodeToString(sum[:])}},
		Host: func(context.Context) any { return map[string]int{"cpus": 8} },
		UI:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("dashboard")) })}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if code, body := do(t, srv, "GET", "/api/v1/host", secret, ""); code != 200 || !strings.Contains(body, `"cpus":8`) {
		t.Fatalf("host: %d %s", code, body)
	}
	if code, _ := do(t, srv, "GET", "/api/v1/host", "", ""); code != 401 {
		t.Fatalf("host without token: %d", code)
	}
	if code, body := do(t, srv, "GET", "/", "", ""); code != 200 || body != "dashboard" {
		t.Fatalf("ui: %d %s", code, body)
	}
}

func TestConsoleWebSocket(t *testing.T) {
	// A fake VNC server: greets like QEMU, then echoes.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = c.Write([]byte("RFB 003.008\n")); _, _ = io.Copy(c, c) }()
		}
	}()
	sum := sha256.Sum256([]byte(secret))
	var dialed string
	s := &Server{VMs: &fakeVMs{}, Version: "test", Tokens: []config.Token{{Name: "t", SHA256: hex.EncodeToString(sum[:])}},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			dialed = addr
			return net.Dial("tcp", l.Addr().String())
		}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, body := do(t, srv, "POST", "/api/v1/vms/web1/console", secret, "")
	if code != 201 || !strings.Contains(body, `"path":"/api/v1/console?ticket=`) {
		t.Fatalf("ticket: %d %s", code, body)
	}
	var tk struct{ Path string }
	_ = json.Unmarshal([]byte(body), &tk)

	open := func(path, origin string) (net.Conn, *bufio.Reader, *http.Response) {
		c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		host := strings.TrimPrefix(srv.URL, "http://")
		req := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: MDEyMzQ1Njc4OWFiY2RlZg==\r\nSec-WebSocket-Protocol: binary\r\n"
		if origin != "" {
			req += "Origin: " + origin + "\r\n"
		}
		_, _ = io.WriteString(c, req+"\r\n")
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c, br, resp
	}

	// Another site's page can't use a ticket.
	c, _, resp := open(tk.Path, "https://evil.example")
	c.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", resp.StatusCode)
	}
	c, br, resp := open(tk.Path, srv.URL)
	defer c.Close()
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Protocol") != "binary" {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	if dialed != "vnc-of-web1" {
		t.Fatalf("dialed %q", dialed)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 0x82 {
		t.Fatalf("frame header %x %v", hdr, err)
	}
	greet := make([]byte, hdr[1])
	_, _ = io.ReadFull(br, greet)
	if string(greet) != "RFB 003.008\n" {
		t.Fatalf("greeting %q", greet)
	}
	// The ticket was used up.
	c2, _, resp2 := open(tk.Path, srv.URL)
	c2.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused ticket: %d", resp2.StatusCode)
	}
}
