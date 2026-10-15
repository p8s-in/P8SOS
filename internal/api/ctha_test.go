package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// Containers with high availability: copies of the disk go to another
// server; when the container's server dies, the copy is started there.
func TestContainerHA(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(d time.Duration) { clock.Add(int64(d)) }

	mv := newHAVMs(map[string]vm.VM{}, map[string]string{})
	wv := newHAVMs(map[string]vm.VM{}, map[string]string{})
	master := newClusterNode(t, 4096, mv, mv.ownedVMs)
	worker := newClusterNode(t, 16384, wv, wv.ownedVMs)
	mct, mrun, _ := clusterCTs(t, "tank/p8s/ct-templates/debian-13@base\tDebian 13\n")
	wct, wrun, _ := clusterCTs(t, "tank/p8s/ct-templates/debian-13@base\tDebian 13\n")
	master.vms.Jobs = &jobs.Manager{}
	mcts := &cluster.CTs{Local: mct, M: master.m, V: master.vms}
	master.s.CTs, master.s.CTLocal = mcts, mct
	worker.s.CTs, worker.s.CTLocal = &cluster.CTs{Local: wct, M: worker.m, V: worker.vms}, wct
	has := map[*clusterNode]*cluster.HA{}
	downs := map[*clusterNode]*downable{}
	for n, fake := range map[*clusterNode]*haVMs{master: mv, worker: wv} {
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
	has[master].CT = mcts
	has[worker].CT = worker.s.CTs.(*cluster.CTs)

	b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
	code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
	worker.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	master.m.Poll(ctx)
	ha := has[master]
	tick := func() { master.m.Poll(ctx); ha.Tick(ctx) }
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

	// c1 goes to the worker (more room), autostart on
	b = master.req(t, "POST", "/api/v1/containers", secret, `{"name":"c1","template":"debian-13","memory_mib":1024,"start":false,"autostart":true}`, 201)
	if !strings.Contains(b, `"node_id":"n2"`) {
		t.Fatalf("placed %s", b)
	}
	b = master.req(t, "GET", "/api/v1/containers/c1/ha", secret, "", 200)
	if !strings.Contains(b, `"protected":false`) || !strings.Contains(b, `"id":"n1"`) {
		t.Fatalf("info %s", b)
	}

	// the disk streams worker → master
	wct.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "printf", "CT-DISK-V1")
	}
	got := filepath.Join(t.TempDir(), "recv")
	mct.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "cat > "+got)
	}
	wrun.Out["lxc-info -n"] = "RUNNING\n"
	master.req(t, "PUT", "/api/v1/containers/c1/ha", secret, `{"enabled":true,"target":"n2"}`, 400) // its own server
	master.req(t, "PUT", "/api/v1/containers/c1/ha", secret, `{"enabled":true,"target":"n1","every_min":5}`, 200)
	waitFor("first copy", func() bool { b, _ := os.ReadFile(got); return string(b) == "CT-DISK-V1" })
	waitFor("copy recorded", func() bool { c, _ := ha.Config(ctx, "c1"); return c.LastCopy != "" && !c.Copying })
	if c, _ := ha.Config(ctx, "c1"); c.Kind != "ct" || c.Node != "n2" || !c.Autostart || c.Error != "" {
		t.Fatalf("config %+v", c)
	}
	if c, _ := wct.Get(ctx, "c1"); c.Autostart {
		t.Fatal("autostart still on: HA starts it")
	}
	master.req(t, "PATCH", "/api/v1/containers/c1", secret, `{"autostart":true}`, 409)
	if b := master.req(t, "GET", "/api/v1/ha", secret, "", 200); !strings.Contains(b, `"kind":"ct","name":"c1"`) || !strings.Contains(b, `"replicas":[{"kind":"ct","name":"c1"`) {
		t.Fatalf("status %s", b)
	}
	if mct.Exists("c1") {
		t.Fatal("a copy isn't a container")
	}
	tick() // c1 seen running

	// the worker dies: after the failover time c1 starts from its copy on the master
	downs[worker].down.Store(true)
	mrun.Fail = map[string]string{"zfs list -H -o name tank/p8s/ct/c1": "dataset does not exist"}
	mrun.Out["lxc-info -n"] = "RUNNING\n"
	tick()
	advance(100 * time.Second)
	tick()
	if mct.Exists("c1") {
		t.Fatal("failed over too early")
	}
	advance(100 * time.Second)
	tick()
	if !mct.Exists("c1") {
		t.Fatalf("not failed over: %+v", ha.Status(ctx).Events)
	}
	if calls := strings.Join(mrun.Calls, "\n"); !strings.Contains(calls, "zfs rename tank/p8s/ct-replica/c1 tank/p8s/ct/c1") {
		t.Fatalf("promote/start: %s", calls)
	}
	if ev := ha.Status(ctx).Events; !strings.Contains(ev[0].Text, "container c1 started on") {
		t.Fatalf("events %+v", ev)
	}
	c, _ := ha.Config(ctx, "c1")
	if c.Node != "n1" || c.Target != "n2" || len(c.Retire) != 1 {
		t.Fatalf("after failover %+v", c)
	}
	if b := master.req(t, "GET", "/api/v1/containers/c1", secret, "", 200); !strings.Contains(b, `"node_id":"n1"`) {
		t.Fatalf("not on the master: %s", b)
	}

	// the worker comes back: its old c1 is switched off and becomes the copy; copies go master → worker
	downs[worker].down.Store(false)
	mct.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "printf", "CT-DISK-V2")
	}
	got2 := filepath.Join(t.TempDir(), "recv2")
	wct.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "cat > "+got2)
	}
	advance(6 * time.Minute)
	tick()
	if wct.Exists("c1") {
		t.Fatal("old container still on the worker")
	}
	if calls := strings.Join(wrun.Calls, "\n"); !strings.Contains(calls, "lxc-stop -n c1 -k") || !strings.Contains(calls, "zfs rename tank/p8s/ct/c1 tank/p8s/ct-replica/c1") {
		t.Fatalf("retire: %s", calls)
	}
	waitFor("reverse copy", func() bool { b, _ := os.ReadFile(got2); return string(b) == "CT-DISK-V2" })
	waitFor("idle", func() bool { c, _ := ha.Config(ctx, "c1"); return !c.Copying && c.LastCopy != "" })

	// off: the copy on the worker is deleted, autostart comes back
	master.req(t, "PUT", "/api/v1/containers/c1/ha", secret, `{"enabled":false}`, 204)
	if calls := strings.Join(wrun.Calls, "\n"); !strings.Contains(calls, "zfs destroy -r tank/p8s/ct-replica/c1") {
		t.Fatal("copy kept")
	}
	if c, _ := mct.Get(ctx, "c1"); !c.Autostart {
		t.Fatal("autostart not given back")
	}
}
