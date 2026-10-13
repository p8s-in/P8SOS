package api

import (
	"errors"
	"net/http"
	"net/url"

	"p8sos/internal/sso"
	"p8sos/internal/users"
)

// Company login (OpenID Connect). The sign-in itself happens in the browser:
// /sso/start → the provider → /sso/callback → the dashboard with a session.

func (s *Server) ssoStart(w http.ResponseWriter, r *http.Request) {
	if s.SSO == nil || !s.usersReady() {
		http.Redirect(w, r, "/#sso_error="+url.QueryEscape("company login is not set up"), http.StatusFound)
		return
	}
	u, err := s.SSO.Start(r.Context())
	if err != nil {
		http.Redirect(w, r, "/#sso_error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) usersReady() bool { return s.Users != nil && s.Sessions != nil }

func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	refuse := func(msg string) {
		s.audit(r, "company-login", http.StatusForbidden, "sign-in refused: "+msg)
		http.Redirect(w, r, "/#sso_error="+url.QueryEscape(msg), http.StatusFound)
	}
	if s.SSO == nil || !s.usersReady() {
		refuse("company login is not set up")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		refuse("the provider said: " + e + " " + q.Get("error_description"))
		return
	}
	id, err := s.SSO.Finish(r.Context(), q.Get("state"), q.Get("code"))
	if err != nil {
		refuse(err.Error())
		return
	}
	u, err := s.Users.BySSO(id.Key)
	if errors.Is(err, users.ErrNotFound) {
		// an account with the same (verified) email is linked once
		if id.Verified && id.Email != "" {
			if eu, err2 := s.Users.ByEmail(id.Email); err2 == nil && eu.Verified && eu.SSO == "" {
				_, err = s.Users.Update(eu.Name, func(x *users.User) error { x.SSO = id.Key; return nil })
				u = eu
			}
		}
		if u.Name == "" {
			if id.Role == "" {
				refuse("there is no account for " + firstOf(id.Email, id.Username) + "; ask an admin to add you to a P8S group")
				return
			}
			q := users.Quota{}
			if id.Role == users.Customer {
				q = users.DefaultQuota
			}
			name := s.Users.FreeName(firstOf(id.Username, "user"))
			free := id.Email != "" && !s.Users.EmailTaken(id.Email, name)
			if _, err = s.Users.Add(name, id.Role, users.RandomPassword(), q); err == nil {
				_, err = s.Users.Update(name, func(x *users.User) error {
					x.SSO, x.FullName = id.Key, id.Name
					if free {
						x.Email, x.Verified = id.Email, id.Verified
					}
					return nil
				})
			}
			if err == nil {
				u, err = s.Users.Get(name)
				s.audit(r, name, http.StatusCreated, "account made by company login ("+id.Role+")")
			}
		}
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	if u.Disabled {
		refuse("the account " + u.Name + " is disabled")
		return
	}
	// the company's groups decide the role (the last admin is never demoted here)
	if id.Role != "" && id.Role != u.Role && !(u.Role == users.Admin && s.Users.AdminCount() <= 1) {
		if _, err := s.Users.Update(u.Name, func(x *users.User) error { x.Role = id.Role; return nil }); err == nil {
			s.audit(r, u.Name, http.StatusOK, "role "+u.Role+" → "+id.Role+" (company groups)")
		}
	}
	tok, _ := s.Sessions.New(u.Name)
	s.audit(r, u.Name, http.StatusOK, "signed in (company login)")
	http.Redirect(w, r, "/#sso="+url.QueryEscape(tok), http.StatusFound)
}

func firstOf(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

type ssoView struct {
	sso.Settings
	HasSecret   bool   `json:"has_secret"`
	CallbackURL string `json:"callback_url"`
}

func (s *Server) getSSO(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.SSO == nil {
		writeErr(w, http.StatusNotFound, "not_found", "company login isn't available")
		return
	}
	st := s.SSO.Settings()
	v := ssoView{Settings: st, HasSecret: st.ClientSecret != "", CallbackURL: st.CallbackURL()}
	v.ClientSecret = ""
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) putSSO(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.SSO == nil {
		writeErr(w, http.StatusNotFound, "not_found", "company login isn't available")
		return
	}
	var body sso.Settings
	if !decode(w, r, &body) {
		return
	}
	st, err := s.SSO.SetSettings(body)
	if errors.Is(err, sso.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	v := ssoView{Settings: st, HasSecret: st.ClientSecret != "", CallbackURL: st.CallbackURL()}
	v.ClientSecret = ""
	writeJSON(w, http.StatusOK, v)
}
