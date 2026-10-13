package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/virt"
	"p8sos/internal/vm"
)

// This server's side of live moves (the master calls these with the cluster
// token): check, describe, prepare, send (streams progress), finish, abort,
// clean up. Admins only.

func (s *Server) liveOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.LocalLive == nil {
		writeErr(w, http.StatusNotFound, "not_found", "this server can't move running VMs")
		return false
	}
	return true
}

func (s *Server) liveCheck(w http.ResponseWriter, r *http.Request) {
	if !s.liveOn(w, r) {
		return
	}
	why, err := s.LocalLive.LiveCheck(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"why": why})
}

func (s *Server) liveHeader(w http.ResponseWriter, r *http.Request) {
	if !s.liveOn(w, r) {
		return
	}
	h, err := s.LocalLive.LiveHeader(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) livePrepare(w http.ResponseWriter, r *http.Request) {
	if !s.liveOn(w, r) {
		return
	}
	var h vm.LiveHeader
	if !decodeBig(w, r, &h) {
		return
	}
	plan, err := s.LocalLive.LivePrepare(r.Context(), h)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// liveSend streams "progress N" lines, then "done" or "error: …".
func (s *Server) liveSend(w http.ResponseWriter, r *http.Request) {
	if !s.liveOn(w, r) {
		return
	}
	var req cluster.LiveSendRequest
	if !decodeBig(w, r, &req) {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(24 * time.Hour))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	err := s.LocalLive.LiveSend(r.Context(), r.PathValue("name"), req.IP, req.Plan, func(p int) {
		fmt.Fprintf(w, "progress %d\n", p)
		_ = rc.Flush()
	})
	if err != nil {
		fmt.Fprintf(w, "error: %s\n", err)
	} else {
		fmt.Fprintln(w, "done")
	}
}

func (s *Server) liveFinish(w http.ResponseWriter, r *http.Request) {
	if !s.liveOn(w, r) {
		return
	}
	var req cluster.LiveFinishRequest
	if !decode(w, r, &req) {
		return
	}
	v, note, err := s.LocalLive.LiveFinish(r.Context(), r.PathValue("name"), req.Autostart, req.Firewall, req.Schedule)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": v, "note": note})
}

func (s *Server) liveAbort(w http.ResponseWriter, r *http.Request) {
	if !s.liveOn(w, r) {
		return
	}
	if err := s.LocalLive.LiveAbort(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) liveCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.liveOn(w, r) {
		return
	}
	if err := s.LocalLive.LiveCleanup(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeBig reads a JSON body of up to 16 MB (VM descriptions with UEFI variables).
func decodeBig(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// CPUSetter sets a VM's CPU type (cluster.VMs: computes the common CPU).
type CPUSetter interface {
	SetCPUType(ctx context.Context, name, typ string) (vm.VM, error)
}

// putCPU: {"type":"host"|"compatible"} from the dashboard, or an exact
// {"model","features"} from the master (cluster token).
func (s *Server) putCPU(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var body struct {
		Type     string   `json:"type"`
		Model    string   `json:"model"`
		Features []string `json:"features"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	if body.Type == "" {
		if s.LocalCPU == nil {
			writeErr(w, http.StatusNotFound, "not_found", "CPU types aren't available here")
			return
		}
		v, err := s.LocalCPU.SetCPU(r.Context(), name, virt.CPU{Model: body.Model, Features: body.Features})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
		return
	}
	cs, ok := s.VMs.(CPUSetter)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "CPU types aren't available here")
		return
	}
	v, err := cs.SetCPUType(r.Context(), name, body.Type)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// clusterCPU: this server's CPU description, for the master's common CPU.
func (s *Server) clusterCPU(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.LocalCPU == nil {
		writeErr(w, http.StatusNotFound, "not_found", "CPU types aren't available here")
		return
	}
	b, err := s.LocalCPU.HostCPU(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"xml": string(b)})
}

// verifyBackup: POST /backups/{name}/{id}/verify — a restore test (job).
func (s *Server) verifyBackup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.backupFor(w, r); !ok {
		return
	}
	v, ok := s.VMs.(interface {
		StartVerify(ctx context.Context, name, id string) (vm.Job, error)
	})
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "restore tests aren't available here")
		return
	}
	j, err := v.StartVerify(r.Context(), r.PathValue("name"), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}
