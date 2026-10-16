package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/ct"
	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// CTs spans containers across the cluster like VMs does for VMs: each
// container lives on one server; the master forwards calls about it there.
// New containers go to the server with the most free memory that has the
// container's template (or the one asked for).
type CTs struct {
	Local *ct.Service
	M     *Manager
	V     *VMs // placement and jobs

	mu    sync.Mutex
	where map[string]string // container -> node id (workers only)
}

func cpath(name string, rest ...string) string {
	return "/containers/" + url.PathEscape(name) + strings.Join(rest, "")
}

func (c *CTs) workers() []Node {
	if c.M == nil || c.M.Role() != "master" {
		return nil
	}
	return c.M.Workers()
}

func (c *CTs) call(ctx context.Context, n Node, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	return errFrom(c.M.call(ctx, n, method, path, in, out))
}

func (c *CTs) remember(name, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.where == nil {
		c.where = map[string]string{}
	}
	if id == "" {
		delete(c.where, name)
	} else {
		c.where[name] = id
	}
}

// Where finds the worker that has a container (ok=false: here, or nowhere).
func (c *CTs) Where(ctx context.Context, name string) (Node, bool) {
	ws := c.workers()
	if len(ws) == 0 || c.Local.Exists(name) {
		return Node{}, false
	}
	c.mu.Lock()
	id := c.where[name]
	c.mu.Unlock()
	for _, n := range ws {
		if n.ID == id {
			return n, true
		}
	}
	for _, n := range ws {
		var x ct.Container
		if err := c.call(ctx, n, "GET", cpath(name), nil, &x); err == nil {
			c.remember(name, n.ID)
			return n, true
		}
	}
	return Node{}, false
}

func (c *CTs) tag(x ct.Container, n Node) ct.Container {
	x.Node, x.NodeID = n.Name, n.ID
	return x
}

func (c *CTs) selfTag(x ct.Container) ct.Container {
	if c.M != nil && c.M.Role() != "" {
		x.Node, x.NodeID = c.M.SelfName(), c.M.SelfID()
	}
	return x
}

// Exists says whether any server has a container of that name.
func (c *CTs) Exists(name string) bool {
	if c.Local.Exists(name) {
		return true
	}
	_, ok := c.Where(context.Background(), name)
	return ok
}

// Templates are this server's container templates.
func (c *CTs) Templates(ctx context.Context) []ct.Template { return c.Local.Templates(ctx) }

// List is every server's containers (each tagged with its server).
func (c *CTs) List(ctx context.Context) ([]ct.Container, error) {
	l, err := c.Local.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ct.Container, 0, len(l))
	for _, x := range l {
		out = append(out, c.selfTag(x))
	}
	for _, n := range c.workers() {
		var r struct {
			Containers []ct.Container `json:"containers"`
		}
		cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		err := c.M.call(cctx, n, "GET", "/containers", nil, &r)
		cancel()
		if err != nil {
			continue
		}
		for _, x := range r.Containers {
			c.remember(x.Name, n.ID)
			out = append(out, c.tag(x, n))
		}
	}
	return out, nil
}

func (c *CTs) Get(ctx context.Context, name string) (ct.Container, error) {
	if n, ok := c.Where(ctx, name); ok {
		var x ct.Container
		err := c.call(ctx, n, "GET", cpath(name), nil, &x)
		return c.tag(x, n), err
	}
	x, err := c.Local.Get(ctx, name)
	return c.selfTag(x), err
}

// hasTemplate says whether server n has a container template.
func (c *CTs) hasTemplate(ctx context.Context, n Node, tpl string) bool {
	var r struct {
		Templates []ct.Template `json:"templates"`
	}
	if c.call(ctx, n, "GET", "/ct-templates", nil, &r) != nil {
		return false
	}
	for _, t := range r.Templates {
		if t.Name == tpl {
			return true
		}
	}
	return false
}

func (c *CTs) localTemplate(ctx context.Context, tpl string) bool {
	for _, t := range c.Local.Templates(ctx) {
		if t.Name == tpl {
			return true
		}
	}
	return false
}

