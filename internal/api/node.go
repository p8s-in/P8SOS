package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"p8sos/internal/hostinfo"
	"p8sos/internal/tasks"
)

// The Server page (admins): storage, OS templates, home network, Debian
// updates, reboot, and the log of those tasks.

func (s *Server) nodeOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.Node == nil {
		writeErr(w, http.StatusNotFound, "not_found", "server tasks are not available on this host")
		return false
	}
	return true
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	if s.nodeOn(w, r) {
		writeJSON(w, http.StatusOK, s.Node.Overview(r.Context()))
	}
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	var body struct {
		Kind string `json:"kind"`
		Arg  string `json:"arg"`
	}
	if !decode(w, r, &body) {
		return
	}
	switch body.Kind {
	case tasks.TemplateURL:
		writeErr(w, http.StatusBadRequest, "invalid", "use POST /api/v1/node/templates/custom")
		return
	case tasks.ACMEIssue, tasks.ACMERemove:
		writeErr(w, http.StatusBadRequest, "invalid", "use POST / DELETE /api/v1/node/acme")
		return
	case tasks.CTFromImage:
		l, err := s.Node.CTImages(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, "unavailable", "the image list could not be read: "+err.Error())
			return
		}
		ok := false
		for _, im := range l {
			ok = ok || (im.ID == body.Arg && !im.Installed)
		}
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid", "unknown or already added image: "+body.Arg)
			return
		}
	}
	if body.Kind == tasks.Template || body.Kind == tasks.CTTemplate {
		o := s.Node.Overview(r.Context())
		cat := o.Catalog
		if body.Kind == tasks.CTTemplate {
			cat = o.CTCatalog
		}
		ok := false
		for _, c := range cat {
			ok = ok || (c.Name == body.Arg && !c.Installed)
		}
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid", "unknown or already added template: "+body.Arg)
			return
		}
	}
	s.taskStarted(w, r, body.Kind, body.Arg)
}

func (s *Server) taskStarted(w http.ResponseWriter, r *http.Request, kind, arg string) {
	t, err := s.Node.Tasks.Start(r.Context(), kind, arg, who(r).Name)
	switch {
	case errors.Is(err, tasks.ErrBusy):
		writeErr(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, tasks.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusAccepted, t)
	}
}

// renameNode gives this server a new name (Server page → Rename).
func (s *Server) renameNode(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &b) {
		return
	}
	s.taskStarted(w, r, tasks.Rename, strings.ToLower(strings.TrimSpace(b.Name)))
}

// getCTImages: every container image the image server offers (Server page →
// Container templates → Add another).
func (s *Server) getCTImages(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	l, err := s.Node.CTImages(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "unavailable", "the image list could not be read (internet?): "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": l})
}

var (
	reTplName  = regexp.MustCompile(`^[a-z][a-z0-9.-]{1,30}$`)
	reTplLogin = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reVariant  = regexp.MustCompile(`^[a-z0-9.]{1,40}$`)
	reHash     = regexp.MustCompile(`^(sha256:[0-9a-fA-F]{64}|sha512:[0-9a-fA-F]{128})$`)
)

// customTemplate: a VM template from any cloud image address (Server page →
// OS templates → Add another). The description goes to a file the task's
// script reads, so the address never passes through a command line.
func (s *Server) customTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	var b struct {
		Name     string `json:"name"`
		Label    string `json:"label"`
		URL      string `json:"url"`
		Login    string `json:"login"`
		Variant  string `json:"variant"`
		Checksum string `json:"checksum"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Name, b.Label, b.URL = strings.TrimSpace(b.Name), strings.TrimSpace(b.Label), strings.TrimSpace(b.URL)
	b.Login, b.Variant, b.Checksum = strings.TrimSpace(b.Login), strings.TrimSpace(b.Variant), strings.TrimSpace(b.Checksum)
	bad := func(m string) { writeErr(w, http.StatusBadRequest, "invalid", m) }
	u, err := url.Parse(b.URL)
	switch {
	case !reTplName.MatchString(b.Name):
		bad("name: 2-31 characters, lowercase letters, digits, dots and hyphens; start with a letter")
	case len(b.Label) > 60 || strings.ContainsAny(b.Label, "<>\n\r"):
		bad("label: up to 60 characters")
	case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(b.URL) > 500 || strings.ContainsAny(b.URL, " \"'<>"):
		bad("image address: an http(s) link to a cloud image (.qcow2, .img, .raw, also .xz / .gz)")
	case b.Login != "" && !reTplLogin.MatchString(b.Login):
		bad("login user: lowercase letters, digits, _ and -")
	case b.Variant != "" && !reVariant.MatchString(b.Variant):
		bad("OS type: like debian13, ubuntu24.04, rocky9 (or leave it empty)")
	case b.Checksum != "" && !reHash.MatchString(b.Checksum) && !strings.HasPrefix(b.Checksum, "https://") && !strings.HasPrefix(b.Checksum, "http://"):
		bad("checksum: the address of the SHA256SUMS / CHECKSUM file, or sha256:HASH / sha512:HASH, or empty")
	default:
		for _, t := range s.templateList(r.Context()) {
			if t.Name == b.Name {
				bad("a template " + b.Name + " exists already")
				return
			}
		}
		for _, c := range s.Node.Overview(r.Context()).Catalog {
			if c.Name == b.Name {
				bad(b.Name + " is on the list above: add it there")
				return
			}
		}
		if b.Login == "" {
			b.Login = "root"
		}
		spec, _ := json.Marshal(b)
		p := s.Node.Tasks.SpecPath(b.Name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := os.WriteFile(p, spec, 0o600); err != nil {
			s.fail(w, r, err)
			return
		}
		s.taskStarted(w, r, tasks.TemplateURL, b.Name)
	}
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	if s.nodeOn(w, r) {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": s.Node.Tasks.List(r.Context())})
	}
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	t, ok := s.Node.Tasks.Get(r.Context(), r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// getMemory: where the server's RAM goes (Server page → Memory).
func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	amax, amin := hostinfo.ARCLimits()
	writeJSON(w, http.StatusOK, map[string]any{
		"mem": hostinfo.ReadMem(), "top": hostinfo.TopProcs(12),
		"zfs_cache_max_mib": amax, "zfs_cache_min_mib": amin,
	})
}
