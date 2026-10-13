package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/vm"
)

// High availability (admins). The master runs it; every server answers the
// /cluster/replica… calls for the copies it sends or keeps.

func (s *Server) haOn(w http.ResponseWriter) bool {
	if s.HA != nil && s.LocalHA != nil {
		return true
	}
	writeErr(w, http.StatusNotFound, "not_found", "high availability isn't available on this server")
	return false
}

func (s *Server) haStatus(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.HA == nil {
		writeJSON(w, http.StatusOK, cluster.HAStatus{VMs: []cluster.HAVMView{}, Events: []cluster.HAEvent{}})
		return
	}
	writeJSON(w, http.StatusOK, s.HA.Status(r.Context()))
}

func (s *Server) haSettings(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	var body cluster.HASettings
	if !decode(w, r, &body) {
		return
	}
	writeJSON(w, http.StatusOK, s.HA.SetSettings(body))
}

func (s *Server) haInfo(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.HA == nil {
		writeJSON(w, http.StatusOK, cluster.HAInfo{Why: "high availability isn't available on this server", Targets: []cluster.MoveTarget{}})
		return
	}
	info, err := s.HA.Info(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) haSet(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	var body struct {
		Enabled  bool   `json:"enabled"`
		Target   string `json:"target"`
		EveryMin int    `json:"every_min"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	if !body.Enabled {
		if err := s.HA.Unprotect(r.Context(), name); err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	v, err := s.HA.Protect(r.Context(), name, body.Target, body.EveryMin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) haFailover(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	msg, err := s.HA.Failover(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

func (s *Server) haCheck(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	why, err := s.LocalHA.HACheck(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"why": why})
}

func (s *Server) setAutostart(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var body struct {
		Autostart bool `json:"autostart"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.HA != nil && body.Autostart {
		if _, protected := s.HA.Config(r.Context(), r.PathValue("name")); protected {
			writeErr(w, http.StatusConflict, "conflict", "high availability starts this VM; turn HA off first")
			return
		}
	}
	a, ok := s.VMs.(interface {
		SetAutostart(ctx context.Context, name string, on bool) (vm.VM, error)
	})
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "autostart can't be changed here")
		return
	}
	v, err := a.SetAutostart(r.Context(), r.PathValue("name"), body.Autostart)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// haSync: the master's heartbeat (workers).
func (s *Server) haSync(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	var names []string
	for _, n := range strings.Split(r.URL.Query().Get("vms"), ",") {
		if n != "" {
			names = append(names, n)
		}
	}
	sec := 0
	for _, c := range r.URL.Query().Get("fence") {
		if c < '0' || c > '9' {
			sec = 0
			break
		}
		sec = sec*10 + int(c-'0')
	}
	fenced, boot := s.HA.Sync(names, sec)
	writeJSON(w, http.StatusOK, map[string]any{"fenced": fenced, "boot": boot})
}

func (s *Server) replicaSnaps(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	l, err := s.LocalHA.ReplicaSnaps(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snaps": l})
}

func (s *Server) replicaSend(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	name := r.PathValue("name")
	if why, err := s.LocalHA.HACheck(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	} else if why != "" {
		writeErr(w, http.StatusConflict, "conflict", name+" can't be copied: "+why)
		return
	}
	var have []string
	for _, h := range strings.Split(r.URL.Query().Get("have"), ",") {
		if h != "" {
			have = append(have, h)
		}
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(24 * time.Hour))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if err := s.LocalHA.SendReplica(r.Context(), name, have, r.URL.Query().Get("from"), w); err != nil {
		if s.Log != nil {
			s.Log.Error("send vm copy", "vm", name, "err", err)
		}
		panic(http.ErrAbortHandler) // the receiver sees a cut stream
	}
}

func (s *Server) replicaRecv(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(24 * time.Hour))
	rep, err := s.LocalHA.RecvReplica(r.Context(), r.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rep)
}

func (s *Server) replicaPromote(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	v, note, err := s.LocalHA.PromoteReplica(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": v, "note": note})
}

func (s *Server) replicaRetire(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	if err := s.LocalHA.RetireVM(r.Context(), r.PathValue("name"), r.URL.Query().Get("from")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// haStartCopy: start a copy held on this server by hand (its server is gone).
func (s *Server) haStartCopy(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	var body struct {
		Start bool `json:"start"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	v, note, err := s.LocalHA.PromoteReplica(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Start {
		if sv, err := s.VMs.Start(r.Context(), name); err != nil {
			note += "; starting it failed: " + err.Error()
		} else {
			v = sv
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": v, "note": strings.TrimPrefix(note, "; ")})
}

func (s *Server) haDropCopy(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.haOn(w) {
		return
	}
	if err := s.LocalHA.DropReplica(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
