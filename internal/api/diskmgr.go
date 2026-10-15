package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"p8sos/internal/cluster"
	"p8sos/internal/diskmgr"
)

// Server page → Disks (admins): the server's physical disks, wiping and
// protecting them, and ZFS pools made from empty disks for VM disks.
// ?node=ID on the master does it on that cluster server.

func (s *Server) disksOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.DiskMgr == nil {
		writeErr(w, http.StatusNotFound, "not_found", "disk management isn't available here")
		return false
	}
	return true
}

func (s *Server) diskFail(w http.ResponseWriter, err error) {
	var ae *cluster.APIError
	switch {
	case errors.As(err, &ae):
		code := ae.Status
		if code == 0 {
			code = http.StatusBadGateway
		}
		writeErr(w, code, ae.Code, ae.Message)
	case errors.Is(err, diskmgr.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, diskmgr.ErrConfirm):
		writeErr(w, http.StatusConflict, "confirm", err.Error())
	case errors.Is(err, diskmgr.ErrRefused):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, diskmgr.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "disks", err.Error())
	}
}

// diskAdmin: changing disks is for admins only (operators may look).
func (s *Server) diskAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.disksOn(w, r) {
		return false
	}
	if p := who(r); p.Role != "admin" && !p.Cluster {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin can change disks")
		return false
	}
	return true
}

// diskRemote forwards to another cluster server; ok=false: do it here.
func (s *Server) diskRemote(w http.ResponseWriter, r *http.Request, method, path string, in any) bool {
	id := r.URL.Query().Get("node")
	if id == "" || s.Cluster == nil || id == s.Cluster.SelfID() {
		return false
	}
	if p := who(r); p.Cluster {
		writeErr(w, http.StatusBadRequest, "invalid", "no forwarding of forwarded calls")
		return true
	}
	for _, n := range s.Cluster.Workers() {
		if n.ID == id || n.Name == id {
			var out any
			if err := s.Cluster.Call(r.Context(), n, method, path, in, &out); err != nil {
				s.diskFail(w, err)
				return true
			}
			writeJSON(w, http.StatusOK, out)
			return true
		}
	}
	writeErr(w, http.StatusNotFound, "not_found", "no cluster server "+id+" (this must be the master)")
	return true
}

func (s *Server) getDisks(w http.ResponseWriter, r *http.Request) {
	if !s.disksOn(w, r) || s.diskRemote(w, r, "GET", "/node/disks", nil) {
		return
	}
	l, err := s.DiskMgr.List(r.Context(), r.URL.Query().Get("smart") != "0")
	if err != nil {
		s.diskFail(w, err)
		return
	}
	if l == nil {
		l = []diskmgr.Disk{}
	}
	pools := s.DiskMgr.PoolsInfo(r.Context())
	if pools == nil {
		pools = []diskmgr.LocalPool{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"disks": l, "pools": pools})
}

func (s *Server) diskAction(w http.ResponseWriter, r *http.Request) {
	if !s.diskAdmin(w, r) {
		return
	}
	name, action := r.PathValue("disk"), r.PathValue("action")
	var body struct {
		Confirm string `json:"confirm"`
		On      bool   `json:"on"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	if s.diskRemote(w, r, "POST", "/node/disks/"+url.PathEscape(name)+"/"+action, body) {
		return
	}
	var err error
	switch action {
	case "wipe":
		err = s.DiskMgr.Wipe(r.Context(), name, body.Confirm)
	case "protect":
		err = s.DiskMgr.Protect(r.Context(), name, body.On)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "unknown action "+action)
		return
	}
	if err != nil {
		s.diskFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) createDiskPool(w http.ResponseWriter, r *http.Request) {
	if !s.diskAdmin(w, r) {
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Layout string   `json:"layout"`
		Disks  []string `json:"disks"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.diskRemote(w, r, "POST", "/node/pools", body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	p, err := s.DiskMgr.CreatePool(r.Context(), body.Name, body.Layout, body.Disks)
	if err != nil {
		s.diskFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": p,
		"note": "Storage " + p.Name + " is ready: pick it for new VMs (Storage) or move disks onto it (Hardware → Disk storage)."})
}

func (s *Server) deleteDiskPool(w http.ResponseWriter, r *http.Request) {
	if !s.diskAdmin(w, r) {
		return
	}
	name, confirm := r.PathValue("name"), r.URL.Query().Get("confirm")
	if s.diskRemote(w, r, "DELETE", "/node/pools/"+url.PathEscape(name)+"?confirm="+url.QueryEscape(confirm), nil) {
		return
	}
	if err := s.DiskMgr.DestroyPool(r.Context(), name, confirm); err != nil {
		s.diskFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
