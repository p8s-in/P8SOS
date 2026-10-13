package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"p8sos/internal/jobs"
	"p8sos/internal/users"
	"p8sos/internal/vm"
)

// moverVMs adds the move side to the fake VMs: the "disk" is a string.
type moverVMs struct {
	*ownedVMs
	disks map[string]string
}

func (m *moverVMs) MoveCheck(ctx context.Context, n string) (string, error) {
	v, err := m.Get(ctx, n)
	if err != nil {
		return "", err
	}
	if v.State != "stopped" {
		return "shut it down first", nil
	}
	return "", nil
}

func (m *moverVMs) ExportVM(ctx context.Context, n string, w io.Writer) error {
	v, err := m.Get(ctx, n)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	m.mu.Lock()
	d := m.disks[n]
	m.mu.Unlock()
	_, err = fmt.Fprintf(w, "%s\n%s", b, d)
	return err
}

func (m *moverVMs) ImportMovedVM(ctx context.Context, r io.Reader) (vm.VM, string, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return vm.VM{}, "", err
	}
	var v vm.VM
	if err := json.Unmarshal(line, &v); err != nil {
		return vm.VM{}, "", err
	}
	d, err := io.ReadAll(br)
	if err != nil {
		return vm.VM{}, "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vms[v.Name]; ok {
		return vm.VM{}, "", fmt.Errorf("%w: %s exists", vm.ErrConflict, v.Name)
	}
	v.State = "stopped"
	m.vms[v.Name] = v
	m.disks[v.Name] = string(d)
	return v, "", nil
}

func TestClusterMove(t *testing.T) {
	ctx := context.Background()
	mk := func(vms map[string]vm.VM, disks map[string]string) *moverVMs {
		return &moverVMs{&ownedVMs{fakeVMs: &fakeVMs{}, vms: vms}, disks}
	}
	mv := mk(map[string]vm.VM{"web1": {Name: "web1", State: "running", MemoryMiB: 2048, Owner: "alice"}}, map[string]string{"web1": "disk of web1"})
	wv := mk(map[string]vm.VM{}, map[string]string{})
	w2v := mk(map[string]vm.VM{}, map[string]string{})
	master := newClusterNode(t, 8192, mv, mv.ownedVMs)
	worker := newClusterNode(t, 8192, wv, wv.ownedVMs)
	worker2 := newClusterNode(t, 8192, w2v, w2v.ownedVMs)
	for _, n := range []*clusterNode{master, worker, worker2} {
		n.vms.Jobs = &jobs.Manager{}
		n.s.Migrate = n.vms
		n.s.LocalMoves = n.vms.Local.(*moverVMs)
	}

	// no cluster yet
	if b := master.req(t, "GET", "/api/v1/vms/web1/move", secret, "", 200); !strings.Contains(b, "needs a cluster") {
		t.Fatalf("info %s", b)
	}
	for _, w := range []*clusterNode{worker, worker2} {
		b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
		code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
		w.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	}
	master.m.Poll(ctx)

	// running: refused, with the reason
	b := master.req(t, "GET", "/api/v1/vms/web1/move", secret, "", 200)
	if !strings.Contains(b, "shut it down first") || !strings.Contains(b, `"id":"n2"`) || !strings.Contains(b, `"id":"n3"`) || strings.Contains(b, `"id":"n1"`) {
		t.Fatalf("info %s", b)
	}
	master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n2"}`, 409)
	// customers can't move
	master.s.Users.Add("alice", users.Customer, "alice-password", users.Quota{VMs: 5, VCPUs: 8, MemoryMiB: 8192, DiskGB: 100})
	alice := loginOn(t, master, "alice", "alice-password")
	master.req(t, "POST", "/api/v1/vms/web1/move", alice, `{"to":"n2"}`, 403)

	mv.Stop(ctx, "web1", true)
	master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n9"}`, 400)
	master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n1"}`, 400) // already there

	wait := func(body, want string) {
		t.Helper()
		var j0 jobs.Job
		json.Unmarshal([]byte(body), &j0)
		master.vms.Jobs.Wait()
		j, _ := master.vms.Jobs.Get(j0.ID)
		if j.State != "done" || !strings.Contains(j.Message, want) {
			t.Fatalf("job %+v", j)
		}
	}
	// master → worker
	wait(master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n2","start":true}`, 202), "moved to")
	if _, err := mv.Get(ctx, "web1"); err == nil {
		t.Fatal("original still on the master")
	}
	if v, err := wv.Get(ctx, "web1"); err != nil || v.Owner != "alice" || wv.disks["web1"] != "disk of web1" {
		t.Fatalf("on the worker %+v %v %q", v, err, wv.disks["web1"])
	}
	if b := master.req(t, "GET", "/api/v1/vms/web1", secret, "", 200); !strings.Contains(b, `"node_id":"n2"`) {
		t.Fatalf("not found on the worker %s", b)
	}
	// worker → worker (through the master)
	wv.Stop(ctx, "web1", true)
	wait(master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n3"}`, 202), "moved to")
	if _, err := wv.Get(ctx, "web1"); err == nil {
		t.Fatal("original still on worker 1")
	}
	if w2v.disks["web1"] != "disk of web1" {
		t.Fatal("not on worker 2")
	}
	// worker → master
	wait(master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n1"}`, 202), "moved to")
	if _, err := w2v.Get(ctx, "web1"); err == nil {
		t.Fatal("original still on worker 2")
	}
	if b := master.req(t, "GET", "/api/v1/vms/web1", secret, "", 200); !strings.Contains(b, `"node_id":"n1"`) || mv.disks["web1"] != "disk of web1" {
		t.Fatalf("back on the master %s", b)
	}
	// only the master's own tokens may stream a VM out of a worker? the export needs admin
	worker.req(t, "GET", "/api/v1/cluster/vm-export/web1", "", "", 401)
}
