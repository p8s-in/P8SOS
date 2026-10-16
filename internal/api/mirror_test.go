package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/storage"
	"p8sos/internal/vm"
)

// mirrorVMs: haVMs that can mirror (a fake of vm/mirror.go: the mirror is in
// sync after one look; then the copy holds the disk as it is).
type mirrorVMs struct {
	*haVMs
	mmu     sync.Mutex
	jobs    map[string]*fakeJob
	synced  int
	prepped []string
}

type fakeJob struct {
	to    *mirrorVMs
	looks int
}

var mirrorHosts = struct {
	sync.Mutex
	m map[string]*mirrorVMs
}{m: map[string]*mirrorVMs{}}

func newMirrorVMs(h *haVMs) *mirrorVMs { return &mirrorVMs{haVMs: h, jobs: map[string]*fakeJob{}} }

func (m *mirrorVMs) MirrorCheck(ctx context.Context, n string) (string, error) {
	return m.HACheck(ctx, n)
}
func (m *mirrorVMs) MirrorPlan(ctx context.Context, n string) (vm.MirrorPlan, error) {
	if _, err := m.Get(ctx, n); err != nil {
		return vm.MirrorPlan{}, err
	}
	return vm.MirrorPlan{Header: vm.MoveHeader{Name: n}, Disks: []storage.MirrorDisk{{Index: 0, Bytes: 1 << 30}}}, nil
}
func (m *mirrorVMs) MirrorStatus(ctx context.Context, n string) (vm.MirrorState, error) {
	if m.state(n) != "running" {
		return vm.MirrorState{}, nil
	}
	m.mmu.Lock()
	defer m.mmu.Unlock()
	j := m.jobs[n]
	d := vm.MirrorDiskState{Index: 0, Len: 1 << 30}
	if j != nil {
		j.looks++
		d.Job, d.Synced, d.Done = true, j.looks > 1, 1<<30
		if d.Synced { // every write is there: the copy is the disk
			m.mu.Lock()
			v, data := m.vms[n], m.disks[n]
			m.mu.Unlock()
			j.to.rmu.Lock()
			v.State = "stopped"
			j.to.copies[n] = haCopy{v, data, []string{"p8s-mirror"}}
			j.to.rmu.Unlock()
		}
	}
	return vm.MirrorState{Running: true, Disks: []vm.MirrorDiskState{d}}, nil
}
func (m *mirrorVMs) MirrorStart(ctx context.Context, n string, t vm.MirrorTarget) error {
	mirrorHosts.Lock()
	to := mirrorHosts.m[t.Host]
	mirrorHosts.Unlock()
	if to == nil {
		return fmt.Errorf("no server at %s", t.Host)
	}
	m.mmu.Lock()
	defer m.mmu.Unlock()
	m.jobs[n] = &fakeJob{to: to}
	return nil
}
func (m *mirrorVMs) MirrorStop(ctx context.Context, n string) error {
	m.mmu.Lock()
	defer m.mmu.Unlock()
	delete(m.jobs, n)
	return nil
}
func (m *mirrorVMs) MirrorPrepare(ctx context.Context, p vm.MirrorPlan, from, host string) (vm.MirrorTarget, error) {
	if _, err := m.Get(ctx, p.Header.Name); err == nil {
		return vm.MirrorTarget{}, fmt.Errorf("%w: runs here", vm.ErrExists)
	}
	m.mmu.Lock()
	m.prepped = append(m.prepped, p.Header.Name+" from "+from)
	m.mmu.Unlock()
	m.rmu.Lock()
	if _, ok := m.copies[p.Header.Name]; !ok {
		m.copies[p.Header.Name] = haCopy{vm.VM{Name: p.Header.Name}, "", []string{"p8s-mirror"}}
	}
	m.rmu.Unlock()
	return vm.MirrorTarget{Host: host, Port: vm.MirrorPortBase, Disks: []int{0}}, nil
}
func (m *mirrorVMs) MirrorSynced(context.Context, string) error {
	m.mmu.Lock()
	m.synced++
	m.mmu.Unlock()
	return nil
}
func (m *mirrorVMs) job(n string) bool {
	m.mmu.Lock()
	defer m.mmu.Unlock()
	return m.jobs[n] != nil
}

