package storages

import (
	"context"
	"errors"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestGrowNFSPool(t *testing.T) {
	ctx := context.Background()
	r := &testfake.Runner{Out: map[string]string{"timeout 10 stat -f": "90000000 4096\n", "timeout 5 stat -f": "100000000 90000000 4096\n",
		"zpool list -H -p": "107374182400 100000000000 ONLINE\n"}, Fail: map[string]string{"zpool list -H p8s-nas": "no such pool"}}
	m, _ := newMgr(t, r)
	if _, err := m.Add(ctx, Storage{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "/x", VMDisks: true, SizeGB: 100}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Grow(ctx, "nas", 200); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "Connect") {
		t.Fatalf("pool closed: %v", err)
	}
	delete(r.Fail, "zpool list -H p8s-nas")
	if _, err := m.Grow(ctx, "nas", 100); !errors.Is(err, ErrInvalid) {
		t.Fatalf("not bigger: %v", err)
	}
	r.Out["timeout 10 stat -f"] = "2621440 4096\n" // 10 GB free
	if _, err := m.Grow(ctx, "nas", 200); err == nil || !strings.Contains(err.Error(), "10 GB free") {
		t.Fatalf("no room: %v", err)
	}
	r.Out["timeout 10 stat -f"] = "90000000 4096\n"
	if _, err := m.Grow(ctx, "nas", 200); err != nil {
		t.Fatal(err)
	}
	file := m.PoolFile("nas")
	calls := strings.Join(r.Calls, "\n")
	if !strings.Contains(calls, "truncate -s 200G "+file) || !strings.Contains(calls, "zpool online -e p8s-nas "+file) {
		t.Fatal(calls)
	}
	if s, _ := m.Get("nas"); s.SizeGB != 200 {
		t.Fatalf("%+v", s)
	}
	if _, err := m.Grow(ctx, "nope", 300); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
