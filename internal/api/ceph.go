package api

import (
	"errors"
	"net/http"
	"strconv"

	"p8sos/internal/ceph"
)

// Ceph storage across the servers (admins only; Server → Ceph).
//
//	GET    /ceph                 every server's Ceph state, the cluster's health, advice
//	POST   /ceph/install         {"node"}             install Ceph on a server ("" = this one)
//	POST   /ceph/init            {"network"}          start Ceph here: first monitor + manager
//	POST   /ceph/mons            {"node"}             a monitor on a server
//	POST   /ceph/mgrs            {"node"}             a manager on a server
//	POST   /ceph/osds            {"node","disk"}      give an empty disk to Ceph
//	DELETE /ceph/osds/{id}                            take it out (again, when Ceph moved its data: remove)
//	POST   /ceph/pool            {"size"}             the pool for VM disks (0 = 3 copies, fewer with fewer servers)
//
// Every server answers these for itself (the master calls them):
//
//	GET  /ceph/node              this server's state
//	POST /ceph/node/config       the configuration and keys (ceph.Bundle)
//	POST /ceph/node/{action}     install, init, mon, mgr, osd, osd-remove (ceph.NodeRequest)

func (s *Server) cephOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.CephLocal == nil {
		writeErr(w, http.StatusNotFound, "not_found", "Ceph isn't available on this server")
		return false
	}
	return true
}

func (s *Server) cephClusterOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.cephOn(w, r) {
		return false
	}
	if s.Ceph == nil {
		writeErr(w, http.StatusConflict, "conflict", "this server is a worker: manage Ceph on the cluster's master")
		return false
	}
	return true
}

func (s *Server) cephErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ceph.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, ceph.ErrConflict), errors.Is(err, ceph.ErrBusy):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) getCeph(w http.ResponseWriter, r *http.Request) {
	if !s.cephClusterOn(w, r) {
		return
	}
	o := s.Ceph.Overview(r.Context())
	advice := o.Advice()
	if advice == nil {
		advice = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceph": o, "advice": advice, "packages": ceph.Packages})
}

func (s *Server) cephStep(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cephClusterOn(w, r) {
			return
		}
		var req struct {
			Node    string `json:"node"`
			Network string `json:"network"`
			Disk    string `json:"disk"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := s.Ceph.Run(r.Context(), req.Node, action, ceph.NodeRequest{Network: req.Network, Disk: req.Disk}); err != nil {
			s.cephErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"started": action})
	}
}

func (s *Server) removeCephOSD(w http.ResponseWriter, r *http.Request) {
	if !s.cephClusterOn(w, r) {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 {
		writeErr(w, http.StatusBadRequest, "invalid", "bad OSD number")
		return
	}
	if err := s.Ceph.RemoveOSD(r.Context(), id); err != nil {
		s.cephErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": ceph.ActOSDRemove})
}

func (s *Server) createCephPool(w http.ResponseWriter, r *http.Request) {
	if !s.cephClusterOn(w, r) {
		return
	}
	var req struct {
		Size int `json:"size"`
	}
	if !decode(w, r, &req) {
		return
	}
	size, err := s.Ceph.CreatePool(r.Context(), req.Size)
	if err != nil {
		s.cephErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": ceph.Pool, "size": size})
}

// --- this server's own part ---

func (s *Server) getCephNode(w http.ResponseWriter, r *http.Request) {
	if !s.cephOn(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.CephLocal.Info(r.Context()))
}

func (s *Server) setCephConfig(w http.ResponseWriter, r *http.Request) {
	if !s.cephOn(w, r) {
		return
	}
	var b ceph.Bundle
	if !decode(w, r, &b) {
		return
	}
	if err := s.CephLocal.SetBundle(b); err != nil {
		s.cephErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hash": b.Hash()})
}

func (s *Server) cephNodeStep(w http.ResponseWriter, r *http.Request) {
	if !s.cephOn(w, r) {
		return
	}
	var req ceph.NodeRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.CephLocal.Do(r.PathValue("action"), req); err != nil {
		s.cephErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": r.PathValue("action")})
}
