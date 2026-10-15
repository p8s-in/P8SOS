package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"p8sos/internal/ippool"
	"p8sos/internal/vm"
)

// Public IP pools: blocks of datacenter addresses VMs get one address from.
// Admins manage them; customers see the pools open to them and how many
// addresses are free.

func (s *Server) poolErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ippool.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, ippool.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ippool.ErrExists), errors.Is(err, ippool.ErrInUse):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) poolsOn(w http.ResponseWriter) bool {
	if s.Pools != nil {
		return true
	}
	writeErr(w, http.StatusNotFound, "not_found", "public IP pools are not available on this host")
	return false
}

// customerPool reports whether customers may take addresses from this pool.
func (s *Server) customerPool(name string) bool {
	if s.Pools == nil {
		return false
	}
	p, err := s.Pools.Get(name)
	return err == nil && p.Customers
}

type poolSummary struct {
	Name string `json:"name"`
	Free int    `json:"free"`
	Size int    `json:"size"`
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	if s.Pools == nil {
		writeJSON(w, http.StatusOK, map[string]any{"pools": []any{}})
		return
	}
	l, err := s.Pools.List()
	if err != nil {
		s.poolErr(w, r, err)
		return
	}
	if who(r).Admin {
		writeJSON(w, http.StatusOK, map[string]any{"pools": l})
		return
	}
	out := []poolSummary{}
	for _, p := range l {
		if p.Customers {
			out = append(out, poolSummary{p.Name, p.Free, p.Size})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out})
}

func (s *Server) addPool(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.poolsOn(w) {
		return
	}
	var p ippool.Pool
	if !decode(w, r, &p) {
		return
	}
	p.Used, p.Extra, p.Used6 = nil, nil, nil
	v, err := s.Pools.Add(p)
	if err != nil {
		s.poolErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) updatePool(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.poolsOn(w) {
		return
	}
	var body struct {
		DNS       []string `json:"dns"`
		Exclude   []string `json:"exclude"`
		Customers bool     `json:"customers"`
		CIDR6     *string  `json:"cidr6"` // set (or "" to remove) the IPv6 network
		Gateway6  string   `json:"gateway6"`
	}
	if !decode(w, r, &body) {
		return
	}
	var v6 *ippool.V6
	if body.CIDR6 != nil {
		v6 = &ippool.V6{CIDR6: *body.CIDR6, Gateway6: body.Gateway6}
	}
	v, err := s.Pools.UpdateV6(r.PathValue("pool"), body.DNS, body.Exclude, body.Customers, v6)
	if err != nil {
		s.poolErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.poolsOn(w) {
		return
	}
	if err := s.Pools.Delete(r.PathValue("pool")); err != nil {
		s.poolErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PublicIPChanger changes a VM's or container's public addresses.
type PublicIPChanger interface {
	ChangePublicIPs(ctx context.Context, name string, r vm.PublicIPRequest) (vm.PublicIPResult, error)
}

// changePublicIPs: POST /vms/{name}/public-ips and /containers/{name}/public-ips
// {"action":"add|remove|ipv6|change","ip","pool"} (admins).
func (s *Server) changePublicIPs(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.poolsOn(w) {
		return
	}
	var req vm.PublicIPRequest
	if !decode(w, r, &req) {
		return
	}
	var c PublicIPChanger = s.LocalPublicIPs
	if strings.HasPrefix(r.URL.Path, "/api/v1/containers/") {
		if s.CTLocal == nil {
			writeErr(w, http.StatusNotFound, "not_found", "containers are not available on this host")
			return
		}
		c = s.CTLocal
	}
	if c == nil {
		writeErr(w, http.StatusNotFound, "not_found", "public IPs can't be changed on this host")
		return
	}
	res, err := c.ChangePublicIPs(r.Context(), r.PathValue("name"), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