// Create makes a container on the server asked for, or the best one that has its template.
func (c *CTs) Create(ctx context.Context, r ct.CreateRequest) (ct.Container, error) {
	ws := c.workers()
	if len(ws) == 0 {
		x, err := c.Local.Create(ctx, r)
		return c.selfTag(x), err
	}
	if c.Exists(r.Name) {
		return ct.Container{}, fmt.Errorf("%w: container %q", vm.ErrExists, r.Name)
	}
	vr := vm.CreateRequest{Name: r.Name, VCPUs: r.VCPUs, MemoryMiB: max(r.MemoryMiB, 512), DiskGB: max(r.DiskGB, 8), Network: r.Network, Node: r.Node,
		IPPool: r.IPPool} // public IPs: a server with the pool's bridge
	var target Node
	if r.Node != "" {
		t, err := c.V.place(ctx, vr)
		if err != nil {
			return ct.Container{}, err
		}
		target = t
		if target.ID != "" && !c.hasTemplate(ctx, target, r.Template) {
			return ct.Container{}, fmt.Errorf("%w: server %s has no container template %q: add it there first (Server page → Templates, with that server picked)",
				vm.ErrInvalid, target.Name, r.Template)
		}
	} else {
		for _, cd := range c.V.Candidates(ctx, vr) { // best first
			if cd.Reason != "" {
				continue
			}
			if cd.Local {
				if c.localTemplate(ctx, r.Template) {
					target = Node{}
					break
				}
				continue
			}
			if c.hasTemplate(ctx, cd.Node, r.Template) {
				target = cd.Node
				break
			}
		}
	}
	if target.ID == "" {
		x, err := c.Local.Create(ctx, r)
		return c.selfTag(x), err
	}
	r.Plan, r.Node = "", ""
	var x ct.Container
	if err := c.call(ctx, target, "POST", "/containers", r, &x); err != nil {
		return ct.Container{}, err
	}
	c.remember(x.Name, target.ID)
	return c.tag(x, target), nil
}

func (c *CTs) power(ctx context.Context, name, action string, body any) (ct.Container, error) {
	n, _ := c.Where(ctx, name)
	var x ct.Container
	err := c.call(ctx, n, "POST", cpath(name, "/", action), body, &x)
	return c.tag(x, n), err
}

func (c *CTs) Start(ctx context.Context, name string) (ct.Container, error) {
	if _, ok := c.Where(ctx, name); !ok {
		x, err := c.Local.Start(ctx, name)
		return c.selfTag(x), err
	}
	return c.power(ctx, name, "start", nil)
}

func (c *CTs) Stop(ctx context.Context, name string, force bool) (ct.Container, error) {
	if _, ok := c.Where(ctx, name); !ok {
		x, err := c.Local.Stop(ctx, name, force)
		return c.selfTag(x), err
	}
	return c.power(ctx, name, "stop", map[string]bool{"force": force})
}

func (c *CTs) Reboot(ctx context.Context, name string) (ct.Container, error) {
	if _, ok := c.Where(ctx, name); !ok {
		x, err := c.Local.Reboot(ctx, name)
		return c.selfTag(x), err
	}
	return c.power(ctx, name, "reboot", nil)
}

func (c *CTs) Delete(ctx context.Context, name string) error {
	n, ok := c.Where(ctx, name)
	if !ok {
		return c.Local.Delete(ctx, name)
	}
	err := c.call(ctx, n, "DELETE", cpath(name), nil, nil)
	if err == nil {
		c.remember(name, "")
	}
	return err
}

func (c *CTs) Resize(ctx context.Context, name string, r ct.ResizeRequest) (ct.Container, error) {
	n, ok := c.Where(ctx, name)
	if !ok {
		x, err := c.Local.Resize(ctx, name, r)
		return c.selfTag(x), err
	}
	var x ct.Container
	err := c.call(ctx, n, "PATCH", cpath(name), r, &x)
	return c.tag(x, n), err
}

func (c *CTs) SetOwner(ctx context.Context, name, owner string) (ct.Container, error) {
	n, ok := c.Where(ctx, name)
	if !ok {
		x, err := c.Local.SetOwner(ctx, name, owner)
		return c.selfTag(x), err
	}
	var x ct.Container
	err := c.call(ctx, n, "PUT", cpath(name, "/owner"), map[string]string{"owner": owner}, &x)
	return c.tag(x, n), err
}

