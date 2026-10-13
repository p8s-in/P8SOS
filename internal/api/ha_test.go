package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/users"
	"p8sos/internal/vm"
)

// haVMs: fake VMs that can keep copies (a copy = the VM + its "disk" string).
type haVMs struct {
	*moverVMs
	rmu    sync.Mutex
	copies map[string]haCopy
	n      int
	starts []string
}

type haCopy struct {
	v     vm.VM
	data  string
	snaps []string
}

func newHAVMs(vms map[string]vm.VM, disks map[string]string) *haVMs {
	return &haVMs{moverVMs: &moverVMs{&ownedVMs{fakeVMs: &fakeVMs{}, vms: vms}, disks}, copies: map[string]haCopy{}}
}

func (h *haVMs) Start(ctx context.Context, n string) (vm.VM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vms[n]
	if !ok {
		return vm.VM{}, fmt.Errorf("%w: %s", vm.ErrNotFound, n)
	}
	v.State = "running"
	h.vms[n] = v
	h.starts = append(h.starts, n)
	return v, nil
}
func (h *haVMs) state(n string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vms[n]
	if !ok {
		return "gone"
	}
	return string(v.State)
}
func (h *haVMs) HACheck(ctx context.Context, n string) (string, error) {
	_, err := h.Get(ctx, n)
	return "", err
}
func (h *haVMs) ReplicaSnaps(_ context.Context, n string) ([]string, error) {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	return append([]string{}, h.copies[n].snaps...), nil
}
func (h *haVMs) SendReplica(ctx context.Context, n string, have []string, from string, w io.Writer) error {
	v, err := h.Get(ctx, n)
	if err != nil {
		return err
	}
	h.rmu.Lock()
	h.n++
	snap := fmt.Sprintf("p8s-rep-%d", h.n)
	h.rmu.Unlock()
	b, _ := json.Marshal(map[string]any{"vm": v, "snap": snap, "from": from})
	h.mu.Lock()
	d := h.disks[n]
	h.mu.Unlock()
	_, err = fmt.Fprintf(w, "%s\n%s", b, d)
	return err
}
func (h *haVMs) RecvReplica(ctx context.Context, r io.Reader) (vm.Replica, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return vm.Replica{}, err
	}
	var hd struct {
		VM   vm.VM  `json:"vm"`
		Snap string `json:"snap"`
		From string `json:"from"`
	}
	json.Unmarshal(line, &hd)
	d, _ := io.ReadAll(br)
	if _, err := h.Get(ctx, hd.VM.Name); err == nil {
		return vm.Replica{}, fmt.Errorf("%w: runs here", vm.ErrExists)
	}
	h.rmu.Lock()
	defer h.rmu.Unlock()
	h.copies[hd.VM.Name] = haCopy{hd.VM, string(d), []string{hd.Snap}}
	return vm.Replica{Name: hd.VM.Name, From: hd.From, Bytes: int64(len(d)), At: time.Now().UTC().Format(time.RFC3339)}, nil
}
func (h *haVMs) Replicas(context.Context) []vm.Replica {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	out := []vm.Replica{}
	for n, c := range h.copies {
		out = append(out, vm.Replica{Name: n, Bytes: int64(len(c.data))})
	}
	return out
}
func (h *haVMs) PromoteReplica(ctx context.Context, n string) (vm.VM, string, error) {
	h.rmu.Lock()
	c, ok := h.copies[n]
	delete(h.copies, n)
	h.rmu.Unlock()
	if !ok {
		return vm.VM{}, "", fmt.Errorf("%w: no copy", vm.ErrNotFound)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c.v.State = "stopped"
	c.v.Node, c.v.NodeID = "", ""
	h.vms[n], h.disks[n] = c.v, c.data
	return c.v, "", nil
}
func (h *haVMs) DropReplica(_ context.Context, n string) error {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	delete(h.copies, n)
	return nil
}
func (h *haVMs) RetireVM(ctx context.Context, n, from string) error {
	v, err := h.Get(ctx, n)
	if err != nil {
		return nil
	}
	h.mu.Lock()
	d := h.disks[n]
	delete(h.vms, n)
	delete(h.disks, n)
	h.mu.Unlock()
	h.rmu.Lock()
	h.copies[n] = haCopy{v, d, []string{"p8s-rep-old"}}
	h.rmu.Unlock()
	return nil
}
func (h *haVMs) Fence(ctx context.Context, names []string) []string {
	var out []string
	for _, n := range names {
		if h.state(n) == "running" {
			h.Stop(ctx, n, true)
			out = append(out, n)
		}
	}
	return out
}
func (h *haVMs) SetAutostart(ctx context.Context, n string, on bool) (vm.VM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vms[n]
	if !ok {
		return vm.VM{}, vm.ErrNotFound
	}
	v.Autostart = on
	h.vms[n] = v
	return v, nil
}
func (h *haVMs) copyOf(n string) (haCopy, bool) {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	c, ok := h.copies[n]
	return c, ok
}

// downable lets a test cut a server off (every request fails).
type downable struct {
	h    http.Handler
	down atomic.Bool
}

func (d *downable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.down.Load() {
		hj, ok := w.(http.Hijacker)
		if ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	d.h.ServeHTTP(w, r)
}

func TestClusterHA(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(d time.Duration) { clock.Add(int64(d)) }

	mv := newHAVMs(map[string]vm.VM{}, map[string]string{})
	w2 := newHAVMs(map[string]vm.VM{"web1": {Name: "web1", State: "running", MemoryMiB: 1024, Autostart: true}}, map[string]string{"web1": "disk v1"})
	w3 := newHAVMs(map[string]vm.VM{}, map[string]string{})
	master := newClusterNode(t, 8192, mv, mv.ownedVMs)
	n2 := newClusterNode(t, 8192, w2, w2.ownedVMs)
	n3 := newClusterNode(t, 16384, w3, w3.ownedVMs)
	var gw atomic.Bool
	gw.Store(true)
	var boot3 atomic.Int64
	boot3.Store(1000)
	has := map[*clusterNode]*cluster.HA{}
	downs := map[*clusterNode]*downable{}
	for n, fake := range map[*clusterNode]*haVMs{master: mv, n2: w2, n3: w3} {
		n.vms.Jobs = nil
		ha := &cluster.HA{C: n.vms, Local: fake, Dir: t.TempDir(), Now: now,
			SameLAN:   func(context.Context, cluster.Node) bool { return true },
			GatewayOK: func(context.Context) bool { return n != n3 || gw.Load() },
			BootTime:  func() int64 { return map[bool]int64{true: boot3.Load(), false: 7}[n == n3] }}
		n.s.HA, n.s.LocalHA = ha, fake
		has[n] = ha
		d := &downable{h: n.srv.Config.Handler}
		n.srv.Config.Handler = d
		downs[n] = d
	}
	// not clustered
	if b := master.req(t, "GET", "/api/v1/vms/x/ha", secret, "", 404); !strings.Contains(b, "not_found") {
		t.Fatal(b)
	}
	for _, w := range []*clusterNode{n2, n3} {
		b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
		code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
		w.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	}
	master.m.Poll(ctx)
	ha := has[master]
	tick := func() { master.m.Poll(ctx); ha.Tick(ctx) }

	b := master.req(t, "GET", "/api/v1/vms/web1/ha", secret, "", 200)
	if !strings.Contains(b, `"protected":false`) || !strings.Contains(b, `"id":"n3"`) || !strings.Contains(b, `"id":"n1"`) || strings.Contains(b, `"id":"n2"`) {
		t.Fatalf("info %s", b)
	}
	master.req(t, "PUT", "/api/v1/vms/web1/ha", secret, `{"enabled":true,"target":"n2"}`, 400)
	master.req(t, "PUT", "/api/v1/vms/web1/ha", secret, `{"enabled":true,"target":"n3","every_min":5}`, 200)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if ok() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out: " + what)
	}
	waitFor("first copy", func() bool { c, ok := w3.copyOf("web1"); return ok && c.data == "disk v1" })
	waitFor("copy recorded", func() bool { c, _ := ha.Config(ctx, "web1"); return c.LastCopy != "" && !c.Copying })
	if v, _ := w2.Get(ctx, "web1"); v.Autostart {
		t.Fatal("autostart still on: HA starts it")
	}
	master.req(t, "PUT", "/api/v1/vms/web1/autostart", secret, `{"autostart":true}`, 409)
	c, _ := ha.Config(ctx, "web1")
	if !c.Auto || c.Node != "n2" || c.TargetName == "" {
		t.Fatalf("config %+v", c)
	}

	// next copies come every 5 minutes
	w2.mu.Lock()
	w2.disks["web1"] = "disk v2"
	w2.mu.Unlock()
	tick()
	time.Sleep(30 * time.Millisecond)
	if c, _ := w3.copyOf("web1"); c.data != "disk v1" {
		t.Fatal("copied too early")
	}
	advance(6 * time.Minute)
	tick()
	waitFor("second copy", func() bool { c, _ := w3.copyOf("web1"); return c.data == "disk v2" })
	waitFor("idle", func() bool { c, _ := ha.Config(ctx, "web1"); return !c.Copying })

	// a crashed VM is started again
	w2.mu.Lock()
	v := w2.vms["web1"]
	v.State = "crashed"
	w2.vms["web1"] = v
	w2.mu.Unlock()
	tick()
	if w2.state("web1") != "running" {
		t.Fatal("crashed VM not restarted")
	}

	// n2 dies: after the failover time its VM starts from the copy on n3
	downs[n2].down.Store(true)
	tick()
	advance(100 * time.Second)
	tick()
	if w3.state("web1") != "gone" {
		t.Fatal("failed over too early")
	}
	advance(100 * time.Second)
	tick()
	if w3.state("web1") != "running" {
		t.Fatalf("not failed over: %+v", ha.Status(ctx).Events)
	}
	if b := master.req(t, "GET", "/api/v1/vms/web1", secret, "", 200); !strings.Contains(b, `"node_id":"n3"`) {
		t.Fatalf("not on n3: %s", b)
	}
	c, _ = ha.Config(ctx, "web1")
	if c.Node != "n3" || c.Target != "n2" || len(c.Retire) != 1 || c.FailedAt == "" {
		t.Fatalf("after failover %+v", c)
	}

	// n2 comes back (its old web1 still there, switched off): it becomes the copy, copies go n3 → n2
	downs[n2].down.Store(false)
	w2.Fence(ctx, []string{"web1"})
	w3.mu.Lock()
	w3.disks["web1"] = "disk v3 on n3"
	w3.mu.Unlock()
	advance(6 * time.Minute)
	tick()
	if w2.state("web1") != "gone" {
		t.Fatal("old VM still on n2")
	}
	waitFor("reverse copy", func() bool { c, _ := w2.copyOf("web1"); return c.data == "disk v3 on n3" })
	waitFor("idle", func() bool { c, _ := ha.Config(ctx, "web1"); return !c.Copying })
	if l, _ := master.vms.List(ctx); len(l) != 1 {
		t.Fatalf("list %+v", l)
	}

	// n3 is cut off from the master and its network: it fences web1; when the
	// master reaches it again it is started again (no failover happened)
	tick() // lease renewed
	downs[n3].down.Store(true)
	gw.Store(false)
	advance(70 * time.Second)
	has[n3].Tick(ctx)
	if w3.state("web1") != "stopped" {
		t.Fatal("not fenced")
	}
	if st := has[n3].Status(ctx); len(st.Events) == 0 || !strings.Contains(st.Events[0].Text, "switched off web1") {
		t.Fatalf("worker events %+v", st.Events)
	}
	downs[n3].down.Store(false)
	gw.Store(true)
	tick()
	if w3.state("web1") != "running" {
		t.Fatalf("fenced VM not restarted: %+v", ha.Status(ctx).Events)
	}

	// n3 reboots quickly (never seen offline): web1 was running, so HA starts it
	boot3.Store(2000)
	w3.Stop(ctx, "web1", true)
	tick()
	if w3.state("web1") != "running" {
		t.Fatalf("not restarted after reboot: %+v", ha.Status(ctx).Events[:2])
	}
	// stopped by hand: stays stopped
	w3.Stop(ctx, "web1", true)
	tick()
	tick()
	if w3.state("web1") != "stopped" {
		t.Fatal("restarted a VM that was shut down by hand")
	}
	w3.Start(ctx, "web1")
	tick()

	// manual failover is refused while the server is online
	master.req(t, "POST", "/api/v1/vms/web1/ha/failover", secret, "", 409)
	// the status page
	st := master.req(t, "GET", "/api/v1/ha", secret, "", 200)
	if !strings.Contains(st, `"name":"web1"`) || !strings.Contains(st, "started on") {
		t.Fatalf("status %s", st)
	}
	if b := n2.req(t, "GET", "/api/v1/ha", secret, "", 200); !strings.Contains(b, `"replicas":[{"name":"web1"`) {
		t.Fatalf("n2 copies %s", b)
	}
	// customers can't
	master.s.Users.Add("alice", users.Customer, "alice-password", users.Quota{VMs: 5, VCPUs: 8, MemoryMiB: 8192, DiskGB: 100})
	alice := loginOn(t, master, "alice", "alice-password")
	master.req(t, "PUT", "/api/v1/vms/web1/ha", alice, `{"enabled":false}`, 403)
	// off: the copy is deleted
	master.req(t, "PUT", "/api/v1/vms/web1/ha", secret, `{"enabled":false}`, 204)
	if _, ok := w2.copyOf("web1"); ok {
		t.Fatal("copy kept")
	}
}
