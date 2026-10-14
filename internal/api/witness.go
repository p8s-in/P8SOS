package api

import (
	"net/http"
	"os"
	"path/filepath"

	"p8sos/internal/witness"
)

// The cluster's witness (two-server tie-breaker), set on the master.

func (s *Server) getWitness(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"witness": s.Cluster.WitnessStatus(r.Context()), "voters": s.Cluster.Voters()})
}

func (s *Server) setWitness(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	var c witness.Conf
	if !decode(w, r, &c) {
		return
	}
	c.Cluster = "" // the master picks it
	if _, err := s.Cluster.SetWitness(r.Context(), c); err != nil {
		s.clusterFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"witness": s.Cluster.WitnessStatus(r.Context()), "voters": s.Cluster.Voters(),
		"note": "The witness is the third vote: if one server dies, the other takes over by itself (after about a minute)."})
}

func (s *Server) deleteWitness(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.clusterOn(w) {
		return
	}
	if err := s.Cluster.ClearWitness(); err != nil {
		s.clusterFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// WitnessBinDir holds the built witness programs (install.sh puts them there).
var WitnessBinDir = "/usr/local/share/p8s/witness"

func (s *Server) downloadWitness(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	arch := r.PathValue("arch")
	if arch != "amd64" && arch != "arm64" {
		writeErr(w, http.StatusNotFound, "not_found", "amd64 or arm64")
		return
	}
	name := "p8s-witness-" + arch
	f, err := os.Open(filepath.Join(WitnessBinDir, name))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "the witness program isn't built here yet: run sudo bash update.sh")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, st.ModTime(), f)
}