func (c *CTs) SetPassword(ctx context.Context, name, user, password string) error {
	n, ok := c.Where(ctx, name)
	if !ok {
		return c.Local.SetPassword(ctx, name, user, password)
	}
	return c.call(ctx, n, "POST", cpath(name, "/password"), map[string]string{"user": user, "password": password}, nil)
}

func (c *CTs) SetupSSH(ctx context.Context, name string) (string, error) {
	n, ok := c.Where(ctx, name)
	if !ok {
		return c.Local.SetupSSH(ctx, name)
	}
	var r struct {
		Output string `json:"output"`
	}
	err := c.call(ctx, n, "POST", cpath(name, "/setup-ssh"), nil, &r)
	return r.Output, err
}

func (c *CTs) Snapshots(ctx context.Context, name string) ([]ct.Snapshot, error) {
	n, ok := c.Where(ctx, name)
	if !ok {
		return c.Local.Snapshots(ctx, name)
	}
	var r struct {
		Snapshots []ct.Snapshot `json:"snapshots"`
	}
	err := c.call(ctx, n, "GET", cpath(name, "/snapshots"), nil, &r)
	return r.Snapshots, err
}

func (c *CTs) CreateSnapshot(ctx context.Context, name, snap string) (ct.Snapshot, error) {
	n, ok := c.Where(ctx, name)
	if !ok {
		return c.Local.CreateSnapshot(ctx, name, snap)
	}
	var s ct.Snapshot
	err := c.call(ctx, n, "POST", cpath(name, "/snapshots"), map[string]string{"name": snap}, &s)
	return s, err
}

func (c *CTs) Rollback(ctx context.Context, name, snap string, destroyNewer bool) (ct.Container, error) {
	n, ok := c.Where(ctx, name)
	if !ok {
		x, err := c.Local.Rollback(ctx, name, snap, destroyNewer)
		return c.selfTag(x), err
	}
	var x ct.Container
	err := c.call(ctx, n, "POST", cpath(name, "/snapshots/", url.PathEscape(snap), "/rollback"), map[string]bool{"destroy_newer": destroyNewer}, &x)
	return c.tag(x, n), err
}

func (c *CTs) DeleteSnapshot(ctx context.Context, name, snap string) error {
	n, ok := c.Where(ctx, name)
	if !ok {
		return c.Local.DeleteSnapshot(ctx, name, snap)
	}
	return c.call(ctx, n, "DELETE", cpath(name, "/snapshots/", url.PathEscape(snap)), nil, nil)
}

// --- backups (they stay on the server that made them) ---

func (c *CTs) ListBackups(name string) ([]backup.Meta, error) {
	l, err := c.Local.ListBackups(name)
	if err != nil {
		return nil, err
	}
	for _, n := range c.workers() {
		var r struct {
			Backups []backup.Meta `json:"backups"`
		}
		p := "/ct-backups"
		if name != "" {
			p = cpath(name, "/backups")
		}
		cctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		if c.M.call(cctx, n, "GET", p, nil, &r) == nil {
			l = append(l, r.Backups...)
		}
		cancel()
	}
	return l, nil
}

func (c *CTs) backupNode(name, id string) (Node, bool) {
	if l, err := c.Local.ListBackups(name); err == nil {
		for _, b := range l {
			if b.ID == id {
				return Node{}, false
			}
		}
	}
	for _, n := range c.workers() {
		var r struct {
			Backups []backup.Meta `json:"backups"`
		}
		if c.call(context.Background(), n, "GET", "/ct-backups", nil, &r) == nil {
			for _, b := range r.Backups {
				if b.VM == name && b.ID == id {
					return n, true
				}
			}
		}
	}
	return Node{}, false
}

func (c *CTs) DeleteBackup(name, id string) error {
	n, ok := c.backupNode(name, id)
	if !ok {
		return c.Local.DeleteBackup(name, id)
	}
	return c.call(context.Background(), n, "DELETE", "/ct-backups/"+url.PathEscape(name)+"/"+url.PathEscape(id), nil, nil)
}

