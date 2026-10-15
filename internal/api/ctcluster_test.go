package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/cluster"
	"p8sos/internal/ct"
	"p8sos/internal/jobs"
	"p8sos/internal/testfake"
	"p8sos/internal/vm"
)

func clusterCTs(t *testing.T, templates string) (*ct.Service, *testfake.Runner, string) {
	dir := t.TempDir()
	run := &testfake.Runner{Out: map[string]string{
		"zfs list -H -t snapshot -o name,p8s:label": templates,
		"lxc-info -n":               "STOPPED\n",
		"zfs get -Hp -o value used": "1000\n",
	}}
	s := &ct.Service{Run: run, Pool: "tank", MountDir: filepath.Join(dir, "ct"), LXCDir: filepath.Join(dir, "lxc"),
		MetaFile: filepath.Join(dir, "containers.json"), Bridge: "virbr0", Chown: func(string, int, int) error { return nil }}
	t.Cleanup(s.Wait) // background first-boot setup, before the temp dir goes
	for _, n := range []string{"c1", "c2"} {
		_ = os.MkdirAll(filepath.Join(dir, "ct", n), 0o755)
	}
	return s, run, dir
}

func TestContainersInCluster(t *testing.T) {
	ctx := context.Background()
	mOwned := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
	wOwned := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
	master := newClusterNode(t, 4096, mOwned, mOwned)
	worker := newClusterNode(t, 16384, wOwned, wOwned)
	mct, _, _ := clusterCTs(t, "tank/p8s/ct-templates/debian-13@base\tDebian 13\n")
	wct, _, wdir := clusterCTs(t, "tank/p8s/ct-templates/debian-13@base\tDebian 13\ntank/p8s/ct-templates/alpine-3.22@base\t-\n")
	master.vms.Jobs = &jobs.Manager{}
	master.s.CTs, master.s.CTLocal = &cluster.CTs{Local: mct, M: master.m, V: master.vms}, mct
	worker.s.CTs, worker.s.CTLocal = &cluster.CTs{Local: wct, M: worker.m, V: worker.vms}, wct

	b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
	code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
	worker.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	master.m.Poll(ctx)

	// the worker has more room: the container goes there
	b = master.req(t, "POST", "/api/v1/containers", secret, `{"name":"c1","template":"debian-13","memory_mib":1024,"start":false}`, 201)
	if !strings.Contains(b, `"node_id":"n2"`) || !wct.Exists("c1") {
		t.Fatalf("placed %s", b)
	}
	// only the worker has alpine: asking for the master fails, auto goes to the worker
	master.req(t, "POST", "/api/v1/containers", secret, `{"name":"c2","template":"alpine-3.22","node":"n9","start":false}`, 400)
	// one list, tagged
	l := master.req(t, "GET", "/api/v1/containers", secret, "", 200)
	if !strings.Contains(l, `"name":"c1"`) || !strings.Contains(l, `"node":`) {
		t.Fatalf("list %s", l)
	}
	// manage it from the master
	master.req(t, "GET", "/api/v1/containers/c1", secret, "", 200)
	master.req(t, "PATCH", "/api/v1/containers/c1", secret, `{"memory_mib":2048}`, 200)
	if c, _ := wct.Get(ctx, "c1"); c.MemoryMiB != 2048 {
		t.Fatalf("resize didn't reach the worker: %+v", c)
	}

	// move it to the master (stopped): the disk streams worker → master
	wct.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "printf", "CT-DISK")
	}
	got := filepath.Join(t.TempDir(), "recv")
	mct.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "cat > "+got)
	}
	j := master.req(t, "POST", "/api/v1/containers/c1/move", secret, `{"to":"n1"}`, 202)
	if !strings.Contains(j, `"id"`) {
		t.Fatal(j)
	}
	master.vms.Jobs.Wait()
	jl := master.vms.Jobs.List()
	if len(jl) == 0 || jl[len(jl)-1].State != jobs.Done {
		t.Fatalf("move job %+v", jl)
	}
	if b, _ := os.ReadFile(got); string(b) != "CT-DISK" || !mct.Exists("c1") || wct.Exists("c1") {
		t.Fatalf("moved? %q here=%v there=%v", b, mct.Exists("c1"), wct.Exists("c1"))
	}
	if c, _ := mct.Get(ctx, "c1"); c.MemoryMiB != 2048 {
		t.Fatalf("settings lost: %+v", c)
	}
	_ = wdir
}