func TestClusterMirror(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(d time.Duration) { clock.Add(int64(d)) }

	mv := newMirrorVMs(newHAVMs(map[string]vm.VM{}, map[string]string{}))
	w2 := newMirrorVMs(newHAVMs(map[string]vm.VM{"db1": {Name: "db1", State: "running", MemoryMiB: 1024}}, map[string]string{"db1": "rows v1"}))
	w3 := newMirrorVMs(newHAVMs(map[string]vm.VM{}, map[string]string{}))
	master := newClusterNode(t, 8192, mv, mv.ownedVMs)
	n2 := newClusterNode(t, 8192, w2, w2.ownedVMs)
	n3 := newClusterNode(t, 16384, w3, w3.ownedVMs)
	has := map[*clusterNode]*cluster.HA{}
	downs := map[*clusterNode]*downable{}
	for n, fake := range map[*clusterNode]*mirrorVMs{master: mv, n2: w2, n3: w3} {
		n.vms.Jobs = nil
		ha := &cluster.HA{C: n.vms, Local: fake, Dir: t.TempDir(), Now: now,
			SameLAN:   func(context.Context, cluster.Node) bool { return true },
			GatewayOK: func(context.Context) bool { return true },
			BootTime:  func() int64 { return 7 }}
		n.s.HA, n.s.LocalHA = ha, fake
		has[n] = ha
		d := &downable{h: n.srv.Config.Handler}
		n.srv.Config.Handler = d
		downs[n] = d
	}
	for _, w := range []*clusterNode{n2, n3} {
		b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
		code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
		w.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	}
	master.m.Poll(ctx)
	for n, fake := range map[*clusterNode]*mirrorVMs{master: mv, n2: w2, n3: w3} {
		mirrorHosts.Lock()
		mirrorHosts.m[n.m.SelfWGIP()] = fake
		mirrorHosts.Unlock()
	}
	ha := has[master]
	// one round: the tick, then its mirror round (a goroutine) done
	tick := func() {
		t.Helper()
		master.m.Poll(ctx)
		ha.Tick(ctx)
		want := now().UTC().Format(time.RFC3339)
		for i := 0; i < 300; i++ {
			c, ok := ha.Config(ctx, "db1")
			if !ok || c.LastTry == want && !c.Copying {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 300; i++ {
			if ok() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out: " + what)
	}

	// protect with a live mirror to n3: the mirror starts, the first look says "first sync"
	master.req(t, "PUT", "/api/v1/vms/db1/ha", secret, `{"enabled":true,"target":"n3","mode":"bogus"}`, 400)
	master.req(t, "PUT", "/api/v1/vms/db1/ha", secret, `{"enabled":true,"target":"n3","mode":"mirror"}`, 200)
	waitFor("mirror started", func() bool { return w2.job("db1") })
	waitFor("idle", func() bool { c, _ := ha.Config(ctx, "db1"); return !c.Copying })
	if c, _ := ha.Config(ctx, "db1"); c.Mode != "mirror" || c.LastCopy != "" || c.Synced {
		t.Fatalf("before the sync %+v", c)
	}
	if len(w3.prepped) != 1 || !strings.Contains(w3.prepped[0], "db1 from ") {
		t.Fatalf("target not prepared: %v", w3.prepped)
	}
	// 10 s later it is in sync: the copy's server is told, the copy counts as current
	for i := 0; i < 3; i++ {
		advance(11 * time.Second)
		tick()
		waitFor("idle", func() bool { c, _ := ha.Config(ctx, "db1"); return !c.Copying })
	}
	c, _ := ha.Config(ctx, "db1")
	if !c.Synced || c.LastCopy == "" || !strings.Contains(c.MirrorState, "in sync") || w3.synced == 0 {
		t.Fatalf("not in sync: %+v synced-calls=%d", c, w3.synced)
	}
	if cp, ok := w3.copyOf("db1"); !ok || cp.data != "rows v1" {
		t.Fatalf("copy %+v", cp)
	}
	if st := master.req(t, "GET", "/api/v1/ha", secret, "", 200); !strings.Contains(st, `"mode":"mirror"`) {
		t.Fatalf("status %s", st)
	}

	// the newest write: with a mirror it is on n3 too
	w2.mu.Lock()
	w2.disks["db1"] = "rows v2 (just written)"
	w2.mu.Unlock()
	advance(11 * time.Second)
	tick()
	waitFor("idle", func() bool { c, _ := ha.Config(ctx, "db1"); return !c.Copying })

	// n2 dies: db1 starts on n3 from the mirror, with the newest write
	downs[n2].down.Store(true)
	tick()
	advance(200 * time.Second)
	tick()
	if w3.state("db1") != "running" {
		t.Fatalf("not failed over: %+v", ha.Status(ctx).Events)
	}
	w3.mu.Lock()
	got := w3.disks["db1"]
	w3.mu.Unlock()
	if got != "rows v2 (just written)" {
		t.Fatalf("lost the newest write: %q", got)
	}
	c, _ = ha.Config(ctx, "db1")
	if c.Node != "n3" || c.Target != "n2" || c.Synced || len(c.Retire) != 1 {
		t.Fatalf("after failover %+v", c)
	}
	// n2 back: its old db1 is switched off, the mirror goes n3 → n2
	downs[n2].down.Store(false)
	w2.Fence(ctx, []string{"db1"})
	advance(11 * time.Second)
	tick()
	waitFor("reverse mirror", func() bool { return w3.job("db1") })
	if w2.state("db1") != "gone" {
		t.Fatal("old VM still on n2")
	}
	waitFor("idle", func() bool { c, _ := ha.Config(ctx, "db1"); return !c.Copying })

	// off: the mirror stops and the copy is deleted
	master.req(t, "PUT", "/api/v1/vms/db1/ha", secret, `{"enabled":false}`, 204)
	if w3.job("db1") {
		t.Fatal("mirror still running")
	}
	if _, ok := w2.copyOf("db1"); ok {
		t.Fatal("copy kept")
	}
}
