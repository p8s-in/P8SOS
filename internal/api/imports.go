package api

import (
	"context"
	"io"
	"net/http"
	"time"

	"p8sos/internal/imports"
	"p8sos/internal/vm"
)

// Importing VMs from Proxmox backups and disk images (admins). The files live
// in the server's import folder: upload here, or copy them there with scp.

// Importer is the local VM service's import side.
type Importer interface {
	ListImports() ([]vm.ImportFile, error)
	ImportPlan(name string) (imports.Plan, error)
	SaveImport(name string, r io.Reader) (vm.ImportFile, error)
	DeleteImport(name string) error
	StartImport(ctx context.Context, r vm.ImportRequest) (vm.Job, error)
}

func (s *Server) importsOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.Imports == nil {
		writeErr(w, http.StatusNotFound, "not_found", "imports are not available on this host")
		return false
	}
	return true
}

func (s *Server) listImports(w http.ResponseWriter, r *http.Request) {
	if !s.importsOn(w, r) {
		return
	}
	l, err := s.Imports.ListImports()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": l, "folder": s.ImportFolder})
}

func (s *Server) importPlan(w http.ResponseWriter, r *http.Request) {
	if !s.importsOn(w, r) {
		return
	}
	p, err := s.Imports.ImportPlan(r.PathValue("file"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) uploadImport(w http.ResponseWriter, r *http.Request) {
	if !s.importsOn(w, r) {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(12 * time.Hour))
	_ = rc.SetWriteDeadline(time.Now().Add(12 * time.Hour))
	f, err := s.Imports.SaveImport(r.PathValue("file"), r.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) deleteImport(w http.ResponseWriter, r *http.Request) {
	if !s.importsOn(w, r) {
		return
	}
	if err := s.Imports.DeleteImport(r.PathValue("file")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startImport(w http.ResponseWriter, r *http.Request) {
	if !s.importsOn(w, r) {
		return
	}
	var req vm.ImportRequest
	if !decode(w, r, &req) {
		return
	}
	req.File = r.PathValue("file")
	if s.CTs != nil && req.Name != "" && s.CTs.Exists(req.Name) {
		writeErr(w, http.StatusConflict, "exists", "a container is already called "+req.Name)
		return
	}
	j, err := s.Imports.StartImport(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}
