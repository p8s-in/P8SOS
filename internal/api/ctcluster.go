package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/cluster"
	"p8sos/internal/ct"
	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// CTService is what the API needs from containers: the local ct.Service, or
// cluster.CTs on a cluster's master (containers on every server).
type CTService interface {
	Exists(name string) bool
	Templates(ctx context.Context) []ct.Template
	Create(ctx context.Context, r ct.CreateRequest) (ct.Container, error)
	Get(ctx context.Context, name string) (ct.Container, error)
	List(ctx context.Context) ([]ct.Container, error)
	Start(ctx context.Context, name string) (ct.Container, error)
	Stop(ctx context.Context, name string, force bool) (ct.Container, error)
	Reboot(ctx context.Context, name string) (ct.Container, error)
	Delete(ctx context.Context, name string) error
	Resize(ctx context.Context, name string, r ct.ResizeRequest) (ct.Container, error)
	SetOwner(ctx context.Context, name, owner string) (ct.Container, error)
	SetPassword(ctx context.Context, name, user, password string) error
	SetupSSH(ctx context.Context, name string) (string, error)
	Snapshots(ctx context.Context, name string) ([]ct.Snapshot, error)
	CreateSnapshot(ctx context.Context, name, snap string) (ct.Snapshot, error)
	Rollback(ctx context.Context, name, snap string, destroyNewer bool) (ct.Container, error)
	DeleteSnapshot(ctx context.Context, name, snap string) error
	ListBackups(name string) ([]backup.Meta, error)
	DeleteBackup(name, id string) error
	StartBackup(ctx context.Context, name string, keep int) (jobs.Job, error)
	StartRestore(ctx context.Context, name, id, newName string, exists func(string) bool) (jobs.Job, error)
}

// ctMover moves containers between cluster servers (cluster.CTs).
type ctMover interface {
	Move(ctx context.Context, name, to string, start bool) (jobs.Job, error)
	Where(ctx context.Context, name string) (cluster.Node, bool)
}

// moveCT: POST /containers/{name}/move {"to":"p8s-vm1","start":true} (staff).
func (s *Server) moveCT(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	if !who(r).Admin {
		writeErr(w, http.StatusForbidden, "forbidden", "only staff can move containers between servers")
		return
	}
	mv, ok := s.CTs.(ctMover)
	if !ok {
		writeErr(w, http.StatusConflict, "conflict", "moving containers needs a cluster (this server is on its own or a worker)")
		return
	}
	var body struct {
		To    string `json:"to"`
		Start bool   `json:"start"`
	}
	if !decode(w, r, &body) {
		return
	}
	j, err := mv.Move(r.Context(), c.Name, body.To, body.Start)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.noteJob(r, j.ID)
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

// --- server-to-server (the master calls these on the container's server) ---

func (s *Server) ctClusterOnly(w http.ResponseWriter, r *http.Request) bool {
	if !who(r).Cluster {
		writeErr(w, http.StatusForbidden, "forbidden", "only the cluster master can do that")
		return false
	}
	if s.CTLocal == nil {
		writeErr(w, http.StatusNotFound, "not_found", "containers are not available on this server")
		return false
	}
	return true
}

// ctExport: GET /cluster/ct-export/{name} streams a stopped container.
func (s *Server) ctExport(w http.ResponseWriter, r *http.Request) {
	if !s.ctClusterOnly(w, r) {
		return
	}
	name, phase := r.PathValue("name"), r.URL.Query().Get("phase")
	check := s.CTLocal.MoveCheck
	if phase == ct.PhasePre {
		check = s.CTLocal.LiveMoveCheck
	}
	if why, err := check(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	} else if why != "" {
		s.fail(w, r, fmt.Errorf("%w: %s can't move: %s", vm.ErrConflict, name, why))
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(24 * time.Hour))
	w.Header().Set("Content-Type", "application/octet-stream")
	if err := s.CTLocal.ExportPhase(r.Context(), name, phase, w); err != nil {
		if s.Log != nil {
			s.Log.Error("container export", "ct", name, "err", err)
		}
		panic(http.ErrAbortHandler) // the receiver sees a cut stream
	}
}

// ctImportDrop: DELETE /cluster/ct-import/{name} removes the disk of a move
// of a running container that stopped halfway.
func (s *Server) ctImportDrop(w http.ResponseWriter, r *http.Request) {
	if !s.ctClusterOnly(w, r) {
		return
	}
	if err := s.CTLocal.DropMove(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ctImport: POST /cluster/ct-import takes a stream from ctExport.
func (s *Server) ctImport(w http.ResponseWriter, r *http.Request) {
	if !s.ctClusterOnly(w, r) {
		return
	}
	c, note, err := s.CTLocal.Import(r.Context(), r.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"container": c, "note": note})
}

// ctClusterShell: GET /cluster/ct-shell/{name} — a shell in a container
// here, for the master's dashboard terminal (upgraded connection, shell frames).
func (s *Server) ctClusterShell(w http.ResponseWriter, r *http.Request) {
	if !s.ctClusterOnly(w, r) {
		return
	}
	t, err := s.CTLocal.Shell(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Close()
		writeErr(w, http.StatusInternalServerError, "internal", "can't take over the connection: "+err.Error())
		return
	}
	_ = conn.SetDeadline(time.Time{})
	_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: p8s-shell\r\n\r\n")
	_ = buf.Flush()
	var once sync.Once
	stop := func() { once.Do(func() { conn.Close(); t.Close() }) }
	go func() { _, _ = io.Copy(conn, t.PTY); stop() }()
	for {
		kind, p, err := cluster.ReadShellFrame(buf)
		if err != nil {
			break
		}
		if kind == cluster.ShellResize {
			var c, rws int
			if _, err := fmt.Sscan(string(p), &c, &rws); err == nil {
				_ = t.Resize(c, rws)
			}
			continue
		}
		if _, err := t.PTY.Write(p); err != nil {
			break
		}
	}
	stop()
}

// LocalISOs is this server's own ISO library (for the master's copies).
type LocalISOs interface {
	ISOFile(name string) (string, int64, error)
	ReplaceISO(ctx context.Context, name string, r io.Reader) (vm.ISO, error)
	DeleteISO(ctx context.Context, name string) error
}

// clusterISO: GET / PUT / DELETE /cluster/iso/{name} — the master keeps
// copies of its ISOs here for VMs installed on this server.
func (s *Server) clusterISO(w http.ResponseWriter, r *http.Request) {
	if !who(r).Cluster {
		writeErr(w, http.StatusForbidden, "forbidden", "only the cluster master can do that")
		return
	}
	if s.LocalISOs == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no ISO library on this server")
		return
	}
	name := r.PathValue("name")
	switch r.Method {
	case http.MethodGet:
		_, n, err := s.LocalISOs.ISOFile(name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, cluster.ISOInfo{Name: name, Bytes: n})
	case http.MethodPut:
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(6 * time.Hour))
		_ = rc.SetWriteDeadline(time.Now().Add(6 * time.Hour))
		iso, err := s.LocalISOs.ReplaceISO(r.Context(), name, r.Body)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, iso)
	case http.MethodDelete:
		if err := s.LocalISOs.DeleteISO(r.Context(), name); err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
