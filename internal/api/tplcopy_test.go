package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// tplVMs adds templates made from VMs to the fake: name → its "disk".
type tplVMs struct {
	*moverVMs
	tmu  sync.Mutex
	tpls map[string]string
}

func (f *tplVMs) StartClone(context.Context, string, vm.CloneRequest) (vm.Job, error) {
	return vm.Job{}, vm.ErrConflict
}
func (f *tplVMs) StartMakeTemplate(context.Context, string, vm.TemplateRequest) (vm.Job, error) {
	return vm.Job{}, vm.ErrConflict
}
func (f *tplVMs) ListTemplates(context.Context) []vm.TemplateInfo {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	l := []vm.TemplateInfo{{Name: "debian-13", Label: "Debian 13"}}
	for n := range f.tpls {
		l = append(l, vm.TemplateInfo{Name: n, Label: n, Custom: true, From: "base"})
	}
	return l
}
func (f *tplVMs) DeleteTemplate(_ context.Context, n string) error {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if _, ok := f.tpls[n]; !ok {
		return fmt.Errorf("%w: %s", vm.ErrNotFound, n)
	}
	delete(f.tpls, n)
	return nil
}
func (f *tplVMs) TemplateCopyCheck(n string) error {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if _, ok := f.tpls[n]; !ok {
		return fmt.Errorf("%w: %s", vm.ErrNotFound, n)
	}
	return nil
}
func (f *tplVMs) ExportTemplate(_ context.Context, n string, w io.Writer) error {
	f.tmu.Lock()
	d := f.tpls[n]
	f.tmu.Unlock()
	_, err := fmt.Fprintf(w, "{\"name\":%q}\n%s", n, d)
	return err
}
func (f *tplVMs) ImportTemplate(_ context.Context, r io.Reader) (vm.TemplateInfo, error) {
	br := bufio.NewReader(r)
	line, _ := br.ReadBytes('\n')
	var h struct{ Name string }
	if err := json.Unmarshal(line, &h); err != nil {
		return vm.TemplateInfo{}, err
	}
	d, err := io.ReadAll(br)
	if err != nil {
		return vm.TemplateInfo{}, err
	}
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if _, ok := f.tpls[h.Name]; ok {
		return vm.TemplateInfo{}, fmt.Errorf("%w: %s", vm.ErrExists, h.Name)
	}
	f.tpls[h.Name] = string(d)
	return vm.TemplateInfo{Name: h.Name, Custom: true}, nil
}
func (f *tplVMs) Create(ctx context.Context, r vm.CreateRequest) (vm.VM, error) {
	f.tmu.Lock()
	_, ok := f.tpls[r.Template]
	f.tmu.Unlock()
	if r.Template != "" && r.Template != "debian-13" && !ok {
		return vm.VM{}, fmt.Errorf("%w: no template %s here", vm.ErrInvalid, r.Template)
	}
	return f.moverVMs.Create(ctx, r)
}

func TestClusterTemplateCopy(t *testing.T) {
	ctx := context.Background()
	mk := func(tpls map[string]string) *tplVMs {
		return &tplVMs{moverVMs: &moverVMs{&ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}, map[string]string{}}, tpls: tpls}
	}
	mv, wv, w2v := mk(map[string]string{}), mk(map[string]string{"web-base": "disk of web-base"}), mk(map[string]string{})
	// the master has little RAM: VMs go to the workers unless a template says otherwise
	master := newClusterNode(t, 4096, mv, mv.ownedVMs)
	worker := newClusterNode(t, 8192, wv, wv.ownedVMs)
	worker2 := newClusterNode(t, 16384, w2v, w2v.ownedVMs)
	for _, n := range []*clusterNode{master, worker, worker2} {
		n.vms.Jobs = &jobs.Manager{}
		n.s.LocalMoves = n.vms.Local.(*tplVMs)
	}
	master.req(t, "POST", "/api/v1/templates/web-base/copy", secret, `{}`, 409) // no cluster
	for _, w := range []*clusterNode{worker, worker2} {
		b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
		code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
		w.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	}
	master.m.Poll(ctx)

	// only the worker has it: a VM from it goes there, though worker 2 has more room
	b := master.req(t, "POST", "/api/v1/vms", secret, `{"name":"web1","template":"web-base","memory_mib":1024}`, 201)
	if !strings.Contains(b, `"node_id":"n2"`) {
		t.Fatalf("placed %s", b)
	}
	b = master.req(t, "GET", "/api/v1/templates", secret, "", 200)
	if !strings.Contains(b, `"nodes":[{"id":"n2"`) {
		t.Fatalf("list %s", b)
	}
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
	master.req(t, "POST", "/api/v1/templates/debian-13/copy", secret, `{}`, 409) // built-in
	master.req(t, "POST", "/api/v1/templates/nope/copy", secret, `{}`, 404)
	master.req(t, "POST", "/api/v1/templates/web-base/copy", secret, `{"to":["n2"]}`, 409) // has it
	master.req(t, "POST", "/api/v1/templates/web-base/copy", secret, `{"to":["n9"]}`, 400)

	// worker → master
	wait(master.req(t, "POST", "/api/v1/templates/web-base/copy", secret, `{"to":["n1"]}`, 202), "copied to")
	if mv.tpls["web-base"] != "disk of web-base" {
		t.Fatalf("master %v", mv.tpls)
	}
	// the rest (worker 2): from the master
	wait(master.req(t, "POST", "/api/v1/templates/web-base/copy", secret, ``, 202), "copied to")
	if w2v.tpls["web-base"] != "disk of web-base" {
		t.Fatalf("worker 2 %v", w2v.tpls)
	}
	master.req(t, "POST", "/api/v1/templates/web-base/copy", secret, `{}`, 409) // everyone has it
	b = master.req(t, "GET", "/api/v1/templates", secret, "", 200)
	if strings.Count(b, `"name":"web-base"`) != 1 || strings.Count(b, `"id":"n`) != 3 {
		t.Fatalf("list %s", b)
	}
	// now the VM goes to the server with the most room
	if b := master.req(t, "POST", "/api/v1/vms", secret, `{"name":"web2","template":"web-base","memory_mib":1024}`, 201); !strings.Contains(b, `"node_id":"n3"`) {
		t.Fatalf("placed %s", b)
	}

	// delete one copy, then all
	master.req(t, "DELETE", "/api/v1/templates/web-base?node=n3", secret, "", 204)
	if _, ok := w2v.tpls["web-base"]; ok || mv.tpls["web-base"] == "" {
		t.Fatal("one copy")
	}
	master.req(t, "DELETE", "/api/v1/templates/web-base?node=n3", secret, "", 404)
	master.req(t, "DELETE", "/api/v1/templates/web-base", secret, "", 204)
	if len(mv.tpls)+len(wv.tpls) != 0 {
		t.Fatal("all copies")
	}
	worker.req(t, "GET", "/api/v1/cluster/tpl-export/web-base", "", "", 401)
}
