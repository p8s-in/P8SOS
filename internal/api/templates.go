package api

import (
	"context"
	"net/http"

	"p8sos/internal/vm"
)

// Clones and templates made from VMs.
//
//	POST   /vms/{name}/clone     {"name","start"}   copy a VM (background job)
//	POST   /vms/{name}/template  {"name","label"}   make a template from a VM (admins; job)
//	GET    /templates                               every template (custom: made from a VM)
//	DELETE /templates/{name}                        delete a template made from a VM (admins)

// TemplateVMs is the clone/template part of the VM service (local or cluster).
type TemplateVMs interface {
	StartClone(ctx context.Context, name string, r vm.CloneRequest) (vm.Job, error)
	StartMakeTemplate(ctx context.Context, name string, r vm.TemplateRequest) (vm.Job, error)
	ListTemplates(ctx context.Context) []vm.TemplateInfo
	DeleteTemplate(ctx context.Context, name string) error
}

func (s *Server) tplAPI(w http.ResponseWriter) (TemplateVMs, bool) {
	t, ok := s.VMs.(TemplateVMs)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "clones and templates aren't available here")
	}
	return t, ok
}

// templateList is every template, from the VM service when it knows them.
func (s *Server) templateList(ctx context.Context) []vm.TemplateInfo {
	if t, ok := s.VMs.(TemplateVMs); ok {
		return t.ListTemplates(ctx)
	}
	out := []vm.TemplateInfo{}
	for _, t := range s.Templates {
		label := t.Label
		if label == "" {
			label = t.Name
		}
		out = append(out, vm.TemplateInfo{Name: t.Name, Label: label, MinDiskGB: t.MinDiskGB, OSVariant: t.OSVariant, DefaultUser: t.User()})
	}
	return out
}

func (s *Server) cloneVM(w http.ResponseWriter, r *http.Request) {
	if !who(r).Admin { // a copy is a new VM: customers ask support (plans, quotas)
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin or operator can clone VMs")
		return
	}
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	t, ok := s.tplAPI(w)
	if !ok {
		return
	}
	var req vm.CloneRequest
	if !decode(w, r, &req) {
		return
	}
	j, err := t.StartClone(r.Context(), cur.Name, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) templateFromVM(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	t, ok := s.tplAPI(w)
	if !ok {
		return
	}
	var req vm.TemplateRequest
	if !decode(w, r, &req) {
		return
	}
	j, err := t.StartMakeTemplate(r.Context(), cur.Name, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	t, ok := s.tplAPI(w)
	if !ok {
		return
	}
	if err := t.DeleteTemplate(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
