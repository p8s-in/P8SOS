package cluster

import (
	"context"
	"fmt"
	"net/url"

	"p8sos/internal/vm"
)

// Clones and templates made from VMs, across the cluster: a VM is cloned on
// its own server; a template made from a VM lives on that server, and VMs
// made from it are created there.

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
	if !remote {
		return t.StartClone(ctx, name, r)
	}
	var j vm.Job
	err = c.call(ctx, n, "POST", vpath(name, "/clone"), r, &j)
	return j, err
}

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

// ListTemplates lists this server's templates and those made from VMs on the others.
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
	seen := map[string]bool{}
	for _, x := range t.ListTemplates(ctx) {
		if x.Custom {
			x.Node, x.NodeName = self, c.selfName()
		}
		seen[x.Name] = true
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
			if !x.Custom || seen[x.Name] {
				continue // built-in ones are on every server
			}
			x.Node, x.NodeName = n.ID, n.Name
			seen[x.Name] = true
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

// templateNode is the server that has a template made from a VM ("" = this one, or built-in).
func (c *VMs) templateNode(ctx context.Context, name string) string {
	if len(c.workers()) == 0 || name == "" {
		return ""
	}
	if t, err := c.tplLocal(); err == nil {
		for _, x := range t.ListTemplates(ctx) {
			if x.Name == name {
				return ""
			}
		}
	}
	for _, x := range c.ListTemplates(ctx) {
		if x.Name == name && x.Custom {
			return x.Node
		}
	}
	return ""
}

// DeleteTemplate deletes a template made from a VM, on the server that has it.
func (c *VMs) DeleteTemplate(ctx context.Context, name string) error {
	t, err := c.tplLocal()
	if err != nil {
		return err
	}
	if id := c.templateNode(ctx, name); id != "" && (c.M == nil || id != c.M.SelfID()) {
		n, ok := c.node(id)
		if !ok {
			return fmt.Errorf("%w: server %s is gone", vm.ErrConflict, id)
		}
		return c.call(ctx, n, "DELETE", "/templates/"+url.PathEscape(name), nil, nil)
	}
	return t.DeleteTemplate(ctx, name)
}
