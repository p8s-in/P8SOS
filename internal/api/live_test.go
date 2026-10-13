package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"p8sos/internal/firewall"
	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// liveVMs adds the live-move side to moverVMs; "the network" is a map of
// tunnel addresses, and a send hands the VM to the fake at that address.
type liveVMs struct {
	*moverVMs
	net      *sync.Map // tunnel ip -> *liveVMs
	prepared map[string]bool
	aborted  map[string]bool
	cleaned  map[string]bool
}

func (l *liveVMs) LiveCheck(ctx context.Context, n string) (string, error) {
	v, err := l.Get(ctx, n)
	if err != nil {
		return "", err
	}
	if v.State != "running" {
		return "it isn't running", nil
	}
	return "", nil
}
func (l *liveVMs) LiveHeader(ctx context.Context, n string) (vm.LiveHeader, error) {
	v, err := l.Get(ctx, n)
	if err != nil {
		return vm.LiveHeader{}, err
	}
	h := vm.LiveHeader{XML: "<domain>" + n + "</domain>", PersistentXML: "<domain/>", Disks: []vm.LiveDisk{{Index: 0, Bytes: 1 << 30, Path: "/dev/zvol/a"}}}
	h.Name, h.Owner, h.Autostart = n, v.Owner, true
	h.Firewall = &firewall.Settings{Enabled: true}
	return h, nil
}
func (l *liveVMs) LivePrepare(_ context.Context, h vm.LiveHeader) (vm.LivePlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.vms[h.Name]; ok {
		return vm.LivePlan{}, fmt.Errorf("%w: exists", vm.ErrExists)
	}
	l.prepared[h.Name] = true
	return vm.LivePlan{XML: strings.ReplaceAll(h.XML, "domain>", "domain>"), PersistentXML: h.PersistentXML}, nil
}
func (l *liveVMs) LiveSend(ctx context.Context, n, ip string, plan vm.LivePlan, progress func(int)) error {
	if n == "flaky" {
		return errors.New("connection to the other server lost")
	}
	t, ok := l.net.Load(ip)
	if !ok {
		return fmt.Errorf("no server at %s", ip)
	}
	dst := t.(*liveVMs)
	dst.mu.Lock()
	ready := dst.prepared[n]
	dst.mu.Unlock()
	if !ready || !strings.Contains(plan.XML, n) {
		return errors.New("target not prepared")
	}
	progress(40)
	progress(100)
	l.mu.Lock()
	v := l.vms[n]
	delete(l.vms, n)
	l.mu.Unlock()
	dst.mu.Lock()
	dst.vms[n] = v
	dst.mu.Unlock()
	return nil
}
func (l *liveVMs) LiveFinish(ctx context.Context, n string, auto bool, fw *firewall.Settings, _ *vm.Schedule) (vm.VM, string, error) {
	v, err := l.Get(ctx, n)
	if err == nil && (!auto || fw == nil || !fw.Enabled) {
		err = errors.New("settings lost on the way")
	}
	return v, "", err
}
func (l *liveVMs) LiveAbort(_ context.Context, n string) error {
	l.mu.Lock()
	l.aborted[n] = true
	l.mu.Unlock()
	return nil
}
func (l *liveVMs) LiveCleanup(_ context.Context, n string) error {
	l.mu.Lock()
	l.cleaned[n] = true
	l.mu.Unlock()
	return nil
}

