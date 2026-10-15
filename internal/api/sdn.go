package api

import (
	"context"
	"net/http"
	"strings"

	"p8sos/internal/sdn"
	"p8sos/internal/vm"
)

// Virtual networks across the cluster's servers (internal/sdn).
// GET /sdn: every signed-in user (to pick one for a VM).
// PUT /sdn {vnets}: admins; saved and made here, and on the master sent to
// every worker. A network that VMs still use can't be removed.

func (s *Server) getSDN(w http.ResponseWriter, r *http.Request) {
	if s.VNets == nil {
		writeErr(w, http.StatusNotFound, "not_found", "virtual networks aren't available here")
		return
	}
	c := s.VNets.Store.Load()
	gw := s.Cluster == nil || s.Cluster.Role() != "worker"
	writeJSON(w, http.StatusOK, map[string]any{"vnets": c.VNets, "gateway_here": gw, "mtu": sdn.MTU})
}

// vnetUsers lists the VMs on each virtual network.
func (s *Server) vnetUsers(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	l, err := s.VMs.List(ctx)
	if err != nil {
		return out
	}
	for _, v := range l {
		nets := map[string]bool{v.Network: true}
		if full, err := s.VMs.Get(ctx, v.Name); err == nil {
			for _, n := range full.NICs {
				nets[n.Network] = true
			}
		}
		for n := range nets {
			if vm.IsVNet(n) {
				name := strings.TrimPrefix(n, vm.NetVNetPrefix)
				out[name] = append(out[name], v.Name)
			}
		}
	}
	return out
}

func (s *Server) putSDN(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if !p.Admin || (p.Role != "admin" && !p.Cluster) {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin can change virtual networks")
		return
	}
	if s.VNets == nil {
		writeErr(w, http.StatusNotFound, "not_found", "virtual networks aren't available here")
		return
	}
	var c sdn.Config
	if !decode(w, r, &c) {
		return
	}
	if !p.Cluster {
		if s.Cluster != nil && s.Cluster.Role() == "worker" {
			writeErr(w, http.StatusConflict, "conflict", "change virtual networks on the cluster master")
			return
		}
		v, err := sdn.Validate(c)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), sdn.ErrInvalid.Error()+": "))
			return
		}
		old := s.VNets.Store.Load()
		users := s.vnetUsers(r.Context())
		for _, o := range old.VNets {
			n, ok := v.Find(o.Name)
			if l := users[o.Name]; len(l) > 0 && (!ok || n.VNI != o.VNI) {
				writeErr(w, http.StatusConflict, "conflict", "virtual network "+o.Name+" is used by "+strings.Join(l, ", ")+": move those VMs to another network first")
				return
			}
		}
	}
	saved, err := s.VNets.Store.Save(c)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), sdn.ErrInvalid.Error()+": "))
		return
	}
	note := ""
	if err := s.VNets.Apply(r.Context()); err != nil {
		note = "saved, but setting it up here failed: " + err.Error()
	}
	if s.Cluster != nil && !p.Cluster && s.Cluster.Role() == "master" {
		self := s.Cluster.SelfID()
		var failed []string
		for _, n := range s.Cluster.Workers() {
			if n.ID == self {
				continue
			}
			if err := s.Cluster.Call(r.Context(), n, "PUT", "/sdn", saved, nil); err != nil {
				failed = append(failed, n.Name)
			}
		}
		if len(failed) > 0 {
			note = strings.TrimSpace(note + " These servers couldn't be reached now and pick it up within a minute of coming back: " + strings.Join(failed, ", "))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"vnets": saved.VNets, "note": note})
}
