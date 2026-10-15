package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/vm"
)

// Cluster: one master and worker servers ("extenders") joined over a
// WireGuard tunnel. See internal/cluster.

func (s *Server) clusterOn(w http.ResponseWriter) bool {
	if s.Cluster != nil {
		return true
	}
	writeErr(w, http.StatusNotFound, "not_found", "clustering is not available on this host")
	return false
}

func (s *Server) clusterFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, cluster.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, cluster.ErrRole):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, cluster.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeErr(w, http.StatusBadGateway, "cluster", err.Error())
	}
}

type nodeView struct {
	cluster.Node
	Role   string         `json:"role"`
	Self   bool           `json:"self"`
	Status cluster.Status `json:"status"`
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	st := s.Cluster.State()
	out := map[string]any{"role": st.Role, "endpoint": st.Endpoint, "term": st.Term,
		"size": s.Cluster.ClusterSize(), "peer": s.Cluster.Peer(), "quorum": s.Cluster.HasQuorum(),
		"voters": s.Cluster.Voters(), "witness": s.Cluster.WitnessStatus(r.Context())}
	nodes := []nodeView{}
	if st.Role != "" {
		self := nodeView{Node: st.Self, Role: st.Role, Self: true, Status: cluster.Status{Online: true}}
		if s.Cluster.LocalCapacity != nil {
			self.Status.Capacity = s.Cluster.LocalCapacity(r.Context())
		}
		nodes = append(nodes, self)
		for _, n := range st.Nodes {
			role := "worker"
			if st.Role == "worker" {
				role = "master"
			}
			nodes = append(nodes, nodeView{Node: n, Role: role, Status: s.Cluster.NodeStatus(n.ID)})
		}
		for _, n := range st.Mesh { // a worker's view of the other workers
			nodes = append(nodes, nodeView{Node: n, Role: "worker", Status: cluster.Status{Online: true}})
		}
	}
	out["nodes"] = nodes
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) clusterJoinCode(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	var body struct {
		MasterURL string `json:"master_url"`
		Endpoint  string `json:"endpoint"`
	}
	if !decode(w, r, &body) {
		return
	}
	code, cmd, err := s.Cluster.JoinCode(r.Context(), body.MasterURL, body.Endpoint)
	if err != nil {
		s.clusterFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"code": code, "command": cmd, "expires_in": 3600})
}

// clusterRegister is called by a joining server; the join code is its pass.
func (s *Server) clusterRegister(w http.ResponseWriter, r *http.Request) {
	if !s.clusterOn(w) {
		return
	}
	var req cluster.RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	req.FromIP = clientIP(r) // where its tunnel can be reached (same network)
	rep, err := s.Cluster.Register(r.Context(), req)
	if err != nil {
		s.audit(r, "cluster-join", http.StatusBadRequest, err.Error())
		s.clusterFail(w, r, err)
		return
	}
	s.audit(r, "cluster-join", http.StatusOK, "server "+req.Name+" joined as "+rep.ID)
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) clusterJoin(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	var body struct{ URL, Code string }
	if !decode(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	rep, err := s.Cluster.Join(ctx, body.URL, body.Code)
	if err != nil {
		s.clusterFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": rep.ID, "wg_ip": rep.WGIP, "master": rep.Name})
}

func (s *Server) clusterLeave(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	if err := s.Cluster.Leave(r.Context()); err != nil {
		s.clusterFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clusterRemove(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	id := r.PathValue("id")
	if r.URL.Query().Get("force") != "1" && s.ClusterVMs != nil {
		if l, err := s.VMs.List(r.Context()); err == nil {
			for _, v := range l {
				if v.NodeID == id {
					writeErr(w, http.StatusConflict, "conflict", "that server still has VMs ("+v.Name+" …); delete or move them first, or remove it anyway")
					return
				}
			}
		}
	}
	if err := s.Cluster.Remove(r.Context(), id); err != nil {
		s.clusterFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clusterRename: the master renames one of its workers (it asks the worker).
func (s *Server) clusterRename(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &b) {
		return
	}
	if err := s.Cluster.Rename(r.Context(), r.PathValue("id"), strings.ToLower(strings.TrimSpace(b.Name))); err != nil {
		s.clusterFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"note": "renaming; the new name shows in a few seconds"})
}

func (s *Server) clusterCapacity(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	if who(r).Cluster {
		s.Cluster.HeardFromMaster()
	}
	c := cluster.Capacity{}
	if s.Cluster.LocalCapacity != nil {
		c = s.Cluster.LocalCapacity(r.Context())
	}
	writeJSON(w, http.StatusOK, c)
}

// clusterCandidates: where would a VM of this size go (Create VM's server list).
func (s *Server) clusterCandidates(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || s.ClusterVMs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"servers": []any{}})
		return
	}
	q := r.URL.Query()
	mem, _ := strconv.Atoi(q.Get("memory_mib"))
	disk, _ := strconv.Atoi(q.Get("disk_gb"))
	req := vm.CreateRequest{MemoryMiB: mem, DiskGB: disk, Network: q.Get("network"), ISO: q.Get("iso"), IPPool: q.Get("ip_pool")}
	type row struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Local   bool   `json:"local"`
		Online  bool   `json:"online"`
		FreeMiB int    `json:"free_mib"`
		Reason  string `json:"reason,omitempty"`
	}
	out := []row{}
	for _, c := range s.ClusterVMs.Candidates(r.Context(), req) {
		out = append(out, row{c.Node.ID, c.Node.Name, c.Local, c.Online, c.Cap.MemTotalMiB - c.Cap.MemUsedMiB - 1536, c.Reason})
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

// clusterVNC hands the master a raw VNC stream of a local VM (HTTP Upgrade).
func (s *Server) clusterVNC(w http.ResponseWriter, r *http.Request) {
	if !who(r).Cluster {
		writeErr(w, http.StatusForbidden, "forbidden", "only the cluster master can do that")
		return
	}
	addr, err := s.VMs.ConsoleAddr(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tcp, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		tcp.Close()
		writeErr(w, http.StatusInternalServerError, "internal", "can't take over the connection: "+err.Error())
		return
	}
	_ = conn.SetDeadline(time.Time{}) // a console stays open longer than any request
	_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: p8s-vnc\r\n\r\n")
	_ = buf.Flush()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(tcp, buf); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, tcp); done <- struct{}{} }()
	<-done
	conn.Close()
	tcp.Close()
}