func TestClusterLiveMove(t *testing.T) {
	ctx := context.Background()
	net := &sync.Map{}
	mk := func(vms map[string]vm.VM) *liveVMs {
		return &liveVMs{&moverVMs{&ownedVMs{fakeVMs: &fakeVMs{}, vms: vms}, map[string]string{}}, net,
			map[string]bool{}, map[string]bool{}, map[string]bool{}}
	}
	mv := mk(map[string]vm.VM{
		"web1":  {Name: "web1", State: "running", MemoryMiB: 2048, Owner: "alice"},
		"flaky": {Name: "flaky", State: "running", MemoryMiB: 512},
		"off1":  {Name: "off1", State: "stopped", MemoryMiB: 512},
	})
	wv, w2v := mk(map[string]vm.VM{}), mk(map[string]vm.VM{})
	master := newClusterNode(t, 8192, mv, mv.ownedVMs)
	worker := newClusterNode(t, 8192, wv, wv.ownedVMs)
	worker2 := newClusterNode(t, 8192, w2v, w2v.ownedVMs)
	for _, n := range []*clusterNode{master, worker, worker2} {
		n.vms.Jobs = &jobs.Manager{}
		n.s.Migrate = n.vms
		l := n.vms.Local.(*liveVMs)
		n.s.LocalMoves, n.s.LocalLive = l, l
	}
	for _, w := range []*clusterNode{worker, worker2} {
		b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
		code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
		w.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	}
	master.m.Poll(ctx)
	for n, l := range map[*clusterNode]*liveVMs{master: mv, worker: wv, worker2: w2v} {
		net.Store(n.m.State().Self.WGIP, l)
	}
	wait := func(body string) jobs.Job {
		t.Helper()
		var j0 jobs.Job
		json.Unmarshal([]byte(body), &j0)
		master.vms.Jobs.Wait()
		j, _ := master.vms.Jobs.Get(j0.ID)
		return j
	}

	b := master.req(t, "GET", "/api/v1/vms/web1/move", secret, "", 200)
	if !strings.Contains(b, `"running":true`) || strings.Contains(b, `"live_why"`) {
		t.Fatalf("info %s", b)
	}
	if b := master.req(t, "GET", "/api/v1/vms/off1/move", secret, "", 200); !strings.Contains(b, `"live_why":"it isn't running"`) {
		t.Fatalf("stopped vm info %s", b)
	}
	master.req(t, "POST", "/api/v1/vms/off1/move", secret, `{"to":"n2","live":true}`, 409)

	// master → worker, while running
	j := wait(master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n2","live":true}`, 202))
	if j.State != "done" || !strings.Contains(j.Message, "while running") {
		t.Fatalf("job %+v", j)
	}
	if v, err := wv.Get(ctx, "web1"); err != nil || v.State != "running" || !mv.cleaned["web1"] {
		t.Fatalf("on worker: %+v %v cleaned=%v", v, err, mv.cleaned)
	}
	if b := master.req(t, "GET", "/api/v1/vms/web1", secret, "", 200); !strings.Contains(b, `"node_id":"n2"`) {
		t.Fatalf("not tracked on the worker %s", b)
	}
	// worker → worker, then back to the master
	if j := wait(master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n3","live":true}`, 202)); j.State != "done" || !wv.cleaned["web1"] {
		t.Fatalf("worker to worker %+v", j)
	}
	if j := wait(master.req(t, "POST", "/api/v1/vms/web1/move", secret, `{"to":"n1","live":true}`, 202)); j.State != "done" {
		t.Fatalf("back %+v", j)
	}
	if v, err := mv.Get(ctx, "web1"); err != nil || v.State != "running" {
		t.Fatalf("back on master %+v %v", v, err)
	}
	// a failed send cleans up the target and leaves the VM where it was
	j = wait(master.req(t, "POST", "/api/v1/vms/flaky/move", secret, `{"to":"n2","live":true}`, 202))
	if j.State != "failed" || !strings.Contains(j.Error, "keeps running where it was") || !wv.aborted["flaky"] {
		t.Fatalf("failure %+v aborted=%v", j, wv.aborted)
	}
	if _, err := mv.Get(ctx, "flaky"); err != nil {
		t.Fatal("flaky should still be on the master")
	}
	// the node-level steps are for admins (the cluster token) only
	worker.req(t, "GET", "/api/v1/cluster/live-header/web1", "", "", 401)
}