func (c *CTs) StartBackup(ctx context.Context, name string, keep int) (jobs.Job, error) {
	n, ok := c.Where(ctx, name)
	if !ok {
		return c.Local.StartBackup(ctx, name, keep)
	}
	var j jobs.Job
	err := c.call(ctx, n, "POST", cpath(name, "/backups"), nil, &j)
	return j, err
}

func (c *CTs) StartRestore(ctx context.Context, name, id, newName string, exists func(string) bool) (jobs.Job, error) {
	n, ok := c.backupNode(name, id)
	if !ok {
		return c.Local.StartRestore(ctx, name, id, newName, exists)
	}
	var j jobs.Job
	err := c.call(ctx, n, "POST", "/ct-backups/"+url.PathEscape(name)+"/"+url.PathEscape(id)+"/restore", map[string]string{"name": newName}, &j)
	return j, err
}

// --- terminal and moves ---

// DialShell opens a shell in a container on a worker (through the cluster tunnel).
func (c *CTs) DialShell(ctx context.Context, n Node, name string) (net.Conn, error) {
	return c.M.dialUpgrade(ctx, n, "/api/v1/cluster/ct-shell/"+url.PathEscape(name), "p8s-shell")
}

// Move moves a container to another server (a job): the disk goes across
// the tunnel, then the old one is deleted. A running container is copied
// while it runs, then shut down for the last changes only and started on the
// new server (it is off for its shutdown and that last copy). start: start a
// stopped one there.
func (c *CTs) Move(ctx context.Context, name, to string, start bool) (jobs.Job, error) {
	if c.M == nil || c.M.Role() != "master" {
		return jobs.Job{}, fmt.Errorf("%w: move containers from the cluster master", vm.ErrConflict)
	}
	src, remote := c.Where(ctx, name)
	if !remote && !c.Local.Exists(name) {
		return jobs.Job{}, fmt.Errorf("%w: container %q", vm.ErrNotFound, name)
	}
	var dst Node
	toSelf := false
	if strings.EqualFold(to, c.M.SelfName()) || to == c.M.SelfID() {
		toSelf = true
	} else {
		for _, n := range c.workers() {
			if n.ID == to || strings.EqualFold(n.Name, to) {
				dst = n
			}
		}
		if dst.ID == "" {
			return jobs.Job{}, fmt.Errorf("%w: no server %q in the cluster", vm.ErrInvalid, to)
		}
	}
	if toSelf && !remote || remote && dst.ID == src.ID {
		return jobs.Job{}, fmt.Errorf("%w: container %q is on that server already", vm.ErrConflict, name)
	}
	// no public IP; running is fine (two rounds)
	var x ct.Container
	if remote {
		if err := c.call(ctx, src, "GET", cpath(name), nil, &x); err != nil {
			return jobs.Job{}, err
		}
	} else {
		var err error
		if x, err = c.Local.Get(ctx, name); err != nil {
			return jobs.Job{}, err
		}
	}
	if x.PublicIP != "" || x.Network == vm.NetPublic { // its address comes along: the bridge must be there
		if x.Public == nil || c.V == nil {
			return jobs.Job{}, fmt.Errorf("%w: %s has a public IP from its server's pool; it can't take it along", vm.ErrConflict, name)
		}
		id := dst.ID
		if toSelf {
			id = c.M.SelfID()
		}
		if why := c.V.bridgeReason(ctx, id, x.Public.Bridge, x.Public.Pool); why != "" {
			return jobs.Job{}, fmt.Errorf("%w: that server can't take %s: %s", vm.ErrConflict, name, why)
		}
	}
	switch x.State {
	case "stopped", "running":
	default:
		return jobs.Job{}, fmt.Errorf("%w: %s is %s; try again in a moment", vm.ErrConflict, name, x.State)
	}
	live := x.State == "running"
	if c.V == nil || c.V.Jobs == nil {
		return jobs.Job{}, fmt.Errorf("%w: background jobs aren't set up", vm.ErrConflict)
	}
	// transfer sends one round (phase "" = all of a stopped container).
	transfer := func(ctx context.Context, phase string) (string, error) {
		pr, pw := io.Pipe()
		errc := make(chan error, 1)
		go func() {
			var err error
			if remote {
				p := "/cluster/ct-export/" + url.PathEscape(name)
				if phase != "" {
					p += "?phase=" + phase
				}
				var resp *http.Response
				resp, err = c.M.stream(ctx, src, "GET", p, nil)
				if err == nil {
					_, err = io.Copy(pw, resp.Body)
					resp.Body.Close()
				}
			} else {
				err = c.Local.ExportPhase(ctx, name, phase, pw)
			}
			pw.CloseWithError(err)
			errc <- err
		}()
		var note string
		var err error
		if toSelf {
			_, note, err = c.Local.Import(ctx, pr)
		} else {
			var resp *http.Response
			resp, err = c.M.stream(ctx, dst, "POST", "/cluster/ct-import", pr)
			if err == nil {
				var r struct {
					Note string `json:"note"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&r)
				resp.Body.Close()
				note = r.Note
			}
		}
		pr.CloseWithError(err)
		if serr := <-errc; serr != nil && err == nil {
			err = serr
		}
		return note, err
	}
	// dropHalf deletes the new server's disk after a failed live move.
	dropHalf := func(ctx context.Context) {
		if toSelf {
			_ = c.Local.DropMove(ctx, name)
		} else {
			_ = c.call(ctx, dst, "DELETE", "/cluster/ct-import/"+url.PathEscape(name), nil, nil)
		}
	}
	return c.V.Jobs.Start("move-ct", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		bg := context.WithoutCancel(ctx)
		var note string
		var err error
		if live {
			p("copying the disk while it runs")
			if _, err := transfer(ctx, ct.PhasePre); err != nil {
				dropHalf(bg)
				return "", fmt.Errorf("copy (it keeps running here): %w", err)
			}
			p("shutting it down for the last changes")
			if err := c.stopLong(ctx, src, remote, name); err != nil {
				dropHalf(bg)
				return "", fmt.Errorf("shut it down: %w (it stays here)", err)
			}
			p("copying the last changes")
			if note, err = transfer(ctx, ct.PhaseFinal); err != nil {
				dropHalf(bg)
				msg := "it stays here"
				if _, serr := c.Start(bg, name); serr != nil {
					msg += ", but starting it again failed: " + serr.Error()
				} else {
					msg += " and runs again"
				}
				return "", fmt.Errorf("copy the last changes: %w (%s)", err, msg)
			}
			start = true
		} else {
			p("copying the disk")
			if note, err = transfer(ctx, ""); err != nil {
				return "", err
			}
		}
		p("removing it from the old server")
		if remote {
			err = c.call(bg, src, "DELETE", cpath(name)+"?moved=1", nil, nil)
		} else if d, ok := any(c.Local).(interface {
			DeleteMoved(ctx context.Context, name string) error
		}); ok {
			err = d.DeleteMoved(bg, name) // its public address stays with it
		} else {
			err = c.Local.Delete(bg, name)
		}
		if err != nil {
			note += "; the old copy couldn't be deleted: " + err.Error()
		}
		where := c.M.SelfName()
		if toSelf {
			c.remember(name, "")
		} else {
			c.remember(name, dst.ID)
			where = dst.Name
		}
		if start {
			p("starting it")
			if _, err := c.Start(bg, name); err != nil {
				note += "; starting it failed: " + err.Error()
			} else {
				note += "; started"
			}
		}
		return fmt.Sprintf("container %s is on %s now%s", name, where, note), nil
	})
}

// stopLong shuts a container down cleanly, waiting up to its 60 s shutdown.
func (c *CTs) stopLong(ctx context.Context, src Node, remote bool, name string) error {
	if !remote {
		_, err := c.Local.Stop(ctx, name, false)
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	var x ct.Container
	if err := errFrom(c.M.call(ctx, src, "POST", cpath(name, "/stop"), map[string]bool{"force": false}, &x)); err != nil {
		return err
	}
	if x.State != "stopped" {
		return fmt.Errorf("it is still %s", x.State)
	}
	return nil
}
