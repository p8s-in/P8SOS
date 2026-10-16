package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// Clones and templates made from VMs, across the cluster: a VM is cloned on
// its own server; a template made from a VM lives on that server and can be
// copied to others; VMs made from it go on a server that has a copy.

// templateLocal is the clone/template part of the local VM service.
type templateLocal interface {
	StartClone(ctx context.Context, name string, r vm.CloneRequest) (vm.Job, error)
	StartMakeTemplate(ctx context.Context, name string, r vm.TemplateRequest) (vm.Job, error)
	ListTemplates(ctx context.Context) []vm.TemplateInfo
	DeleteTemplate(ctx context.Context, name string) error
}

var errNoTemplates = fmt.Errorf("%w: clones and templates aren't available on this server", vm.ErrConflict)

func (c *VMs) tplLocal() (templateLocal, error) {
	t, ok := c.Local.(templateLocal)
	if !ok {
		return nil, errNoTemplates
	}
	return t, nil
}

// StartClone copies a VM on the server that has it (names are unique in the cluster).
func (c *VMs) StartClone(ctx context.Context, name string, r vm.CloneRequest) (vm.Job, error) {
	t, err := c.tplLocal()
	if err != nil {
		return vm.Job{}, err
	}
	if len(c.workers()) > 0 {
		if _, err := c.Get(ctx, r.Name); err == nil {
			return vm.Job{}, fmt.Errorf("%w: vm %q", vm.ErrExists, r.Name)
		}
	}
	n, remote := c.locate(ctx, name)
	from := ""
	if c.M != nil {
		from = c.M.SelfID()
	}
	if remote {
		from = n.ID
	}
	if to := c.cloneTarget(r.Node); r.Node != "" && to != from {
		return c.startCloneTo(ctx, name, r, from, to)
	}
	r.Node = ""
	if !remote {
		return t.StartClone(ctx, name, r)
	}
	var j vm.Job
	err = c.call(ctx, n, "POST", vpath(name, "/clone"), r, &j)
	return j, err
}

// cloneTarget is the id of the server asked for by id or name ("" = none such).
func (c *VMs) cloneTarget(node string) string {
	if node == "" || c.M == nil {
		return ""
	}
	if node == c.M.SelfID() || strings.EqualFold(node, c.M.SelfName()) {
		return c.M.SelfID()
	}
	if n, ok := c.node(node); ok {
		return n.ID
	}
	if n, ok := c.nodeByName(node); ok {
		return n.ID
	}
	return ""
}

// startCloneTo copies a VM onto another server in one job: the copy is made
// next to the VM (shut down), then moved to the server asked for.
func (c *VMs) startCloneTo(ctx context.Context, name string, r vm.CloneRequest, from, to string) (vm.Job, error) {
	switch {
	case to == "":
		return vm.Job{}, fmt.Errorf("%w: no server %q in the cluster", vm.ErrInvalid, r.Node)
	case r.Linked:
		return vm.Job{}, fmt.Errorf("%w: a linked clone shares the VM's disk, so it stays on the VM's server; make a full copy to put it on another one", vm.ErrInvalid)
	case r.Storage != "" && r.Storage != "local":
		return vm.Job{}, fmt.Errorf("%w: a copy for another server goes on its local disks (storage \"local\")", vm.ErrInvalid)
	case c.Jobs == nil:
		return vm.Job{}, fmt.Errorf("%w: moving isn't set up", vm.ErrConflict)
	}
	if to != c.M.SelfID() && !c.M.NodeStatus(to).Online {
		return vm.Job{}, fmt.Errorf("%w: server %s is offline", vm.ErrConflict, c.nodeName(to))
	}
	start := r.Start
	r.Start, r.Node, r.Storage = false, "", ""
	self := c.M.SelfID()
	j, err := c.Jobs.Start("clone", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		p("copying " + name + " on " + c.nodeName(from))
		var cj vm.Job
		var err error
		if from == self {
			cj, err = c.tplLocalMust().StartClone(ctx, name, r)
		} else if n, ok := c.node(from); ok {
			err = c.call(ctx, n, "POST", vpath(name, "/clone"), r, &cj)
		} else {
			err = fmt.Errorf("server %s is gone", from)
		}
		if err != nil {
			return "", err
		}
		for {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(cloneWait):
			}
			got, ok := c.Job(cj.ID)
			if !ok {
				return "", fmt.Errorf("the copy's job on %s disappeared", c.nodeName(from))
			}
			if got.State == jobs.Failed {
				return "", errors.New(got.Error)
			}
			if got.State == jobs.Done {
				break
			}
			if got.Message != "" {
				p("copying on " + c.nodeName(from) + ": " + got.Message)
			}
		}
		if from == self {
			c.remember(r.Name, "")
		} else {
			c.remember(r.Name, from)
		}
		info, err := c.MoveInfo(ctx, r.Name)
		if err == nil && info.Why != "" {
			err = errors.New(info.Why)
		}
		if err != nil {
			return "", fmt.Errorf("%s was made on %s but can't be moved to %s: %v (it stays on %s)", r.Name, c.nodeName(from), c.nodeName(to), err, c.nodeName(from))
		}
		msg, err := c.move(ctx, r.Name, from, to, self, start, p)
		if err != nil {
			return "", fmt.Errorf("%s was made on %s; moving it to %s failed: %v (it stays on %s)", r.Name, c.nodeName(from), c.nodeName(to), err, c.nodeName(from))
		}
		return name + " copied: " + msg, nil
	})
	if errors.Is(err, jobs.ErrBusy) {
		return vm.Job{}, fmt.Errorf("%w: %v", vm.ErrConflict, err)
	}
	return j, err
}

