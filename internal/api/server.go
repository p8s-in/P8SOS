// Package api exposes the p8sd REST API (v1).
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"p8sos/internal/config"
	"p8sos/internal/console"
	"p8sos/internal/metrics"
	"p8sos/internal/vm"
)

// VMService is what the API needs from the vm layer (faked in tests).
type VMService interface {
	Create(ctx context.Context, r vm.CreateRequest) (vm.VM, error)
	Get(ctx context.Context, name string) (vm.VM, error)
	List(ctx context.Context) ([]vm.VM, error)
	Start(ctx context.Context, name string) (vm.VM, error)
	Stop(ctx context.Context, name string, force bool) (vm.VM, error)
	Reboot(ctx context.Context, name string) (vm.VM, error)
	Delete(ctx context.Context, name string) error
	Resize(ctx context.Context, name string, r vm.ResizeRequest) (vm.VM, error)
	Snapshots(ctx context.Context, name string) ([]vm.Snapshot, error)
	CreateSnapshot(ctx context.Context, name, snap string) (vm.Snapshot, error)
	Rollback(ctx context.Context, name, snap string, destroyNewer bool) (vm.VM, error)
	DeleteSnapshot(ctx context.Context, name, snap string) error
	ConsoleAddr(ctx context.Context, name string) (string, error)
	StartBackup(ctx context.Context, name string) (vm.Job, error)
	ListBackups(name string) ([]vm.Backup, error)
	DeleteBackup(name, id string) error
	StartRestore(ctx context.Context, name, id, newName string) (vm.Job, error)
	GetSchedule(name string) (vm.Schedule, error)
	SetSchedule(name string, s vm.Schedule) error
	Job(id string) (vm.Job, bool)
	ListJobs() []vm.Job
	ListISOs() ([]vm.ISO, error)
	SaveISO(name string, r io.Reader) (vm.ISO, error)
	StartISODownload(ctx context.Context, url, name, sha256 string) (vm.Job, error)
	DeleteISO(ctx context.Context, name string) error
	EjectISO(ctx context.Context, name string) (vm.VM, error)
	SetPassword(ctx context.Context, name, user, password string) error
}

type Server struct {
	VMs       VMService
	Templates []config.Template
	Tokens    []config.Token
	Version   string
	Log       *slog.Logger
	Host      func(ctx context.Context) any // host capacity for the dashboard; nil = 404
	UI        http.Handler                  // web dashboard served at /; nil = none
	Tickets   *console.Tickets              // console passes; nil = created on first use
	// Metrics serves the usage charts; nil = no charts.
	Metrics interface {
		VM(name string) []metrics.Point
		Host() []metrics.HostPoint
	}
	// BackupKeep/BackupTime are the host defaults shown with backup schedules.
	BackupKeep int
	BackupTime string
	// Dial connects to a VM's VNC port; nil = net.Dialer (tests replace it).
	Dial func(ctx context.Context, addr string) (net.Conn, error)

	once sync.Once
}

func (s *Server) tickets() *console.Tickets {
	s.once.Do(func() {
		if s.Tickets == nil {
			s.Tickets = &console.Tickets{}
		}
	})
	return s.Tickets
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/console", s.consoleSocket) // authorized by its one-time ticket

	auth := http.NewServeMux()
	auth.HandleFunc("GET /api/v1/templates", s.listTemplates)
	auth.HandleFunc("GET /api/v1/vms", s.listVMs)
	auth.HandleFunc("POST /api/v1/vms", s.createVM)
	auth.HandleFunc("GET /api/v1/vms/{name}", s.getVM)
	auth.HandleFunc("POST /api/v1/vms/{name}/start", s.startVM)
	auth.HandleFunc("POST /api/v1/vms/{name}/stop", s.stopVM)
	auth.HandleFunc("POST /api/v1/vms/{name}/reboot", s.rebootVM)
	auth.HandleFunc("DELETE /api/v1/vms/{name}", s.deleteVM)
	auth.HandleFunc("PATCH /api/v1/vms/{name}", s.resizeVM)
	auth.HandleFunc("GET /api/v1/vms/{name}/snapshots", s.listSnapshots)
	auth.HandleFunc("POST /api/v1/vms/{name}/snapshots", s.createSnapshot)
	auth.HandleFunc("POST /api/v1/vms/{name}/snapshots/{snap}/rollback", s.rollback)
	auth.HandleFunc("DELETE /api/v1/vms/{name}/snapshots/{snap}", s.deleteSnapshot)
	auth.HandleFunc("GET /api/v1/host", s.hostInfo)
	auth.HandleFunc("POST /api/v1/vms/{name}/console", s.consoleTicket)
	auth.HandleFunc("POST /api/v1/vms/{name}/password", s.setPassword)
	auth.HandleFunc("GET /api/v1/backups", s.listBackups)
	auth.HandleFunc("GET /api/v1/vms/{name}/backups", s.listBackups)
	auth.HandleFunc("POST /api/v1/vms/{name}/backups", s.startBackup)
	auth.HandleFunc("DELETE /api/v1/backups/{name}/{id}", s.deleteBackup)
	auth.HandleFunc("POST /api/v1/backups/{name}/{id}/restore", s.restoreBackup)
	auth.HandleFunc("GET /api/v1/vms/{name}/backup-schedule", s.getSchedule)
	auth.HandleFunc("PUT /api/v1/vms/{name}/backup-schedule", s.putSchedule)
	auth.HandleFunc("GET /api/v1/vms/{name}/metrics", s.vmMetrics)
	auth.HandleFunc("GET /api/v1/host/metrics", s.hostMetrics)
	auth.HandleFunc("GET /api/v1/isos", s.listISOs)
	auth.HandleFunc("PUT /api/v1/isos/{name}", s.uploadISO)
	auth.HandleFunc("POST /api/v1/isos/download", s.downloadISO)
	auth.HandleFunc("DELETE /api/v1/isos/{name}", s.deleteISO)
	auth.HandleFunc("POST /api/v1/vms/{name}/eject", s.ejectISO)
	auth.HandleFunc("GET /api/v1/jobs", s.listJobs)
	auth.HandleFunc("GET /api/v1/jobs/{id}", s.getJob)
	mux.Handle("/api/v1/", s.requireToken(auth))
	if s.UI != nil {
		mux.Handle("/", s.UI) // the page itself is public; every API call it makes needs the token
	}

	return s.logRequests(mux)
}

