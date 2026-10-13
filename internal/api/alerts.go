package api

import (
	"errors"
	"net/http"

	"p8sos/internal/monitor"
)

// Health & alerts (admins): what is watched, what is wrong, where alerts go.

func (s *Server) alertsOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.Monitor == nil {
		writeErr(w, http.StatusNotFound, "not_found", "monitoring is not available on this host")
		return false
	}
	return true
}

func (s *Server) getAlerts(w http.ResponseWriter, r *http.Request) {
	if s.alertsOn(w, r) {
		writeJSON(w, http.StatusOK, s.Monitor.Status())
	}
}

func (s *Server) putAlertSettings(w http.ResponseWriter, r *http.Request) {
	if !s.alertsOn(w, r) {
		return
	}
	var body monitor.Settings
	if !decode(w, r, &body) {
		return
	}
	set, err := s.Monitor.SetSettings(body)
	if errors.Is(err, monitor.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) testAlert(w http.ResponseWriter, r *http.Request) {
	if !s.alertsOn(w, r) {
		return
	}
	if err := s.Monitor.Test(); errors.Is(err, monitor.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
