package api

import (
	"net/http"

	"p8sos/internal/users"
)

// Per-VM permissions: GET /vms/{name}/shares → {"shares":[{"user","level"}]};
// PUT /vms/{name}/shares {"user":"bob","level":"viewer"|"user"|""} (owner or staff).

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	if s.Shares == nil {
		writeJSON(w, http.StatusOK, map[string]any{"shares": []users.Share{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": s.Shares.List(r.PathValue("name"))})
}

func (s *Server) putShare(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	p := who(r)
	if !p.Admin && v.Owner != p.Name { // a shared user can't share further
		writeErr(w, http.StatusForbidden, "forbidden", "only the VM's owner or an admin can share it")
		return
	}
	if s.Shares == nil || s.Users == nil {
		writeErr(w, http.StatusNotFound, "not_found", "sharing needs user accounts")
		return
	}
	var body struct {
		User  string `json:"user"`
		Level string `json:"level"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Level != "" {
		u, err := s.Users.Get(body.User)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if u.Name == v.Owner {
			writeErr(w, http.StatusBadRequest, "invalid", u.Name+" owns this VM already")
			return
		}
		if users.Staff(u.Role) {
			writeErr(w, http.StatusBadRequest, "invalid", u.Name+" is staff and sees every VM already")
			return
		}
	}
	if err := s.Shares.Set(v.Name, body.User, body.Level); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": s.Shares.List(v.Name)})
}