// --- handlers ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Version})
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	type tpl struct {
		Name        string `json:"name"`
		Label       string `json:"label"`
		MinDiskGB   int    `json:"min_disk_gb"`
		OSVariant   string `json:"os_variant"`
		DefaultUser string `json:"default_user"`
	}
	out := []tpl{}
	for _, t := range s.Templates {
		label := t.Label
		if label == "" {
			label = t.Name
		}
		out = append(out, tpl{t.Name, label, t.MinDiskGB, t.OSVariant, t.User()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

func (s *Server) hostInfo(w http.ResponseWriter, r *http.Request) {
	if s.Host == nil {
		writeErr(w, http.StatusNotFound, "not_found", "host info not available")
		return
	}
	writeJSON(w, http.StatusOK, s.Host(r.Context()))
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.VMs.List(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": vms})
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var req vm.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.VMs.Create(r.Context(), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/vms/"+v.Name)
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) {
	v, err := s.VMs.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) startVM(w http.ResponseWriter, r *http.Request) {
	v, err := s.VMs.Start(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) stopVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	v, err := s.VMs.Stop(r.Context(), r.PathValue("name"), body.Force)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) rebootVM(w http.ResponseWriter, r *http.Request) {
	v, err := s.VMs.Reboot(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	if err := s.VMs.Delete(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resizeVM(w http.ResponseWriter, r *http.Request) {
	var req vm.ResizeRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.VMs.Resize(r.Context(), r.PathValue("name"), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.VMs.Snapshots(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	sn, err := s.VMs.CreateSnapshot(r.Context(), r.PathValue("name"), body.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sn)
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DestroyNewer bool `json:"destroy_newer"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	v, err := s.VMs.Rollback(r.Context(), r.PathValue("name"), r.PathValue("snap"), body.DestroyNewer)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.VMs.DeleteSnapshot(r.Context(), r.PathValue("name"), r.PathValue("snap")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) consoleTicket(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.VMs.ConsoleAddr(r.Context(), name); err != nil {
		s.fail(w, err)
		return
	}
	t, ttl := s.tickets().Issue(name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"ticket": t, "path": "/api/v1/console?ticket=" + t, "expires_in": int(ttl.Seconds()),
	})
}

// consoleSocket bridges the browser (noVNC over WebSocket) to the VM's VNC port.
func (s *Server) consoleSocket(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			writeErr(w, http.StatusForbidden, "forbidden", "console must be opened from this host's dashboard")
			return
		}
	}
	name, ok := s.tickets().Redeem(r.URL.Query().Get("ticket"))
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "console ticket is invalid, used or expired")
		return
	}
	addr, err := s.VMs.ConsoleAddr(r.Context(), name)
	if err != nil {
		s.fail(w, err)
		return
	}
	dial := s.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second}
		dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	}
	tcp, err := dial(r.Context(), addr)
	if err != nil {
		s.fail(w, fmt.Errorf("connect to the VM's screen at %s: %w", addr, err))
		return
	}
	ws, err := console.Upgrade(w, r)
	if err != nil {
		tcp.Close()
		return
	}
	if s.Log != nil {
		s.Log.Info("console opened", "vm", name, "remote", r.RemoteAddr)
	}
	console.Proxy(ws, tcp)
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "" {
		if _, err := s.VMs.Get(r.Context(), name); err != nil && !errors.Is(err, vm.ErrNotFound) {
			s.fail(w, err)
			return
		}
	}
	b, err := s.VMs.ListBackups(name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": b})
}

func (s *Server) startBackup(w http.ResponseWriter, r *http.Request) {
	j, err := s.VMs.StartBackup(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.VMs.DeleteBackup(r.PathValue("name"), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"` // new VM name; empty = restore over the original VM
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	j, err := s.VMs.StartRestore(r.Context(), r.PathValue("name"), r.PathValue("id"), body.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

type scheduleView struct {
	Daily bool   `json:"daily"`
	Keep  int    `json:"keep"`
	At    string `json:"at"` // host time of the daily run (informational)
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.VMs.Get(r.Context(), name); err != nil {
		s.fail(w, err)
		return
	}
	sch, err := s.VMs.GetSchedule(name)
	if err != nil {
		s.fail(w, err)
		return
	}
	keep := sch.Keep
	if keep == 0 {
		keep = s.BackupKeep
	}
	writeJSON(w, http.StatusOK, scheduleView{Daily: sch.Daily, Keep: keep, At: s.BackupTime})
}

func (s *Server) putSchedule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Daily bool `json:"daily"`
		Keep  int  `json:"keep"`
	}
	if !decode(w, r, &body) {
		return
	}
	if _, err := s.VMs.Get(r.Context(), name); err != nil {
		s.fail(w, err)
		return
	}
	if body.Daily && body.Keep == 0 {
		body.Keep = s.BackupKeep
	}
	if err := s.VMs.SetSchedule(name, vm.Schedule{Daily: body.Daily, Keep: body.Keep}); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleView{Daily: body.Daily, Keep: body.Keep, At: s.BackupTime})
}

func (s *Server) vmMetrics(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.VMs.Get(r.Context(), name); err != nil {
		s.fail(w, err)
		return
	}
	pts := []metrics.Point{}
	if s.Metrics != nil {
		pts = s.Metrics.VM(name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": 10, "points": pts})
}

func (s *Server) hostMetrics(w http.ResponseWriter, r *http.Request) {
	pts := []metrics.HostPoint{}
	if s.Metrics != nil {
		pts = s.Metrics.Host()
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": 10, "points": pts})
}

func (s *Server) listISOs(w http.ResponseWriter, r *http.Request) {
	l, err := s.VMs.ListISOs()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"isos": l})
}

// uploadISO takes the raw image as the request body (PUT, any size up to the
// library's limit). Uploads can take long, so this request gets hours, not minutes.
func (s *Server) uploadISO(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(6 * time.Hour))
	_ = rc.SetWriteDeadline(time.Now().Add(6 * time.Hour))
	iso, err := s.VMs.SaveISO(r.PathValue("name"), r.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, iso)
}

func (s *Server) downloadISO(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL    string `json:"url"`
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	if !decode(w, r, &body) {
		return
	}
	j, err := s.VMs.StartISODownload(r.Context(), body.URL, body.Name, body.SHA256)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) deleteISO(w http.ResponseWriter, r *http.Request) {
	if err := s.VMs.DeleteISO(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ejectISO(w http.ResponseWriter, r *http.Request) {
	v, err := s.VMs.EjectISO(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.VMs.ListJobs()})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.VMs.Job(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) setPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.VMs.SetPassword(r.Context(), r.PathValue("name"), body.User, body.Password); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- middleware ---

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" || !s.validToken(tok) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="p8sd"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validToken(tok string) bool {
	sum := sha256.Sum256([]byte(tok))
	got := []byte(hex.EncodeToString(sum[:]))
	match := 0
	for _, t := range s.Tokens { // check all to keep timing constant
		match |= subtle.ConstantTimeCompare(got, []byte(t.SHA256))
	}
	return match == 1
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

// Unwrap lets http.ResponseController reach the real writer (WebSocket hijack).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		if s.Log != nil {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code,
				"dur_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
		}
	})
}

// --- helpers ---

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vm.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, vm.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, vm.ErrExists):
		writeErr(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, vm.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		if s.Log != nil {
			s.Log.Error("request failed", "err", err)
		}
		// Single-admin host: show the real cause (libvirt/zfs stderr) instead of a
		// bare 500, so problems can be fixed without digging through the journal.
		writeErr(w, http.StatusInternalServerError, "internal", "host error: "+err.Error())
	}
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
