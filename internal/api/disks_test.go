package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"p8sos/internal/vm"
)

func (f *fakeVMs) AddDisk(ctx context.Context, n string, gb int) (vm.VM, string, error) {
	v, err := f.Get(ctx, n)
	if err != nil {
		return v, "", err
	}
	if gb < 1 {
		return vm.VM{}, "", fmt.Errorf("%w: size_gb must be 1-500", vm.ErrInvalid)
	}
	v.Disks = []vm.Disk{{Index: 1, SizeGB: gb, Device: "vdb"}}
	return v, "new empty disk vdb", nil
}
func (f *fakeVMs) ResizeDisk(ctx context.Context, n string, i, gb int) (vm.VM, error) {
	v, err := f.Get(ctx, n)
	if err == nil && i != 1 {
		err = fmt.Errorf("%w: no disk %d", vm.ErrNotFound, i)
	}
	return v, err
}
func (f *fakeVMs) RemoveDisk(ctx context.Context, n string, i int) (vm.VM, error) {
	return f.ResizeDisk(ctx, n, i, 0)
}

func TestDiskAPI(t *testing.T) {
	srv, _ := newTestServer()
	defer srv.Close()
	for _, c := range []struct {
		method, path, body string
		code               int
		contains           string
	}{
		{"POST", "/api/v1/vms/web1/disks", `{"size_gb":50}`, 201, `"note":"new empty disk vdb"`},
		{"POST", "/api/v1/vms/web1/disks", `{"size_gb":50}`, 201, `"disks":[{"index":1,"size_gb":50,"device":"vdb"}]`},
		{"POST", "/api/v1/vms/web1/disks", `{"size_gb":0}`, 400, "size_gb"},
		{"POST", "/api/v1/vms/nope/disks", `{"size_gb":5}`, 404, "not_found"},
		{"PATCH", "/api/v1/vms/web1/disks/1", `{"size_gb":60}`, 200, `"web1"`},
		{"PATCH", "/api/v1/vms/web1/disks/2", `{"size_gb":60}`, 404, "no disk 2"},
		{"PATCH", "/api/v1/vms/web1/disks/x", `{"size_gb":60}`, 404, "1-8"},
		{"DELETE", "/api/v1/vms/web1/disks/1", "", 200, `"web1"`},
		{"DELETE", "/api/v1/vms/web1/disks/9", "", 404, "1-8"},
	} {
		code, body := do(t, srv, c.method, c.path, secret, c.body)
		if code != c.code || !strings.Contains(body, c.contains) {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
}
