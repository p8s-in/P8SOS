package api

import (
	"errors"
	"net/http"

	"p8sos/internal/jobs"
	"p8sos/internal/license"
	"p8sos/internal/offsite"
)

// Off-site copies of backups (admins; needs a subscription with off-site backups).

func (s *Server) offsiteOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.Offsite == nil {
		writeErr(w, http.StatusNotFound, "not_found", "off-site backups are not available on this host")
		return false
	}
	return true
}

func (s *Server) offsiteFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, offsite.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, offsite.ErrNoSuch), errors.Is(err, offsite.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, jobs.ErrBusy):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		// target problems (wrong key, disk not mounted) are the admin's to fix: show them
		writeErr(w, http.StatusBadGateway, "target", err.Error())
	}
}

func (s *Server) getOffsite(w http.ResponseWriter, r *http.Request) {
	if !s.offsiteOn(w, r) {
		return
	}
	l, err := s.Offsite.Targets()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	key, _ := s.Offsite.PublicKey(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"targets": l, "ssh_public_key": key,
		"allowed": s.License.Allows(license.FeatOffsite)})
}

func (s *Server) putOffsite(w http.ResponseWriter, r *http.Request) {
	if !s.offsiteOn(w, r) || !s.needs(w, license.FeatOffsite, "Off-site backups") {
		return
	}
	var t offsite.Target
	if !decode(w, r, &t) {
		return
	}
	t.Name = r.PathValue("name")
	v, err := s.Offsite.Save(t)
	if err != nil {
		s.offsiteFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteOffsite(w http.ResponseWriter, r *http.Request) {
	if !s.offsiteOn(w, r) {
		return
	}
	if err := s.Offsite.Delete(r.PathValue("name")); err != nil {
		s.offsiteFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) offsiteAction(w http.ResponseWriter, r *http.Request) {
	if !s.offsiteOn(w, r) {
		return
	}
	name := r.PathValue("name")
	switch r.PathValue("action") {
	case "test":
		if err := s.Offsite.Test(r.Context(), name); err != nil {
			s.offsiteFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "sync":
		if !s.needs(w, license.FeatOffsite, "Off-site backups") {
			return
		}
		j, err := s.Offsite.StartSync(name)
		if err != nil {
			s.offsiteFail(w, r, err)
			return
		}
		s.noteJob(r, j.ID)
		writeJSON(w, http.StatusAccepted, j)
	case "fetch":
		var body struct{ Kind, Guest, ID string }
		if !decode(w, r, &body) {
			return
		}
		j, err := s.Offsite.StartFetch(name, body.Kind, body.Guest, body.ID)
		if err != nil {
			s.offsiteFail(w, r, err)
			return
		}
		s.noteJob(r, j.ID)
		writeJSON(w, http.StatusAccepted, j)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "unknown action")
	}
}

func (s *Server) offsiteBackups(w http.ResponseWriter, r *http.Request) {
	if !s.offsiteOn(w, r) {
		return
	}
	l, err := s.Offsite.RemoteBackups(r.Context(), r.PathValue("name"))
	if err != nil {
		s.offsiteFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": l})
}
