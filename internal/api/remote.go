package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"p8sos/internal/execx"
	"p8sos/internal/vpn"
)

// Remote access (admins): how to reach this server from outside the home
// network — the built-in VPN, Tailscale, or the Cloudflare Tunnel.
type Remote struct {
	VPN        *vpn.Manager
	Run        execx.Runner // tailscale status; nil = not checked
	PublicIP   func(ctx context.Context) (string, error)
	FindRouter func(ctx context.Context) (vpn.Router, error)

	mu       sync.Mutex
	router   vpn.Router
	routerAt time.Time
	pub      string
	pubAt    time.Time
}

// TailscaleInfo is what `tailscale status` says.
type TailscaleInfo struct {
	Installed bool     `json:"installed"`
	State     string   `json:"state,omitempty"` // Running, NeedsLogin, Stopped
	IPs       []string `json:"ips,omitempty"`
	Name      string   `json:"name,omitempty"`
}

func (rm *Remote) tailscale(ctx context.Context) TailscaleInfo {
	var t TailscaleInfo
	if rm.Run == nil {
		return t
	}
	out, err := rm.Run.Run(ctx, "tailscale", "status", "--json")
	if len(out) == 0 && err != nil {
		return t
	}
	t.Installed = true
	var st struct {
		BackendState string
		Self         struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if json.Unmarshal(out, &st) == nil {
		t.State, t.IPs, t.Name = st.BackendState, st.Self.TailscaleIPs, st.Self.DNSName
	}
	return t
}

// lookups are slow (internet, router): cached for a few minutes, done in parallel.
func (rm *Remote) lookups(ctx context.Context, fresh bool) (string, vpn.Router) {
	rm.mu.Lock()
	needPub := fresh || time.Since(rm.pubAt) > 10*time.Minute
	needRouter := fresh || time.Since(rm.routerAt) > 5*time.Minute
	rm.mu.Unlock()
	var wg sync.WaitGroup
	if needPub && rm.PublicIP != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			if ip, err := rm.PublicIP(c); err == nil {
				rm.mu.Lock()
				rm.pub, rm.pubAt = ip, time.Now()
				rm.mu.Unlock()
			}
		}()
	}
	if needRouter && rm.FindRouter != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			r, _ := rm.FindRouter(c)
			rm.mu.Lock()
			rm.router, rm.routerAt = r, time.Now()
			rm.mu.Unlock()
		}()
	}
	wg.Wait()
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.pub, rm.router
}

func (s *Server) remoteOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminOnly(w, r) {
		return false
	}
	if s.Remote == nil || s.Remote.VPN == nil {
		writeErr(w, http.StatusNotFound, "not_found", "remote access isn't available on this server")
		return false
	}
	return true
}

func (s *Server) getRemote(w http.ResponseWriter, r *http.Request) {
	if !s.remoteOn(w, r) {
		return
	}
	st, err := s.Remote.VPN.Status(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pub, router := s.Remote.lookups(r.Context(), r.URL.Query().Get("refresh") != "")
	out := map[string]any{"vpn": st, "public_ip": pub, "router": router, "tailscale": s.Remote.tailscale(r.Context())}
	if s.Tunnel != nil {
		out["tunnel"] = s.Tunnel.Status()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putVPN(w http.ResponseWriter, r *http.Request) {
	if !s.remoteOn(w, r) {
		return
	}
	var body vpn.Settings
	if !decode(w, r, &body) {
		return
	}
	st, err := s.Remote.VPN.SetSettings(r.Context(), body)
	if errors.Is(err, vpn.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) addVPNDevice(w http.ResponseWriter, r *http.Request) {
	if !s.remoteOn(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	d, conf, err := s.Remote.VPN.AddDevice(r.Context(), body.Name, who(r).Name)
	if errors.Is(err, vpn.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	d.PSK = ""
	writeJSON(w, http.StatusCreated, map[string]any{"device": d, "config": conf})
}

func (s *Server) deleteVPNDevice(w http.ResponseWriter, r *http.Request) {
	if !s.remoteOn(w, r) {
		return
	}
	err := s.Remote.VPN.RemoveDevice(r.Context(), r.PathValue("id"))
	if errors.Is(err, vpn.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// forwardVPN asks the home router (UPnP) to send the VPN port to this server.
func (s *Server) forwardVPN(w http.ResponseWriter, r *http.Request) {
	if !s.remoteOn(w, r) {
		return
	}
	_, router := s.Remote.lookups(r.Context(), true)
	if !router.Found {
		writeErr(w, http.StatusConflict, "conflict", "no router answered (UPnP is off on it, or it doesn't have it): forward the port by hand on the router's page")
		return
	}
	st, _ := s.Remote.VPN.Status(r.Context())
	if err := router.ForwardUDP(r.Context(), st.Port, "P8S OS VPN"); err != nil {
		writeErr(w, http.StatusBadGateway, "router", "the router refused: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "the router now forwards UDP " + itoa(st.Port) + " to this server", "router": router.Name})
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
