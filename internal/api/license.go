package api

import (
	"errors"
	"fmt"
	"net/http"

	"p8sos/internal/license"
)

// Subscriptions (P8S OS licence) — what this server's tier unlocks.

// needs answers 402 when this server's subscription doesn't unlock a feature.
func (s *Server) needs(w http.ResponseWriter, feature, what string) bool {
	if s.License.Allows(feature) {
		return true
	}
	writeErr(w, http.StatusPaymentRequired, "subscription", what+" needs a P8S OS subscription; see Subscription in the dashboard")
	return false
}

// userLimitReached reports whether a Home server already has its maximum accounts.
func (s *Server) userLimitReached(w http.ResponseWriter) bool {
	if s.License == nil || s.Users == nil || s.License.Allows(license.FeatUsers) || s.Users.Count() < license.HomeUsers {
		return false
	}
	writeErr(w, http.StatusPaymentRequired, "subscription",
		fmt.Sprintf("P8S OS Home allows %d user accounts; a Business subscription removes the limit", license.HomeUsers))
	return true
}

func (s *Server) licenseOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.License == nil {
		writeErr(w, http.StatusNotFound, "not_found", "subscriptions are not used on this host")
		return false
	}
	return true
}

func (s *Server) getLicense(w http.ResponseWriter, r *http.Request) {
	if s.licenseOn(w, r) {
		writeJSON(w, http.StatusOK, s.License.Status())
	}
}

func (s *Server) putLicense(w http.ResponseWriter, r *http.Request) {
	if !s.licenseOn(w, r) {
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &body) {
		return
	}
	st, err := s.License.Install(body.Key)
	if err != nil {
		if errors.Is(err, license.ErrKey) || errors.Is(err, license.ErrServer) {
			writeErr(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) deleteLicense(w http.ResponseWriter, r *http.Request) {
	if !s.licenseOn(w, r) {
		return
	}
	if err := s.License.Remove(); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.License.Status())
}

func (s *Server) checkLicense(w http.ResponseWriter, r *http.Request) {
	if !s.licenseOn(w, r) {
		return
	}
	if err := s.License.Check(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, "unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.License.Status())
}
