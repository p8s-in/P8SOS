package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"p8sos/internal/vm"
)

// isoVMs: the master's VMs with an ISO library of one file.
type isoVMs struct {
	*ownedVMs
	path string
}

func (v *isoVMs) ISOFile(name string) (string, int64, error) {
	if name != "debian.iso" {
		return "", 0, fmt.Errorf("%w: iso", vm.ErrNotFound)
	}
	st, err := os.Stat(v.path)
	if err != nil {
		return "", 0, err
	}
	return v.path, st.Size(), nil
}

// isoShelf: a worker's library.
type isoShelf struct {
	mu    sync.Mutex
	files map[string][]byte
	puts  int
	dels  []string
}

func (l *isoShelf) ISOFile(name string) (string, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.files[name]
	if !ok {
		return "", 0, fmt.Errorf("%w: no ISO", vm.ErrNotFound)
	}
	return "/iso/" + name, int64(len(b)), nil
}
func (l *isoShelf) ReplaceISO(_ context.Context, name string, r io.Reader) (vm.ISO, error) {
	b, err := io.ReadAll(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.files[name] = b
	l.puts++
	return vm.ISO{Name: name, Bytes: int64(len(b))}, err
}
func (l *isoShelf) DeleteISO(_ context.Context, name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.files, name)
	l.dels = append(l.dels, name)
	return nil
}

func TestISOInstallOnWorker(t *testing.T) {
	ctx := context.Background()
	iso := filepath.Join(t.TempDir(), "debian.iso")
	data := bytes.Repeat([]byte("CD001"), 100000)
	os.WriteFile(iso, data, 0o644)
	mOwned := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
	wOwned := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
	master := newClusterNode(t, 4096, &isoVMs{ownedVMs: mOwned, path: iso}, mOwned)
	worker := newClusterNode(t, 16384, wOwned, wOwned)
	shelf := &isoShelf{files: map[string][]byte{}}
	worker.s.LocalISOs = shelf

	b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
	code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
	worker.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	master.m.Poll(ctx)

	// an ISO install goes to the roomier worker now; the ISO is copied there first
	b = master.req(t, "POST", "/api/v1/vms", secret, `{"name":"inst1","iso":"debian.iso","memory_mib":2048,"disk_gb":20}`, 201)
	if !strings.Contains(b, `"node_id":"n2"`) {
		t.Fatalf("placed %s", b)
	}
	if !bytes.Equal(shelf.files["debian.iso"], data) || shelf.puts != 1 {
		t.Fatalf("iso not copied (%d puts)", shelf.puts)
	}
	// the second install doesn't copy again
	master.req(t, "POST", "/api/v1/vms", secret, `{"name":"inst2","iso":"debian.iso","memory_mib":2048,"disk_gb":20}`, 201)
	if shelf.puts != 1 {
		t.Fatalf("copied again (%d puts)", shelf.puts)
	}
	// an ISO the master doesn't have
	master.req(t, "POST", "/api/v1/vms", secret, `{"name":"inst3","iso":"nope.iso","node":"n2"}`, 400)
	// only the cluster master may use the copy endpoints
	worker.req(t, "PUT", "/api/v1/cluster/iso/x.iso", secret, "data", 403)
	// deleting on the master deletes the copies
	master.req(t, "DELETE", "/api/v1/isos/debian.iso", secret, "", 204)
	if len(shelf.dels) != 1 || shelf.dels[0] != "debian.iso" {
		t.Fatalf("copies not deleted: %v", shelf.dels)
	}
}
