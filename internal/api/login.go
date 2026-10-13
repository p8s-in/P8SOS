package api

import (
	"net/http"

	"p8sos/internal/creds"
)

// Login details of VMs and containers: P8S makes a root password when none is
// given (like a VPS provider) and shows it to the owner and admins in the
// dashboard and at GET /vms/{name}/login, /containers/{name}/login.

// PasswordLength of generated root passwords.
const PasswordLength = 16

// loginView is what the API shows.
type loginView struct {
	Known    bool   `json:"known"` // false: P8S didn't make or set a password for it
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	SSH      bool   `json:"ssh_password"` // root may SSH in with it
	SetAt    string `json:"set_at,omitempty"`
	Note     string `json:"note,omitempty"`
}

func viewOf(l creds.Login, ok bool) loginView {
	if !ok {
		return loginView{Note: "No password stored by P8S: use SSH keys, or set one with POST .../password (generate: true makes one)."}
	}
	return loginView{Known: true, User: l.User, Password: l.Password, SSH: l.SSH, SetAt: l.SetAt,
		Note: "Changed inside the guest since? P8S doesn't see that; set a new one here to keep this current."}
}

// autoPassword fills in a root password when the request has none (and P8S
// makes them: Server.AutoPasswords). The cluster master already decided for
// the workers, so requests from it are left alone.
func (s *Server) autoPassword(r *http.Request, pw *string) bool {
	if !s.AutoPasswords || *pw != "" || who(r).Cluster {
		return false
	}
	*pw = creds.Generate(PasswordLength)
	return true
}

func (s *Server) vmLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); ok {
		writeJSON(w, http.StatusOK, viewOf(s.Creds.Get("vm", r.PathValue("name"))))
	}
}

func (s *Server) ctLogin(w http.ResponseWriter, r *http.Request) {
	if c, ok := s.ctFor(w, r); ok {
		writeJSON(w, http.StatusOK, viewOf(s.Creds.Get("ct", c.Name)))
	}
}

// rememberPassword keeps root's password after it was set through P8S (other
// users' passwords aren't kept: the panel shows root's login).
func (s *Server) rememberPassword(kind, name, user, password string) {
	if user != "root" {
		return
	}
	old, _ := s.Creds.Get(kind, name)
	ssh := old.SSH || kind == "ct"
	if err := s.Creds.Set(kind, name, creds.Login{User: "root", Password: password, SSH: ssh}); err != nil && s.Log != nil {
		s.Log.Error("store login", "guest", name, "err", err)
	}
}
