package api

import (
	"errors"
	"net/http"
	"strings"

	"p8sos/internal/tunnel"
)

// Public access through a Cloudflare Tunnel: the host's settings (admin) and
// websites published from VMs (owner or admin).

func (s *Server) tunnelOn(w http.ResponseWriter) bool {
	if s.Tunnel != nil {
		return true
	}
	writeErr(w, http.StatusNotFound, "not_found", "public access is not available on this host")
	return false
}

func (s *Server) tunnelErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tunnel.ErrNotSetUp):
		writeErr(w, http.StatusConflict, "not_set_up", err.Error())
	case errors.Is(err, tunnel.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, tunnel.ErrExists):
		writeErr(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, tunnel.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		s.fail(w, r, err)
	}
}

// getTunnel tells everyone whether public access is set up and under which
// domain (no secrets).
func (s *Server) getTunnel(w http.ResponseWriter, r *http.Request) {
	if !s.tunnelOn(w) {
		return
	}
	st := s.Tunnel.Status()
	if !who(r).Admin {
		st.TunnelID, st.TunnelName, st.Connector = "", "", nil
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) setupTunnel(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.tunnelOn(w) {
		return
	}
	var body struct {
		APIToken string `json:"api_token"`
		Domain   string `json:"domain"`
		Name     string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Name == "" {
		body.Name = "p8s-" + strings.ReplaceAll(s.issuerHost(), ".", "-")
	}
	st, _, err := s.Tunnel.Setup(r.Context(), body.APIToken, body.Domain, body.Name)
	if err != nil {
		s.tunnelErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// disconnectTunnel switches public access off (routes, DNS records, cloudflared).
func (s *Server) disconnectTunnel(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.tunnelOn(w) {
		return
	}
	if err := s.Tunnel.Disconnect(r.Context()); err != nil {
		s.tunnelErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setDashboardHost(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.tunnelOn(w) {
		return
	}
	var body struct {
		Name string `json:"name"` // e.g. "panel" -> panel.example.com; "" = off
	}
	if !decode(w, r, &body) {
		return
	}
	st, err := s.Tunnel.SetDashboard(r.Context(), body.Name)
	if err != nil {
		s.tunnelErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) listSites(w http.ResponseWriter, r *http.Request) {
	if !s.tunnelOn(w) {
		return
	}
	vmName := r.PathValue("name")
	if vmName != "" {
		if _, ok := s.vmFor(w, r); !ok {
			return
		}
	}
	owner := ""
	if p := who(r); !p.Admin && vmName == "" {
		owner = p.Name
	}
	l, err := s.Tunnel.Sites(vmName, owner)
	if err != nil {
		s.tunnelErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": l})
}

func (s *Server) addSite(w http.ResponseWriter, r *http.Request) {
	if !s.tunnelOn(w) {
		return
	}
	v, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Name  string `json:"name"` // label: shop -> shop.<domain>
		Port  int    `json:"port"`
		HTTPS bool   `json:"https"`
	}
	if !decode(w, r, &body) {
		return
	}
	p := who(r)
	if !p.Admin {
		defer s.quotaLock(p.Name)()
		if err := s.checkQuota(r.Context(), p, Usage{Sites: 1}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	site, err := s.Tunnel.Add(r.Context(), body.Name, v.Name, body.Port, body.HTTPS, v.Owner)
	if err != nil {
		s.tunnelErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, site)
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) {
	if !s.tunnelOn(w) {
		return
	}
	host := strings.ToLower(r.PathValue("host"))
	site, err := s.Tunnel.Get(host)
	if err == nil && !mayUse(who(r), site.Owner) {
		err = tunnel.ErrNotFound
	}
	if err == nil {
		err = s.Tunnel.Remove(r.Context(), host)
	}
	if err != nil {
		s.tunnelErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// issuerHost is the host part of the issuer name ("P8S OS (p8s-node)" -> p8s-node).
func (s *Server) issuerHost() string {
	i := s.issuer()
	if a, b := strings.Index(i, "("), strings.LastIndex(i, ")"); a >= 0 && b > a {
		return i[a+1 : b]
	}
	return "host"
}
