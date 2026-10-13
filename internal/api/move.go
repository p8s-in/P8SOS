package api

import (
	"context"
	"net/http"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/vm"
)

// Moving VMs between cluster servers (admins). The master runs the move;
// every server answers vm-export / vm-import for the stream itself.

// Migrator is the cluster's move side (cluster.VMs).
type Migrator interface {
	MoveInfo(ctx context.Context, name string) (cluster.MoveInfo, error)
	StartMove(ctx context.Context, name, to string, start bool) (vm.Job, error)
}

func (s *Server) moveInfo(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.Migrate == nil {
		writeJSON(w, http.StatusOK, cluster.MoveInfo{Why: "moving VMs needs a cluster", Targets: []cluster.MoveTarget{}})
		return
	}
	info, err := s.Migrate.MoveInfo(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) startMove(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.Migrate == nil {
		writeErr(w, http.StatusConflict, "conflict", "moving VMs needs a cluster")
		return
	}
	var body struct {
		To    string `json:"to"`
		Start bool   `json:"start"`
	}
	if !decode(w, r, &body) {
		return
	}
	j, err := s.Migrate.StartMove(r.Context(), r.PathValue("name"), body.To, body.Start)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}

// moveCheck: can this server's VM be moved now (asked by the master).
func (s *Server) moveCheck(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.LocalMoves == nil {
		writeJSON(w, http.StatusOK, map[string]string{"why": "this server can't move VMs"})
		return
	}
	why, err := s.LocalMoves.MoveCheck(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"why": why})
}

func (s *Server) vmExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.LocalMoves == nil {
		writeErr(w, http.StatusNotFound, "not_found", "this server can't move VMs")
		return
	}
	name := r.PathValue("name")
	if why, err := s.LocalMoves.MoveCheck(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	} else if why != "" {
		writeErr(w, http.StatusConflict, "conflict", name+" can't be moved: "+why)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(24 * time.Hour))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if err := s.LocalMoves.ExportVM(r.Context(), name, w); err != nil && s.Log != nil {
		// the receiver sees a cut stream and gives up; nothing is deleted here
		s.Log.Error("vm export for a move", "vm", name, "err", err)
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) vmImport(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.LocalMoves == nil {
		writeErr(w, http.StatusNotFound, "not_found", "this server can't move VMs")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(24 * time.Hour))
	v, note, err := s.LocalMoves.ImportMovedVM(r.Context(), r.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"vm": v, "note": note})
}
