package api

import (
	"errors"
	"net/http"

	"p8sos/internal/cluster"
)

// No single master (cluster/quorum.go): the master hands out the cluster
// book; servers answer "do you see the master?" and vote; a worker can take
// over by hand. All need the cluster key (admin) except promote, which an
// admin runs on the worker's own dashboard.

func (s *Server) clusterBook(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	var b cluster.Book
	if !decodeBig(w, r, &b) {
		return
	}
	rep, err := s.Cluster.ReceiveBook(r.Context(), b)
	if errors.Is(err, cluster.ErrStale) {
		writeErr(w, http.StatusConflict, "stale", err.Error())
		return
	}
	if err != nil {
		s.clusterFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) clusterPeer(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.Cluster.Peer())
}

func (s *Server) clusterVote(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	var req cluster.VoteRequest
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, s.Cluster.Vote(req))
}

func (s *Server) clusterPromote(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	if err := s.Cluster.Promote(r.Context(), body.Force); err != nil {
		s.clusterFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"role": "master",
		"note": "This server is the cluster master now. Open its dashboard to manage the cluster; the old master becomes a worker when it comes back."})
}
