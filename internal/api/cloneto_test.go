package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// cloneVMs: moverVMs that can copy a VM (in a job, like the real one).
type cloneVMs struct {
	*moverVMs
	jm *jobs.Manager
}

func (c *cloneVMs) StartClone(ctx context.Context, name string, r vm.CloneRequest) (vm.Job, error) {
	if r.Node != "" {
		return vm.Job{}, fmt.Errorf("%w: node reached the server", vm.ErrInvalid)
	}
	return c.jm.Start("clone", name, func(context.Context, jobs.Progress) (string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		v := c.vms[name]
		v.Name, v.State = r.Name, "stopped"
		c.vms[r.Name] = v
		c.disks[r.Name] = "copy of " + c.disks[name]
		return "copied", nil
	})
}
func (c *cloneVMs) StartMakeTemplate(context.Context, string, vm.TemplateRequest) (vm.Job, error) {
	return vm.Job{}, vm.ErrConflict
}
func (c *cloneVMs) ListTemplates(context.Context) []vm.TemplateInfo { return []vm.TemplateInfo{} }
func (c *cloneVMs) DeleteTemplate(context.Context, string) error    { return vm.ErrNotFound }
func (c *cloneVMs) Job(id string) (vm.Job, bool)                    { return c.jm.Get(id) }
func (c *cloneVMs) ListJobs() []vm.Job                              { return c.jm.List() }

func TestCloneToAnotherServer(t *testing.T) {
	ctx := context.Background()
	defer func(w time.Duration) { cluster.SetCloneWait(w) }(cluster.SetCloneWait(10 * time.Millisecond))
	mk := func(vms map[string]vm.VM, disks map[string]string) *cloneVMs {
		return &cloneVMs{&moverVMs{&ownedVMs{fakeVMs: &fakeVMs{}, vms: vms}, disks}, &jobs.Manager{}}
	}
	mv := mk(map[string]vm.VM{}, map[string]string{})
	wv := mk(map[string]vm.VM{"db1": {Name: "db1", State: "running", MemoryMiB: 2048, Owner: "alice"}}, map[string]string{"db1": "disk of db1"})
	master := newClusterNode(t, 8192, mv, mv.ownedVMs)
	worker := newClusterNode(t, 8192, wv, wv.ownedVMs)
	for _, n := range []*clusterNode{master, worker} {
		n.vms.Jobs = n.vms.Local.(*cloneVMs).jm
		n.s.Migrate = n.vms
		n.s.LocalMoves = n.vms.Local.(*cloneVMs)
	}
	b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
	code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
	worker.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	master.m.Poll(ctx)
	master.req(t, "GET", "/api/v1/vms/db1", secret, "", 200) // the master learns where db1 is

	master.req(t, "POST", "/api/v1/vms/db1/clone", secret, `{"name":"db2","node":"n1","linked":true}`, 400)
	master.req(t, "POST", "/api/v1/vms/db1/clone", secret, `{"name":"db2","node":"n9"}`, 400)

	// db1 runs on the worker; its copy goes to the master, started
	b = master.req(t, "POST", "/api/v1/vms/db1/clone", secret, `{"name":"db2","node":"n1","start":true}`, 202)
	var j0 jobs.Job
	json.Unmarshal([]byte(b), &j0)
	master.vms.Jobs.Wait()
	j, _ := master.vms.Jobs.Get(j0.ID)
	if j.State != "done" || !strings.Contains(j.Message, "db1 copied: db2 moved to") || !strings.Contains(j.Message, "started") {
		t.Fatalf("job %+v", j)
	}
	if mv.disks["db2"] != "copy of disk of db1" || mv.vms["db2"].Owner != "alice" {
		t.Fatalf("on the master: %v %+v", mv.disks, mv.vms["db2"])
	}
	if _, ok := wv.vms["db2"]; ok {
		t.Fatal("the copy stayed on the worker")
	}
	if _, ok := wv.vms["db1"]; !ok {
		t.Fatal("the original is gone")
	}
	// same server: an ordinary clone
	b = master.req(t, "POST", "/api/v1/vms/db1/clone", secret, `{"name":"db3","node":"n2"}`, 202)
	json.Unmarshal([]byte(b), &j0)
	wv.jm.Wait()
	if _, ok := wv.vms["db3"]; !ok {
		t.Fatalf("db3 %s", b)
	}
}