// --- finding servers on this network, invitations, internet checks ---

func (s *Server) clusterDiscover(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	l, err := s.Cluster.Discover(r.Context(), s.Version)
	if err != nil {
		s.clusterFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": l})
}

func (s *Server) clusterInviteSend(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	var f cluster.Found
	if !decode(w, r, &f) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	confirm, err := s.Cluster.SendInvite(ctx, f)
	if err != nil {
		s.clusterFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"confirm": confirm})
}

// clusterInvite receives an invitation (no login: it only waits for this
// server's admin to accept it).
func (s *Server) clusterInvite(w http.ResponseWriter, r *http.Request) {
	if !s.clusterOn(w) {
		return
	}
	var body struct {
		MasterName string `json:"master_name"`
		MasterURL  string `json:"master_url"`
		Code       string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	inv, err := s.Cluster.ReceiveInvite(body.MasterName, body.MasterURL, body.Code, clientIP(r))
	if err != nil {
		s.clusterFail(w, r, err)
		return
	}
	s.audit(r, "cluster-invite", http.StatusAccepted, "from "+body.MasterName+" ("+clientIP(r)+"), number "+inv.Confirm)
	writeJSON(w, http.StatusAccepted, map[string]any{"waiting": true})
}

func (s *Server) clusterInvites(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": s.Cluster.Invites()})
}

func (s *Server) clusterInviteAnswer(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	id := r.PathValue("id")
	switch r.PathValue("answer") {
	case "accept":
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		rep, err := s.Cluster.AcceptInvite(ctx, id)
		if err != nil {
			s.clusterFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": rep.ID, "wg_ip": rep.WGIP, "master": rep.Name})
	case "decline":
		if err := s.Cluster.DeclineInvite(id); err != nil {
			s.clusterFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "accept or decline")
	}
}

func (s *Server) clusterPublicIP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	ip, err := cluster.PublicIP(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ip": "", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ip": ip})
}

// clusterCheck tests a public address from this server (best effort).
func (s *Server) clusterCheck(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Port == 0 {
		body.Port = 8006
	}
	out := map[string]any{"private": cluster.PrivateAddr(body.Host)}
	if out["private"] == true { // a LAN/CGNAT address: no need to try
		out["tcp"] = false
		writeJSON(w, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if err := cluster.CheckTCP(ctx, body.Host, body.Port); err != nil {
		out["tcp"], out["error"] = false, err.Error()
	} else {
		out["tcp"] = true
	}
	writeJSON(w, http.StatusOK, out)
}
