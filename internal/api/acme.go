package api

import (
	"encoding/json"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"p8sos/internal/tasks"
)

// Let's Encrypt for the dashboard (admins, Server page → Network):
// GET /node/acme → {"cert":{names,not_after,issuer}|null,"request":{domain,email,method}};
// POST /node/acme {"domain","email","method":"http"|"cloudflare","cf_token"} → task;
// DELETE /node/acme → task (back to the server's own certificate).

var reCFToken = regexp.MustCompile(`^[A-Za-z0-9_-]{20,100}$`)

type acmeReq struct {
	Domain string `json:"domain"`
	Email  string `json:"email,omitempty"`
	Method string `json:"method"`
}

func (s *Server) acmeSpec() (acmeReq, bool) {
	var a acmeReq
	b, err := os.ReadFile(s.Node.Tasks.ACMESpec())
	if err != nil || json.Unmarshal(b, &a) != nil {
		return a, false
	}
	return a, true
}

func (s *Server) getACME(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	out := map[string]any{"cert": nil, "request": nil}
	if s.CertInfo != nil {
		if in, ok := s.CertInfo(); ok {
			out["cert"] = in
		}
	}
	if a, ok := s.acmeSpec(); ok {
		out["request"] = a
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) postACME(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	var b struct {
		acmeReq
		CFToken string `json:"cf_token"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Domain = strings.ToLower(strings.TrimSpace(b.Domain))
	b.Email = strings.TrimSpace(b.Email)
	bad := func(m string) { writeErr(w, http.StatusBadRequest, "invalid", m) }
	switch {
	case !tasks.ValidDomain(b.Domain):
		bad("domain: a name like p8s.example.com that points to this server")
		return
	case b.Email != "" && func() bool { _, err := mail.ParseAddress(b.Email); return err != nil }():
		bad("email: an address for expiry warnings (or empty)")
		return
	case b.Method == "":
		b.Method = "http"
	}
	switch b.Method {
	case "http":
	case "cloudflare":
		if b.CFToken != "" {
			if !reCFToken.MatchString(b.CFToken) {
				bad("Cloudflare API token: the token text (Zone → DNS → Edit permission)")
				return
			}
			if err := os.MkdirAll("/etc/p8s", 0o755); err == nil {
				err = os.WriteFile(s.cfIni(), []byte("dns_cloudflare_api_token = "+b.CFToken+"\n"), 0o600)
				if err != nil {
					s.fail(w, r, err)
					return
				}
			}
		} else if _, err := os.Stat(s.cfIni()); err != nil {
			bad("Cloudflare: give an API token with DNS edit rights for the domain's zone")
			return
		}
	default:
		bad(`method: "http" (port 80 open to the internet) or "cloudflare" (DNS at Cloudflare, no ports)`)
		return
	}
	spec, _ := json.Marshal(b.acmeReq)
	p := s.Node.Tasks.ACMESpec()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := os.WriteFile(p, spec, 0o600); err != nil {
		s.fail(w, r, err)
		return
	}
	s.taskStarted(w, r, tasks.ACMEIssue, "")
}

func (s *Server) cfIni() string {
	if s.CFIni != "" {
		return s.CFIni
	}
	return "/etc/p8s/cloudflare.ini"
}

func (s *Server) deleteACME(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	a, _ := s.acmeSpec()
	_ = os.Remove(s.Node.Tasks.ACMESpec())
	s.taskStarted(w, r, tasks.ACMERemove, a.Domain)
}
