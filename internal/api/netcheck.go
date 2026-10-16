package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/tasks"
)

// The network check of every server, for the Datacenter page (admins):
//
//	GET  /node/netcheck                   this server: host internet, guests' NAT/DNS/firewall, reachable from outside
//	GET  /cluster/netcheck                every server of the cluster
//	POST /cluster/netcheck/{id}/repair    run the repair on that server (a server task)

func (s *Server) nodeNetCheck(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	rep, err := s.Node.Tasks.NetCheck(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

type serverNet struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Self   bool             `json:"self,omitempty"`
	Report *tasks.NetReport `json:"report,omitempty"`
	Error  string           `json:"error,omitempty"`
}

func (s *Server) clusterNetCheck(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	self := serverNet{ID: "", Name: "this server", Self: true}
	if s.Cluster != nil {
		self.ID, self.Name = s.Cluster.SelfID(), s.Cluster.SelfName()
	}
	out := []serverNet{self}
	var workers []serverNet
	if s.Cluster != nil {
		for _, n := range s.Cluster.Workers() {
			workers = append(workers, serverNet{ID: n.ID, Name: n.Name})
		}
	}
	out = append(out, workers...)
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(x *serverNet) {
			defer wg.Done()
			var rep tasks.NetReport
			var err error
			if x.Self {
				rep, err = s.Node.Tasks.NetCheck(ctx)
			} else if s.Cluster.NodeStatus(x.ID).Online {
				n, _ := s.clusterNode(x.ID)
				err = s.Cluster.Call(ctx, n, "GET", "/node/netcheck", nil, &rep)
			} else {
				x.Error = "offline"
				return
			}
			if err != nil {
				x.Error = err.Error()
				return
			}
			x.Report = &rep
		}(&out[i])
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (s *Server) clusterNetRepair(w http.ResponseWriter, r *http.Request) {
	if !s.nodeOn(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.Cluster == nil || id == "" || id == "self" || id == s.Cluster.SelfID() {
		s.taskStarted(w, r, tasks.NetRepair, "")
		return
	}
	n, ok := s.clusterNode(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no server "+id+" in the cluster")
		return
	}
	var t tasks.Task
	if err := s.Cluster.Call(r.Context(), n, "POST", "/node/tasks", map[string]string{"kind": tasks.NetRepair}, &t); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, t)
}

// clusterNode is a worker of this master, with its token.
func (s *Server) clusterNode(id string) (cluster.Node, bool) {
	if s.Cluster == nil {
		return cluster.Node{}, false
	}
	for _, n := range s.Cluster.Workers() {
		if n.ID == id {
			return n, true
		}
	}
	return cluster.Node{}, false
}
