package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"p8sos/internal/cluster"
	"p8sos/internal/creds"
	"strings"
	"sync"
	"time"

	"p8sos/internal/console"
	"p8sos/internal/ct"
	"p8sos/internal/firewall"
	"p8sos/internal/vm"
)

// Containers (LXC), next to VMs: same owners, quotas and access rules.

func (s *Server) ctOn(w http.ResponseWriter) bool {
	if s.CTs != nil {
		return true
	}
	writeErr(w, http.StatusNotFound, "not_found", "containers are not available on this host")
	return false
}

// ctFor loads the container in the path if the caller may use it; someone
// else's container looks like one that doesn't exist.
func (s *Server) ctFor(w http.ResponseWriter, r *http.Request) (ct.Container, bool) {
	if !s.ctOn(w) {
		return ct.Container{}, false
	}
	name := r.PathValue("name")
	c, err := s.CTs.Get(r.Context(), name)
	if err == nil && !mayUse(who(r), c.Owner) {
		err = fmt.Errorf("%w: container %q", vm.ErrNotFound, name)
	}
	if err != nil {
		s.fail(w, r, err)
		return ct.Container{}, false
	}
	return c, true
}

func (s *Server) listCTs(w http.ResponseWriter, r *http.Request) {
	if s.CTs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"containers": []ct.Container{}, "enabled": false})
		return
	}
	all, err := s.CTs.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := who(r)
	out := []ct.Container{}
	for _, c := range all {
		if p.Admin || (c.Owner != "" && c.Owner == p.Name) {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": out, "enabled": true})
}

func (s *Server) ctTemplates(w http.ResponseWriter, r *http.Request) {
	if !s.ctOn(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": s.CTs.Templates(r.Context())})
}

func (s *Server) createCT(w http.ResponseWriter, r *http.Request) {
	if !s.ctOn(w) || s.suspended(w, r) {
		return
	}
	var req ct.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	p := who(r)
	if _, err := s.VMs.Get(r.Context(), req.Name); err == nil {
		writeErr(w, http.StatusConflict, "exists", fmt.Sprintf("a VM is already called %q", req.Name))
		return
	}
	// P8S Cloud plans size containers like VMs (same plans, same billing)
	vr := vm.CreateRequest{Plan: req.Plan, IPPool: req.IPPool, Network: req.Network, VCPUs: req.VCPUs, MemoryMiB: req.MemoryMiB, DiskGB: req.DiskGB}
	plan, ok := s.applyPlan(w, r, &vr)
	if !ok {
		return
	}
	req.IPPool, req.Network, req.VCPUs, req.MemoryMiB, req.DiskGB = vr.IPPool, vr.Network, vr.VCPUs, vr.MemoryMiB, vr.DiskGB
	if !p.Admin {
		req.Owner = p.Name
		if req.Network == vm.NetLAN && !p.Quota.LAN {
			writeErr(w, http.StatusForbidden, "forbidden", "your plan doesn't include the LAN network; use nat")
			return
		}
		d := Usage{VMs: 1, VCPUs: max(req.VCPUs, 1), MemoryMiB: max(req.MemoryMiB, 512), DiskGB: max(req.DiskGB, 8)}
		if req.IPPool != "" {
			if !s.customerPool(req.IPPool) {
				writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("no ip pool %q for customers", req.IPPool))
				return
			}
			d.PublicIPs = 1
		}
		defer s.quotaLock(p.Name)()
		if err := s.checkQuota(r.Context(), p, d); err != nil {
			s.fail(w, r, err)
			return
		}
		if !s.canAfford(w, r, p.Name, plan) {
			return
		}
	} else if req.Owner != "" && !p.Cluster { // the master checked it already
		if s.Users == nil {
			writeErr(w, http.StatusBadRequest, "invalid", "user accounts are not enabled on this host")
			return
		}
		if _, err := s.Users.Get(req.Owner); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if req.Owner != "" {
		req.NoDefaultKeys = true
		if len(req.SSHKeys) == 0 && s.Users != nil {
			if u, err := s.Users.Get(req.Owner); err == nil {
				req.SSHKeys = u.SSHKeys
			}
		}
	}
	generated := s.autoPassword(r, &req.RootPassword)
	c, err := s.CTs.Create(r.Context(), req)
	if err != nil && c.Name == "" {
		s.fail(w, r, err)
		return
	}
	var login *loginView
	if req.RootPassword != "" {
		l := creds.Login{User: "root", Password: req.RootPassword, SSH: true}
		if err := s.Creds.Set("ct", c.Name, l); err != nil && s.Log != nil {
			s.Log.Error("store container login", "ct", c.Name, "err", err)
		}
		if generated { // a password the caller typed is never sent back
			lv := viewOf(l, true)
			login = &lv
		}
	}
	if c.PublicIP != "" {
		s.reapplyFirewall(r.Context())
	}
	ip := c.PublicIP
	if ip == "" {
		ip = c.IP
	}
	s.afterCreate(r, vm.VM{Name: c.Name, Owner: c.Owner, IP: ip, User: "root"}, plan)
	w.Header().Set("Location", "/api/v1/containers/"+c.Name)
	if err != nil { // made, but didn't start
		writeJSON(w, http.StatusCreated, map[string]any{"container": c, "warning": err.Error(), "login": login})
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		ct.Container
		Login *loginView `json:"login,omitempty"`
	}{c, login})
}

func (s *Server) getCT(w http.ResponseWriter, r *http.Request) {
	if c, ok := s.ctFor(w, r); ok {
		writeJSON(w, http.StatusOK, c)
	}
}

func (s *Server) ctPower(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.ctFor(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	var (
		c   ct.Container
		err error
	)
	switch r.PathValue("action") {
	case "start":
		if s.suspended(w, r) {
			return
		}
		c, err = s.CTs.Start(r.Context(), name)
	case "stop":
		var body struct {
			Force bool `json:"force"`
		}
		if r.ContentLength > 0 && !decode(w, r, &body) {
			return
		}
		c, err = s.CTs.Stop(r.Context(), name, body.Force)
	case "reboot":
		c, err = s.CTs.Reboot(r.Context(), name)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "unknown action")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCT(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	if c.PublicIP != "" {
		defer s.reapplyFirewall(r.Context()) // drop its address pinning
	}
	if err := s.CTs.Delete(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	_ = s.Creds.Delete("ct", r.PathValue("name"))
	if g, ok := s.VMs.(guestFirewalls); ok {
		_ = g.DropGuestFirewall(r.Context(), "ct:"+r.PathValue("name"))
	}
	_ = s.VMs.SetSchedule("ct:"+r.PathValue("name"), vm.Schedule{}) // backups stay, the daily run goes
	if s.Billing != nil {                                           // stop charging
		if err := s.Billing.Unsubscribe(r.PathValue("name")); err != nil && s.Log != nil {
			s.Log.Error("stop billing of deleted container", "container", r.PathValue("name"), "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// guestFirewalls is the shared VM/container firewall (vm.Service).
type guestFirewalls interface {
	GuestFirewall(key string) (firewall.Settings, error)
	SetGuestFirewall(ctx context.Context, key string, fw firewall.Settings) (firewall.Settings, error)
	DropGuestFirewall(ctx context.Context, key string) error
}

func (s *Server) ctFirewall(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	g, ok := s.VMs.(guestFirewalls)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "the firewall is not available on this host")
		return
	}
	var fw firewall.Settings
	var err error
	if r.Method == http.MethodGet {
		fw, err = g.GuestFirewall("ct:" + c.Name)
	} else {
		var body firewall.Settings
		if !decode(w, r, &body) {
			return
		}
		fw, err = g.SetGuestFirewall(r.Context(), "ct:"+c.Name, body)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fw)
}

func (s *Server) resizeCT(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	if s.Billing != nil && !who(r).Admin {
		if _, billed := s.Billing.Sub(c.Name); billed {
			writeErr(w, http.StatusBadRequest, "invalid", "this container is on a plan; to change its size, ask support (plan changes are coming)")
			return
		}
	}
	var req ct.ResizeRequest
	if !decode(w, r, &req) {
		return
	}
	p := who(r)
	if !p.Admin {
		d := Usage{}
		if req.VCPUs > c.VCPUs {
			d.VCPUs = req.VCPUs - c.VCPUs
		}
		if req.MemoryMiB > c.MemoryMiB {
			d.MemoryMiB = req.MemoryMiB - c.MemoryMiB
		}
		if req.DiskGB > c.DiskGB {
			d.DiskGB = req.DiskGB - c.DiskGB
		}
		defer s.quotaLock(p.Name)()
		if err := s.checkQuota(r.Context(), p, d); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	c, err := s.CTs.Resize(r.Context(), c.Name, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) ctSnapshots(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	l, err := s.CTs.Snapshots(r.Context(), c.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": l})
}

func (s *Server) ctSnapshot(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if r.ContentLength > 0 && !decode(w, r, &body) {
		return
	}
	sn, err := s.CTs.CreateSnapshot(r.Context(), c.Name, body.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sn)
}

func (s *Server) ctRollback(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	var body struct {
		DestroyNewer bool `json:"destroy_newer"`
	}
	if r.ContentLength > 0 && !decode(w, r, &body) {
		return
	}
	c, err := s.CTs.Rollback(r.Context(), c.Name, r.PathValue("snap"), body.DestroyNewer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) ctDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	if err := s.CTs.DeleteSnapshot(r.Context(), c.Name, r.PathValue("snap")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ctPassword(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Generate bool   `json:"generate"` // make a random one and return it
	}
	if !decode(w, r, &body) {
		return
	}
	if body.User == "" {
		body.User = "root"
	}
	if body.Generate {
		body.Password = creds.Generate(PasswordLength)
	}
	if err := s.CTs.SetPassword(r.Context(), c.Name, body.User, body.Password); err != nil {
		s.fail(w, r, err)
		return
	}
	s.rememberPassword("ct", c.Name, body.User, body.Password)
	if !body.Generate {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": body.User, "password": body.Password})
}

func (s *Server) ctOwner(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner string `json:"owner"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Owner != "" {
		if s.Users == nil {
			writeErr(w, http.StatusBadRequest, "invalid", "user accounts are not enabled on this host")
			return
		}
		if _, err := s.Users.Get(body.Owner); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	c, err := s.CTs.SetOwner(r.Context(), c.Name, body.Owner)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ctConsole hands out a one-time ticket for the container's terminal.
func (s *Server) ctConsole(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	if c.State != "running" {
		writeErr(w, http.StatusConflict, "conflict", "start the container first")
		return
	}
	t, ttl := s.tickets().Issue("ct:" + c.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": t, "path": "/api/v1/terminal?ticket=" + t, "expires_in": int(ttl.Seconds())})
}

// terminalSocket bridges the browser terminal (xterm.js) to a shell in the
// container: binary frames are keystrokes / output, text frames are
// {"cols":..,"rows":..} size changes.
func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			writeErr(w, http.StatusForbidden, "forbidden", "the terminal must be opened from this host's dashboard")
			return
		}
	}
	id, ok := s.tickets().Redeem(r.URL.Query().Get("ticket"))
	name, isCT := strings.CutPrefix(id, "ct:")
	hostUser, isHost := strings.CutPrefix(id, "host:")
	hostUser, nodeID, remote := strings.Cut(hostUser, "@")
	if !ok || !(isCT && s.CTs != nil || isHost && (s.HostShell != nil || remote && s.Cluster != nil)) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "terminal ticket is invalid, used or expired")
		return
	}
	var term shellIO
	switch {
	case isHost && remote: // a worker's shell, through the cluster tunnel
		var n cluster.Node
		for _, x := range s.Cluster.Workers() {
			if x.ID == nodeID {
				n = x
			}
		}
		if n.ID == "" {
			writeErr(w, http.StatusNotFound, "not_found", "server "+nodeID+" is not in the cluster")
			return
		}
		conn, err := s.Cluster.DialShell(r.Context(), n, hostUser)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		term, name = remoteShell{conn}, n.Name+" (root shell, "+hostUser+")"
	case isHost:
		t, err := s.HostShell()
		if err != nil {
			s.fail(w, r, err)
			return
		}
		term, name = localShell{t}, "server (root shell, "+hostUser+")"
	default:
		if mv, ok := s.CTs.(ctMover); ok { // a container on another server: through the tunnel
			if n, remote := mv.Where(r.Context(), name); remote {
				dial, ok := s.CTs.(interface {
					DialShell(ctx context.Context, n cluster.Node, name string) (net.Conn, error)
				})
				if !ok {
					writeErr(w, http.StatusNotFound, "not_found", "this container's terminal isn't reachable from here")
					return
				}
				conn, err := dial.DialShell(r.Context(), n, name)
				if err != nil {
					s.fail(w, r, err)
					return
				}
				term = remoteShell{conn}
				break
			}
		}
		if s.CTLocal == nil {
			writeErr(w, http.StatusNotFound, "not_found", "containers are not available on this host")
			return
		}
		t, err := s.CTLocal.Shell(r.Context(), name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		term = localShell{t}
	}
	if isHost {
		opened, what := time.Now(), "server shell"
		if remote {
			what = "shell of " + name
		}
		s.audit(r, hostUser, http.StatusOK, what+" opened")
		defer func() {
			s.audit(r, hostUser, http.StatusOK, what+" closed after "+time.Since(opened).Round(time.Second).String())
		}()
	}
	ws, err := console.Upgrade(w, r)
	if err != nil {
		term.Close()
		return
	}
	if s.Log != nil {
		s.Log.Info("terminal opened", "target", name, "remote", r.RemoteAddr)
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = ws.SendClose(); _ = ws.Close(); term.Close() }) }
	go func() { // shell -> browser
		defer stop()
		buf := make([]byte, 32*1024)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				if ws.WriteBinary(buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for { // browser -> shell
		text, p, err := ws.ReadTyped()
		if err != nil {
			break
		}
		if text {
			var sz struct{ Cols, Rows int }
			if json.Unmarshal(p, &sz) == nil {
				_ = term.Resize(sz.Cols, sz.Rows)
			}
			continue
		}
		if err := term.Keys(p); err != nil {
			break
		}
	}
	stop()
}

// shellIO is a terminal the browser drives: here (PTY) or on a cluster worker.
type shellIO interface {
	Read(p []byte) (int, error)
	Keys(p []byte) error
	Resize(cols, rows int) error
	Close()
}

type localShell struct{ t *ct.Terminal }

func (l localShell) Read(p []byte) (int, error)  { return l.t.PTY.Read(p) }
func (l localShell) Keys(p []byte) error         { _, err := l.t.PTY.Write(p); return err }
func (l localShell) Resize(cols, rows int) error { return l.t.Resize(cols, rows) }
func (l localShell) Close()                      { l.t.Close() }

type remoteShell struct{ c net.Conn }

func (r remoteShell) Read(p []byte) (int, error) { return r.c.Read(p) }
func (r remoteShell) Keys(p []byte) error        { return cluster.WriteShellFrame(r.c, cluster.ShellData, p) }
func (r remoteShell) Resize(cols, rows int) error {
	return cluster.WriteShellFrame(r.c, cluster.ShellResize, []byte(fmt.Sprintf("%d %d", cols, rows)))
}
func (r remoteShell) Close() { _ = r.c.Close() }

// clusterShell gives the master a root shell of this worker (Server → Shell
// of this server in the master's dashboard). The user is the master's admin.
func (s *Server) clusterShell(w http.ResponseWriter, r *http.Request) {
	if !who(r).Cluster {
		writeErr(w, http.StatusForbidden, "forbidden", "only the cluster master can do that")
		return
	}
	if s.HostShell == nil {
		writeErr(w, http.StatusNotFound, "not_found", "the server shell is turned off on this server (host_shell in /etc/p8s/p8sd.json)")
		return
	}
	user := r.URL.Query().Get("user")
	if user == "" || len(user) > 64 {
		user = "master"
	}
	t, err := s.HostShell()
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
	opened := time.Now()
	s.audit(r, "master:"+user, http.StatusOK, "server shell opened from the cluster master")
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
	s.audit(r, "master:"+user, http.StatusOK, "server shell closed after "+time.Since(opened).Round(time.Second).String())
}

// ctSetupSSH installs (or repairs) the SSH server inside a container.
func (s *Server) ctSetupSSH(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	out, err := s.CTs.SetupSSH(r.Context(), c.Name)
	if len(out) > 8000 {
		out = out[len(out)-8000:]
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{"code": "setup_failed", "message": err.Error()}, "output": out})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"output": out})
}

// --- container backups (stored next to VM backups; restored as a container) ---

func (s *Server) ctBackups(w http.ResponseWriter, r *http.Request) {
	if !s.ctOn(w) {
		return
	}
	name := r.PathValue("name")
	if name != "" {
		if _, ok := s.ctFor(w, r); !ok {
			return
		}
	}
	b, err := s.CTs.ListBackups(name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, out := who(r), []vm.Backup{}
	for _, m := range b {
		if mayUse(p, m.Owner) {
			out = append(out, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": out})
}

func (s *Server) ctBackup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	j, err := s.CTs.StartBackup(r.Context(), c.Name, 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.noteJob(r, j.ID)
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

// ctBackupFor finds a container backup the caller may use.
func (s *Server) ctBackupFor(w http.ResponseWriter, r *http.Request) (vm.Backup, bool) {
	if !s.ctOn(w) {
		return vm.Backup{}, false
	}
	b, err := s.CTs.ListBackups(r.PathValue("name"))
	if err == nil {
		for _, m := range b {
			if m.ID == r.PathValue("id") && mayUse(who(r), m.Owner) {
				return m, true
			}
		}
		err = fmt.Errorf("%w: backup %s of %s", vm.ErrNotFound, r.PathValue("id"), r.PathValue("name"))
	}
	s.fail(w, r, err)
	return vm.Backup{}, false
}

func (s *Server) ctDeleteBackup(w http.ResponseWriter, r *http.Request) {
	m, ok := s.ctBackupFor(w, r)
	if !ok {
		return
	}
	if err := s.CTs.DeleteBackup(m.VM, m.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ctRestore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"` // new container name; empty = the original (once deleted)
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	m, ok := s.ctBackupFor(w, r)
	if !ok || s.suspended(w, r) {
		return
	}
	p := who(r)
	if !p.Admin {
		defer s.quotaLock(p.Name)()
		if err := s.checkQuota(r.Context(), p, Usage{VMs: 1, VCPUs: m.VCPUs, MemoryMiB: m.MemoryMiB, DiskGB: m.DiskGB}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	vmExists := func(n string) bool { _, err := s.VMs.Get(r.Context(), n); return err == nil }
	j, err := s.CTs.StartRestore(r.Context(), m.VM, m.ID, body.Name, vmExists)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.noteJob(r, j.ID)
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) ctSchedule(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ctFor(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		s.showSchedule(w, r, "ct:"+c.Name)
	} else {
		s.saveSchedule(w, r, "ct:"+c.Name)
	}
}

// reapplyFirewall rebuilds the firewall table (public address pinning
// changed); best effort, the 5-minute refresh catches up otherwise.
func (s *Server) reapplyFirewall(ctx context.Context) {
	if f, ok := s.VMs.(interface{ ApplyFirewalls(context.Context) error }); ok {
		if err := f.ApplyFirewalls(context.WithoutCancel(ctx)); err != nil && s.Log != nil {
			s.Log.Warn("firewall", "err", err)
		}
	}
}

// hostShellTicket lets an admin open the server's root shell (Server → Shell).
func (s *Server) hostShellTicket(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	p := who(r)
	if p.Cluster {
		writeErr(w, http.StatusForbidden, "forbidden", "not for the cluster master's token")
		return
	}
	id := "host:" + p.Name
	if node := r.URL.Query().Get("node"); node != "" && s.Cluster != nil && node != s.Cluster.SelfID() {
		found := false
		for _, n := range s.Cluster.Workers() {
			found = found || n.ID == node
		}
		if !found {
			writeErr(w, http.StatusNotFound, "not_found", "no server "+node+" in this cluster")
			return
		}
		id += "@" + node // a worker's shell, through the tunnel
	} else if s.HostShell == nil {
		writeErr(w, http.StatusNotFound, "not_found", "the server shell is turned off on this host (host_shell in /etc/p8s/p8sd.json)")
		return
	}
	t, ttl := s.tickets().Issue(id)
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": t, "path": "/api/v1/terminal?ticket=" + t, "expires_in": int(ttl.Seconds())})
}