// cloneWait is how often the copy's job is checked.
var cloneWait = 2 * time.Second

func (c *VMs) tplLocalMust() templateLocal { t, _ := c.tplLocal(); return t }

// StartMakeTemplate makes a template from a VM on the server that has it.
func (c *VMs) StartMakeTemplate(ctx context.Context, name string, r vm.TemplateRequest) (vm.Job, error) {
	t, err := c.tplLocal()
	if err != nil {
		return vm.Job{}, err
	}
	for _, x := range c.ListTemplates(ctx) {
		if x.Name == r.Name {
			return vm.Job{}, fmt.Errorf("%w: a template %q exists (on %s)", vm.ErrExists, r.Name, x.NodeName)
		}
	}
	n, remote := c.locate(ctx, name)
	if !remote {
		return t.StartMakeTemplate(ctx, name, r)
	}
	var j vm.Job
	err = c.call(ctx, n, "POST", vpath(name, "/template"), r, &j)
	return j, err
}

// ListTemplates lists this server's templates and those made from VMs on the
// others; a template copied to several servers is listed once, with them all.
func (c *VMs) ListTemplates(ctx context.Context) []vm.TemplateInfo {
	t, err := c.tplLocal()
	if err != nil {
		return []vm.TemplateInfo{}
	}
	self := ""
	if c.M != nil {
		self = c.M.SelfID()
	}
	out := []vm.TemplateInfo{}
	at := map[string]int{}
	for _, x := range t.ListTemplates(ctx) {
		if x.Custom {
			x.Node, x.NodeName = self, c.selfName()
			x.Nodes = []vm.TemplateNode{{ID: self, Name: x.NodeName}}
		}
		at[x.Name] = len(out)
		out = append(out, x)
	}
	for _, n := range c.workers() {
		if c.M != nil && !c.M.NodeStatus(n.ID).Online {
			continue
		}
		var r struct {
			Templates []vm.TemplateInfo `json:"templates"`
		}
		if err := c.call(ctx, n, "GET", "/templates", nil, &r); err != nil {
			continue
		}
		for _, x := range r.Templates {
			if !x.Custom {
				continue // built-in ones are on every server
			}
			if i, ok := at[x.Name]; ok {
				if out[i].Custom {
					out[i].Nodes = append(out[i].Nodes, vm.TemplateNode{ID: n.ID, Name: n.Name})
				}
				continue
			}
			x.Node, x.NodeName = n.ID, n.Name
			x.Nodes = []vm.TemplateNode{{ID: n.ID, Name: n.Name}}
			at[x.Name] = len(out)
			out = append(out, x)
		}
	}
	return out
}

func (c *VMs) selfName() string {
	if c.M == nil {
		return ""
	}
	return c.M.SelfName()
}

// templateHolders is the servers that have a template made from a VM (nil:
// a built-in one, or none of that name).
func (c *VMs) templateHolders(ctx context.Context, name string) map[string]bool {
	if len(c.workers()) == 0 || name == "" {
		return nil
	}
	for _, x := range c.ListTemplates(ctx) {
		if x.Name != name {
			continue
		}
		if !x.Custom {
			return nil
		}
		have := map[string]bool{}
		for _, n := range x.Nodes {
			have[n.ID] = true
		}
		return have
	}
	return nil
}

