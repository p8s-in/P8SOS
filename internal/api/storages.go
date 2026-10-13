package api

import (
	"errors"
	"fmt"
	"net/http"

	"p8sos/internal/offsite"
	"p8sos/internal/storages"
	"p8sos/internal/vm"
)

// Network storage (NFS shares, iSCSI disks); admins only.
//
//	GET    /storages                     list with state and space
//	POST   /storages                     {"name","type":"nfs","server","export"[,"options"][,"backups":true]}
//	                                     {"name","type":"iscsi","portal","target","lun"[,"chap_user","chap_password"],"format":true}
//	POST   /storages/discover            {"portal"} -> {"targets":[...]}
//	POST   /storages/{name}/connect      mount again / log in again
//	DELETE /storages/{name}              disconnect (refused while VMs are on it)

func (s *Server) storagesOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.Storages == nil {
		writeErr(w, http.StatusNotFound, "not_found", "network storage isn't available on this server")
		return false
	}
	return true
}

// localVMs are this server's own VMs (storages belong to one server).
func (s *Server) localVMs(r *http.Request) ([]vm.VM, error) {
	if s.LocalVMs != nil {
		return s.LocalVMs(r.Context())
	}
	return s.VMs.List(r.Context())
}

func (s *Server) storageErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storages.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, storages.ErrConfirm):
		writeErr(w, http.StatusConflict, "confirm", err.Error())
	case errors.Is(err, storages.ErrExists):
		writeErr(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, storages.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) listStorages(w http.ResponseWriter, r *http.Request) {
	if !s.storagesOn(w, r) {
		return
	}
	l := s.Storages.List(r.Context())
	vms, _ := s.localVMs(r)
	type row struct {
		storages.Status
		VMs []string `json:"vms"`
	}
	out := []row{}
	for _, st := range l {
		x := row{Status: st, VMs: []string{}}
		for _, v := range vms {
			if v.Storage == st.Name {
				x.VMs = append(x.VMs, v.Name)
			}
		}
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"storages": out, "initiator": s.Storages.Initiator()})
}

func (s *Server) addStorage(w http.ResponseWriter, r *http.Request) {
	if !s.storagesOn(w, r) {
		return
	}
	var req struct {
		storages.Storage
		Format  bool `json:"format"`
		Backups bool `json:"backups"` // nfs: copy every backup here (an off-site target)
	}
	if !decode(w, r, &req) {
		return
	}
	st, err := s.Storages.Add(r.Context(), req.Storage, req.Format)
	if err != nil {
		s.storageErr(w, r, err)
		return
	}
	note := ""
	if req.Type == storages.NFS {
		note = fmt.Sprintf("ISOs put in %s on the NAS show up in the ISO library.", s.Storages.ISODir(req.Name))
		if req.Backups {
			if s.Offsite == nil {
				note += " Backup copies need off-site backups, which aren't available here."
			} else if _, err := s.Offsite.Save(offsite.Target{Name: req.Name, Type: "dir", Path: s.Storages.BackupDir(req.Name), Keep: 7, Enabled: true}); err != nil {
				note += " Couldn't set up backup copies: " + err.Error()
			} else {
				note += fmt.Sprintf(" Every backup is now also copied there (Backups → Off-site, target %q; it keeps 7 per VM).", req.Name)
			}
		}
	} else {
		note = "New VMs can now be put on it (Create → Storage)."
	}
	writeJSON(w, http.StatusCreated, map[string]any{"storage": st, "note": note})
}

func (s *Server) discoverISCSI(w http.ResponseWriter, r *http.Request) {
	if !s.storagesOn(w, r) {
		return
	}
	var req struct {
		Portal string `json:"portal"`
	}
	if !decode(w, r, &req) {
		return
	}
	l, err := s.Storages.Discover(r.Context(), req.Portal)
	if err != nil {
		s.storageErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": l})
}

func (s *Server) connectStorage(w http.ResponseWriter, r *http.Request) {
	if !s.storagesOn(w, r) {
		return
	}
	st, err := s.Storages.Connect(r.Context(), r.PathValue("name"))
	if err != nil {
		s.storageErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) removeStorage(w http.ResponseWriter, r *http.Request) {
	if !s.storagesOn(w, r) {
		return
	}
	name := r.PathValue("name")
	vms, err := s.localVMs(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var on []string
	for _, v := range vms {
		if v.Storage == name {
			on = append(on, v.Name)
		}
	}
	if len(on) > 0 {
		writeErr(w, http.StatusConflict, "conflict", fmt.Sprintf("VMs %v are on it; move or delete them first", on))
		return
	}
	st, err := s.Storages.Get(name)
	if err != nil {
		s.storageErr(w, r, err)
		return
	}
	if err := s.Storages.Remove(r.Context(), name); err != nil {
		s.storageErr(w, r, err)
		return
	}
	if st.Type == storages.NFS && s.Offsite != nil {
		_ = s.Offsite.Delete(name) // its backup-copy target, if it had one
	}
	w.WriteHeader(http.StatusNoContent)
}
