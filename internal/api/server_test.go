package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"p8sos/internal/config"
	"p8sos/internal/vm"
)

type fakeVMs struct{ created vm.CreateRequest }

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

const secret = "p8s_testtoken"

func newTestServer() (*httptest.Server, *fakeVMs) {
	sum := sha256.Sum256([]byte(secret))
	f := &fakeVMs{}
	s := &Server{VMs: f, Version: "test",
		Tokens:    []config.Token{{Name: "t", SHA256: hex.EncodeToString(sum[:])}},
		Templates: []config.Template{{Name: "debian-13", MinDiskGB: 4}}}
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
