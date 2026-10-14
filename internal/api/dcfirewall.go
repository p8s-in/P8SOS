package api

import (
	"context"
	"net/http"
	"strings"

	"p8sos/internal/firewall"
)

// The datacenter firewall (admins): GET /firewall → {sets, block, allow};
// PUT /firewall → saved and applied here, and on the master sent to every
// worker (they apply it to their own VMs).

func (s *Server) getDCFirewall(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.DCFirewall == nil {
		writeErr(w, http.StatusNotFound, "not_found", "the datacenter firewall isn't available here")
		return
	}
	writeJSON(w, http.StatusOK, s.DCFirewall.Load())
}

func (s *Server) putDCFirewall(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if !p.Admin || (p.Role != "admin" && !p.Cluster) {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin can change the datacenter firewall")
		return
	}
	if s.DCFirewall == nil {
		writeErr(w, http.StatusNotFound, "not_found", "the datacenter firewall isn't available here")
		return
	}
	var g firewall.Global
	if !decode(w, r, &g) {
		return
	}
	saved, err := s.DCFirewall.Save(g)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), firewall.ErrInvalid.Error()+": "))
		return
	}
	note := ""
	if fa, ok := s.VMs.(interface{ ApplyFirewalls(context.Context) error }); ok {
		if err := fa.ApplyFirewalls(r.Context()); err != nil {
			note = "saved, but applying it here failed: " + err.Error()
		}
	}
	// the master hands it to every worker
	if s.Cluster != nil && !p.Cluster && s.Cluster.Role() == "master" {
		self := s.Cluster.SelfID()
		var failed []string
		for _, n := range s.Cluster.Workers() {
			if n.ID == self {
				continue
			}
			if err := s.Cluster.Call(r.Context(), n, "PUT", "/firewall", saved, nil); err != nil {
				failed = append(failed, n.Name)
			}
		}
		if len(failed) > 0 {
			note = "saved; these servers couldn't be reached and keep their old rules until it is saved again: " + strings.Join(failed, ", ")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"firewall": saved, "note": note})
}