// templateNode picks the server for a new VM from a template made from a VM
// when this server has no copy of it ("" = this one, or anywhere).
func (c *VMs) templateNode(ctx context.Context, r vm.CreateRequest) string {
	have := c.templateHolders(ctx, r.Template)
	if have == nil || c.M == nil || have[c.M.SelfID()] {
		return ""
	}
	first := ""
	for _, cd := range c.Candidates(ctx, r) {
		if !have[cd.Node.ID] {
			continue
		}
		if cd.Reason == "" {
			return cd.Node.ID
		}
		if first == "" {
			first = cd.Node.ID // place() says why it can't
		}
	}
	return first
}

// DeleteTemplate deletes a template made from a VM, on every server that has it.
func (c *VMs) DeleteTemplate(ctx context.Context, name string) error {
	return c.DeleteTemplateOn(ctx, name, "")
}

// DeleteTemplateOn deletes the copy of a template on one server (node ""
// = on every server that has it).
func (c *VMs) DeleteTemplateOn(ctx context.Context, name, node string) error {
	t, err := c.tplLocal()
	if err != nil {
		return err
	}
	have := c.templateHolders(ctx, name)
	if have == nil {
		if node != "" && c.M != nil && node != c.M.SelfID() {
			return fmt.Errorf("%w: template %q on server %s", vm.ErrNotFound, name, node)
		}
		return t.DeleteTemplate(ctx, name)
	}
	if n, ok := c.nodeByName(node); ok {
		node = n.ID
	} else if c.M != nil && node != "" && strings.EqualFold(node, c.M.SelfName()) {
		node = c.M.SelfID()
	}
	if node != "" {
		if !have[node] {
			return fmt.Errorf("%w: server %s has no copy of template %q", vm.ErrNotFound, node, name)
		}
		have = map[string]bool{node: true}
	}
	var errs []string
	for id := range have {
		var err error
		if c.M != nil && id == c.M.SelfID() {
			err = t.DeleteTemplate(ctx, name)
		} else if n, ok := c.node(id); !ok {
			err = fmt.Errorf("server %s is gone", id)
		} else {
			err = c.call(ctx, n, "DELETE", "/templates/"+url.PathEscape(name), nil, nil)
		}
		if err != nil {
			errs = append(errs, c.nodeName(id)+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%w: %s", vm.ErrConflict, strings.Join(errs, "; "))
	}
	return nil
}

func (c *VMs) nodeName(id string) string {
	if c.M != nil && id == c.M.SelfID() {
		return c.M.SelfName()
	}
	if n, ok := c.node(id); ok {
		return n.Name
	}
	return id
}

// --- copying a template to other servers ---------------------------------

// TemplateCopier is the copy part of the local VM service (vm.Service).
type TemplateCopier interface {
	TemplateCopyCheck(name string) error
	ExportTemplate(ctx context.Context, name string, w io.Writer) error
	ImportTemplate(ctx context.Context, r io.Reader) (vm.TemplateInfo, error)
}

var _ TemplateCopier = (*vm.Service)(nil)

// StartCopyTemplate copies a template made from a VM to the servers to (none
// = every online server that has no copy yet), in the background.
func (c *VMs) StartCopyTemplate(ctx context.Context, name string, to []string) (vm.Job, error) {
	if _, ok := c.Local.(TemplateCopier); !ok || c.M == nil || c.Jobs == nil {
		return vm.Job{}, errNoTemplates
	}
	if len(c.workers()) == 0 {
		return vm.Job{}, fmt.Errorf("%w: copying templates needs a cluster", vm.ErrConflict)
	}
	var tpl *vm.TemplateInfo
	for _, x := range c.ListTemplates(ctx) {
		if x.Name == name {
			tpl = &x
			break
		}
	}
	if tpl == nil {
		return vm.Job{}, fmt.Errorf("%w: template %q", vm.ErrNotFound, name)
	}
	if !tpl.Custom {
		return vm.Job{}, fmt.Errorf("%w: %q is a built-in template: add it on each server (Server → OS templates)", vm.ErrConflict, name)
	}
	have := map[string]bool{}
	for _, n := range tpl.Nodes {
		have[n.ID] = true
	}
	self := c.M.SelfID()
	online := func(id string) bool { return id == self || c.M.NodeStatus(id).Online }
	all := []string{self}
	for _, n := range c.workers() {
		all = append(all, n.ID)
	}
	var dst []string
	if len(to) == 0 {
		for _, id := range all {
			if !have[id] && online(id) {
				dst = append(dst, id)
			}
		}
		if len(dst) == 0 {
			return vm.Job{}, fmt.Errorf("%w: every online server has %s already", vm.ErrConflict, name)
		}
	}
	for _, id := range to {
		if id == self {
			dst = append(dst, id)
		} else if n, ok := c.node(id); ok {
			dst = append(dst, n.ID)
		} else if n, ok := c.nodeByName(id); ok {
			dst = append(dst, n.ID)
		} else {
			return vm.Job{}, fmt.Errorf("%w: no server %q in the cluster", vm.ErrInvalid, id)
		}
		d := dst[len(dst)-1]
		if have[d] {
			return vm.Job{}, fmt.Errorf("%w: %s has %s already", vm.ErrConflict, c.nodeName(d), name)
		}
		if !online(d) {
			return vm.Job{}, fmt.Errorf("%w: server %s is offline", vm.ErrConflict, c.nodeName(d))
		}
	}
	src := ""
	if have[self] {
		src = self
	} else {
		for _, n := range tpl.Nodes {
			if online(n.ID) {
				src = n.ID
				break
			}
		}
	}
	if src == "" {
		return vm.Job{}, fmt.Errorf("%w: no server with %s is online", vm.ErrConflict, name)
	}
	j, err := c.Jobs.Start("template-copy", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		var ok, bad []string
		for _, d := range dst {
			p("copying to " + c.nodeName(d))
			if err := c.copyTemplate(ctx, name, src, d, p); err != nil {
				bad = append(bad, c.nodeName(d)+": "+err.Error())
				continue
			}
			ok = append(ok, c.nodeName(d))
		}
		if len(ok) == 0 {
			return "", errors.New(strings.Join(bad, "; "))
		}
		msg := fmt.Sprintf("template %s copied to %s", name, strings.Join(ok, ", "))
		if len(bad) > 0 {
			msg += "; not copied to " + strings.Join(bad, "; ")
		}
		return msg, nil
	})
	if errors.Is(err, jobs.ErrBusy) {
		return vm.Job{}, fmt.Errorf("%w: %v", vm.ErrConflict, err)
	}
	return j, err
}

func (c *VMs) nodeByName(name string) (Node, bool) {
	for _, n := range c.workers() {
		if strings.EqualFold(n.Name, name) {
			return n, true
		}
	}
	return Node{}, false
}

// copyTemplate streams the template from server src to server dst (one of them may be this one).
func (c *VMs) copyTemplate(ctx context.Context, name, src, dst string, p jobs.Progress) error {
	lc := c.Local.(TemplateCopier)
	self := c.M.SelfID()
	tick := func(n int64) { p(fmt.Sprintf("copying to %s: %.1f GB sent", c.nodeName(dst), float64(n)/1e9)) }
	path := "/cluster/tpl-export/" + url.PathEscape(name)
	var in io.Reader
	if src == self {
		if err := lc.TemplateCopyCheck(name); err != nil {
			return err
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(lc.ExportTemplate(ctx, name, pw)) }()
		defer pr.CloseWithError(errors.New("copy ended"))
		in = pr
	} else {
		n, ok := c.node(src)
		if !ok {
			return fmt.Errorf("server %s is gone", src)
		}
		resp, err := c.M.stream(ctx, n, "GET", path, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		in = resp.Body
	}
	in = &counter{r: in, tick: tick}
	if dst == self {
		_, err := lc.ImportTemplate(ctx, in)
		return err
	}
	n, ok := c.node(dst)
	if !ok {
		return fmt.Errorf("server %s is gone", dst)
	}
	resp, err := c.M.stream(ctx, n, "POST", "/cluster/tpl-import", in)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type unlinker interface {
	StartUnlink(ctx context.Context, name string) (vm.Job, error)
}

// StartUnlink makes a linked clone independent, on the server that has it.
func (c *VMs) StartUnlink(ctx context.Context, name string) (vm.Job, error) {
	u, ok := c.Local.(unlinker)
	if !ok {
		return vm.Job{}, errNoTemplates
	}
	n, remote := c.locate(ctx, name)
	if !remote {
		return u.StartUnlink(ctx, name)
	}
	var j vm.Job
	err := c.call(ctx, n, "POST", vpath(name, "/unlink"), nil, &j)
	return j, err
}

// SetCloneWait changes how often the copy's job is checked (tests); it returns the old value.
func SetCloneWait(d time.Duration) time.Duration { old := cloneWait; cloneWait = d; return old }
