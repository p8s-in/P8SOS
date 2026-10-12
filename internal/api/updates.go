package api

import (
	"errors"
	"net/http"

	"p8sos/internal/updates"
)

// P8S OS updates from the update server (admins).

func (s *Server) updatesOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.Updates == nil {
		writeErr(w, http.StatusNotFound, "not_found", "updates are not managed on this host")
		return false
	}
	return true
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request) {
	if s.updatesOn(w, r) {
		writeJSON(w, http.StatusOK, s.Updates.Status(r.Context()))
	}
}

func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	if !s.updatesOn(w, r) {
		return
	}
	if err := s.Updates.Check(r.Context()); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, updates.ErrOff) {
			code = http.StatusConflict
		}
		writeErr(w, code, "update_server", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Updates.Status(r.Context()))
}

func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.updatesOn(w, r) {
		return
	}
	unit, err := s.Updates.Install(r.Context())
	switch {
	case errors.Is(err, updates.ErrBusy), errors.Is(err, updates.ErrNone), errors.Is(err, updates.ErrOff):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"unit": unit, "status": s.Updates.Status(r.Context())})
}

func (s *Server) setUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if !s.updatesOn(w, r) {
		return
	}
	var body struct {
		Channel string `json:"channel"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Updates.SetChannel(body.Channel); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Updates.Status(r.Context()))
}
