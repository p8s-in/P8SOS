package api

import (
	"context"
	"net/http"
	"regexp"

	"p8sos/internal/vm"
)

// Network cards of a VM after its first one (staff only):
// POST /vms/{name}/nics {"network":"nat"|"lan","vlan":N} → plugged in at once
// when the VM runs; DELETE /vms/{name}/nics/{mac}.

// NICVMs is the network-card part of the VM service (local or cluster).
type NICVMs interface {
	AddNIC(ctx context.Context, name, network string, vlan int) (vm.VM, string, error)
	RemoveNIC(ctx context.Context, name, mac string) (vm.VM, error)
}

var reMACPath = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`)

func (s *Server) nicsAPI(w http.ResponseWriter, r *http.Request) (NICVMs, vm.VM, bool) {
	if !who(r).Admin {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin or operator can add or remove network cards")
		return nil, vm.VM{}, false
	}
	cur, ok := s.vmFor(w, r)
	if !ok {
		return nil, vm.VM{}, false
	}
	n, ok := s.VMs.(NICVMs)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "extra network cards aren't available here")
	}
	return n, cur, ok
}

func (s *Server) addNIC(w http.ResponseWriter, r *http.Request) {
	n, cur, ok := s.nicsAPI(w, r)
	if !ok {
		return
	}
	var req struct {
		Network string `json:"network"`
		VLAN    int    `json:"vlan"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, note, err := n.AddNIC(r.Context(), cur.Name, req.Network, req.VLAN)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, VMWithNote{v, note})
}

func (s *Server) removeNIC(w http.ResponseWriter, r *http.Request) {
	n, cur, ok := s.nicsAPI(w, r)
	if !ok {
		return
	}
	mac := r.PathValue("mac")
	if !reMACPath.MatchString(mac) {
		writeErr(w, http.StatusNotFound, "not_found", "network card: give its MAC address")
		return
	}
	v, err := n.RemoveNIC(r.Context(), cur.Name, mac)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
