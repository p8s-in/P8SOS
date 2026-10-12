package api

import (
	"errors"
	"net/http"

	"p8sos/internal/tasks"
)

// The Server page (admins): storage, OS templates, home network, Debian
// updates, reboot, and the log of those tasks.

func (s *Server) nodeOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.Node == nil {
		writeErr(w, http.StatusNotFound, "not_found", "server tasks are not available on this host")
		return false
	}
	return true
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	if s.nodeOn(w, r) {
		writeJSON(w, http.StatusOK, s.Node.Overview(r.Context()))
	}
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	var body struct {
		Kind string `json:"kind"`
		Arg  string `json:"arg"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Kind == tasks.Template || body.Kind == tasks.CTTemplate {
		o := s.Node.Overview(r.Context())
		cat := o.Catalog
		if body.Kind == tasks.CTTemplate {
			cat = o.CTCatalog
		}
		ok := false
		for _, c := range cat {
			ok = ok || (c.Name == body.Arg && !c.Installed)
		}
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid", "unknown or already added template: "+body.Arg)
			return
		}
	}
	t, err := s.Node.Tasks.Start(r.Context(), body.Kind, body.Arg, who(r).Name)
	switch {
	case errors.Is(err, tasks.ErrBusy):
		writeErr(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, tasks.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusAccepted, t)
	}
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	if s.nodeOn(w, r) {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": s.Node.Tasks.List(r.Context())})
	}
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	t, ok := s.Node.Tasks.Get(r.Context(), r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	writeJSON(w, http.StatusOK, t)
}
