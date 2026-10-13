package api

import (
	"context"
	"net/http"
	"strconv"

	"p8sos/internal/vm"
)

// Data disks of a VM: POST /vms/{name}/disks {size_gb}, PATCH
// /vms/{name}/disks/{index} {size_gb} (grow, stopped), DELETE
// /vms/{name}/disks/{index} (stopped; its data is gone).

// DiskVMs is the data-disk part of the VM service (local or cluster).
type DiskVMs interface {
	AddDisk(ctx context.Context, name string, sizeGB int) (vm.VM, string, error)
	ResizeDisk(ctx context.Context, name string, index, sizeGB int) (vm.VM, error)
	RemoveDisk(ctx context.Context, name string, index int) (vm.VM, error)
}

// VMWithNote is a VM plus a hint for the user (what to do next).
type VMWithNote struct {
	vm.VM
	Note string `json:"note,omitempty"`
}

func (s *Server) disksAPI(w http.ResponseWriter) (DiskVMs, bool) {
	d, ok := s.VMs.(DiskVMs)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "extra disks aren't available here")
	}
	return d, ok
}

// diskChange checks a customer's plan and quota for growDisk more GB.
func (s *Server) diskChange(w http.ResponseWriter, r *http.Request, cur vm.VM, growGB int) (release func(), ok bool) {
	p := who(r)
	if p.Admin {
		return func() {}, true
	}
	if s.Billing != nil {
		if _, billed := s.Billing.Sub(cur.Name); billed {
			writeErr(w, http.StatusBadRequest, "invalid", "this server is on a plan; to add disk space, ask support")
			return nil, false
		}
	}
	unlock := s.quotaLock(p.Name)
	if growGB > 0 {
		if err := s.checkQuota(r.Context(), p, Usage{DiskGB: growGB}); err != nil {
			unlock()
			s.fail(w, r, err)
			return nil, false
		}
	}
	return unlock, true
}

func (s *Server) addDisk(w http.ResponseWriter, r *http.Request) {
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	d, ok := s.disksAPI(w)
	if !ok {
		return
	}
	var req struct {
		SizeGB int `json:"size_gb"`
	}
	if !decode(w, r, &req) {
		return
	}
	if s.HA != nil {
		if _, protected := s.HA.Config(r.Context(), cur.Name); protected {
			writeErr(w, http.StatusConflict, "conflict", "high availability copies only VMs with one disk for now; turn HA off for this VM first")
			return
		}
	}
	release, ok := s.diskChange(w, r, cur, req.SizeGB)
	if !ok {
		return
	}
	defer release()
	v, note, err := d.AddDisk(r.Context(), cur.Name, req.SizeGB)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, VMWithNote{v, note})
}

func diskIndex(w http.ResponseWriter, r *http.Request) (int, bool) {
	i, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || i < 1 || i > 8 {
		writeErr(w, http.StatusNotFound, "not_found", "disk must be 1-8")
		return 0, false
	}
	return i, true
}

func (s *Server) resizeDisk(w http.ResponseWriter, r *http.Request) {
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	d, ok := s.disksAPI(w)
	if !ok {
		return
	}
	i, ok := diskIndex(w, r)
	if !ok {
		return
	}
	var req struct {
		SizeGB int `json:"size_gb"`
	}
	if !decode(w, r, &req) {
		return
	}
	grow := 0
	for _, x := range cur.Disks {
		if x.Index == i && req.SizeGB > x.SizeGB {
			grow = req.SizeGB - x.SizeGB
		}
	}
	release, ok := s.diskChange(w, r, cur, grow)
	if !ok {
		return
	}
	defer release()
	v, err := d.ResizeDisk(r.Context(), cur.Name, i, req.SizeGB)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) removeDisk(w http.ResponseWriter, r *http.Request) {
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	d, ok := s.disksAPI(w)
	if !ok {
		return
	}
	i, ok := diskIndex(w, r)
	if !ok {
		return
	}
	v, err := d.RemoveDisk(r.Context(), cur.Name, i)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
