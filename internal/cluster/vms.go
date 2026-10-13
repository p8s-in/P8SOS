package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"p8sos/internal/jobs"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"p8sos/internal/firewall"
	"p8sos/internal/vm"
)

// Local is this server's own VM service (the methods the API uses).
type Local interface {
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
	Screenshot(ctx context.Context, name string) ([]byte, error)
	SetPassword(ctx context.Context, name, user, password string) error
	SetOwner(ctx context.Context, name, owner string) (vm.VM, error)
	GetFirewall(ctx context.Context, name string) (firewall.Settings, error)
	SetFirewall(ctx context.Context, name string, fw firewall.Settings) (firewall.Settings, error)
}

// VMs is every VM of the cluster behind the one VM interface the API uses:
// local calls go to this server, the rest to the worker that has the VM.
// On a server that isn't a master it is just the local service.
type VMs struct {
	Local
	M    *Manager
	Jobs *jobs.Manager // runs moves (the local service's job list, so /jobs shows them)

	mu    sync.Mutex
	where map[string]string  // vm -> node id (workers only)
	last  map[string][]vm.VM // node id -> its VMs as last seen (shown while it is offline)
}

const remoteTimeout = 20 * time.Second

func (c *VMs) workers() []Node {
	if c.M == nil {
		return nil
	}
	return c.M.Workers()
}

func (c *VMs) node(id string) (Node, bool) {
	for _, n := range c.workers() {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// errFrom turns a worker's answer into the errors the API understands.
func errFrom(err error) error {
	var e *APIError
	if !errors.As(err, &e) {
		return err
	}
	switch e.Code {
	case "not_found":
		return fmt.Errorf("%w: %s", vm.ErrNotFound, e.Message)
	case "invalid":
		return fmt.Errorf("%w: %s", vm.ErrInvalid, e.Message)
	case "exists":
		return fmt.Errorf("%w: %s", vm.ErrExists, e.Message)
	case "conflict", "busy":
		return fmt.Errorf("%w: %s", vm.ErrConflict, e.Message)
	}
	return err
}

func (c *VMs) call(ctx context.Context, n Node, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	return errFrom(c.M.call(ctx, n, method, path, in, out))
}

func vpath(name string, rest ...string) string {
	return "/vms/" + url.PathEscape(name) + strings.Join(rest, "")
}

// locate finds the worker that has a VM (ok=false: local, or nowhere).
func (c *VMs) locate(ctx context.Context, name string) (Node, bool) {
	ws := c.workers()
	if len(ws) == 0 {
		return Node{}, false
	}
	if _, err := c.Local.Get(ctx, name); err == nil {
		return Node{}, false
	}
	c.mu.Lock()
	id := c.where[name]
	c.mu.Unlock()
	if n, ok := c.node(id); ok {
		return n, true
	}
	for _, n := range ws {
		var v vm.VM
		if err := c.call(ctx, n, "GET", vpath(name), nil, &v); err == nil {
			c.remember(name, n.ID)
			return n, true
		}
	}
	return Node{}, false
}

func (c *VMs) remember(name, id string) {
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

func (c *VMs) tag(v vm.VM, n Node) vm.VM {
	v.Node = n.Name
	v.NodeID = n.ID
	return v
}

func (c *VMs) selfTag(v vm.VM) vm.VM {
	if c.M != nil && c.M.Role() != "" {
		v.Node, v.NodeID = c.M.SelfName(), c.M.SelfID()
	}
	return v
}

// --- list and get ---

func (c *VMs) List(ctx context.Context) ([]vm.VM, error) {
	local, err := c.Local.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]vm.VM, 0, len(local))
	for _, v := range local {
		out = append(out, c.selfTag(v))
	}
	ws := c.workers()
	if len(ws) == 0 {
		return out, nil
	}
	type res struct {
		n   Node
		vms []vm.VM
		err error
	}
	ch := make(chan res, len(ws))
	for _, n := range ws {
		go func() {
			var r struct {
				VMs []vm.VM `json:"vms"`
			}
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			err := c.M.call(cctx, n, "GET", "/vms", nil, &r)
			ch <- res{n, r.VMs, err}
		}()
	}
	for range ws {
		r := <-ch
		c.mu.Lock()
		if c.last == nil {
			c.last = map[string][]vm.VM{}
		}
		if r.err == nil {
			c.last[r.n.ID] = r.vms
		}
		vms := c.last[r.n.ID]
		c.mu.Unlock()
		for _, v := range vms {
			v = c.tag(v, r.n)
			if r.err != nil {
				v.State = "unknown" // its server doesn't answer right now
				v.IP = ""
			}
			c.remember(v.Name, r.n.ID)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *VMs) Get(ctx context.Context, name string) (vm.VM, error) {
	v, err := c.Local.Get(ctx, name)
	if err == nil || len(c.workers()) == 0 || !errors.Is(err, vm.ErrNotFound) {
		return c.selfTag(v), err
	}
	if n, ok := c.locate(ctx, name); ok {
		var rv vm.VM
		if err := c.call(ctx, n, "GET", vpath(name), nil, &rv); err != nil {
			return vm.VM{}, err
		}
		return c.tag(rv, n), nil
	}
	return vm.VM{}, err
}

// remoteVM runs a call that returns a VM on the VM's worker, or locally.
func (c *VMs) remoteVM(ctx context.Context, name string, local func() (vm.VM, error), method, path string, body any) (vm.VM, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		v, err := local()
		return c.selfTag(v), err
	}
	var v vm.VM
	if err := c.call(ctx, n, method, path, body, &v); err != nil {
		return vm.VM{}, err
	}
	return c.tag(v, n), nil
}

func (c *VMs) Start(ctx context.Context, name string) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) { return c.Local.Start(ctx, name) }, "POST", vpath(name, "/start"), nil)
}

func (c *VMs) Stop(ctx context.Context, name string, force bool) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) { return c.Local.Stop(ctx, name, force) }, "POST", vpath(name, "/stop"), map[string]bool{"force": force})
}

func (c *VMs) Reboot(ctx context.Context, name string) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) { return c.Local.Reboot(ctx, name) }, "POST", vpath(name, "/reboot"), nil)
}

func (c *VMs) Resize(ctx context.Context, name string, r vm.ResizeRequest) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) { return c.Local.Resize(ctx, name, r) }, "PATCH", vpath(name), r)
}

func (c *VMs) Rollback(ctx context.Context, name, snap string, destroyNewer bool) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) { return c.Local.Rollback(ctx, name, snap, destroyNewer) },
		"POST", vpath(name, "/snapshots/", url.PathEscape(snap), "/rollback"), map[string]bool{"destroy_newer": destroyNewer})
}

func (c *VMs) EjectISO(ctx context.Context, name string) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) { return c.Local.EjectISO(ctx, name) }, "POST", vpath(name, "/eject"), nil)
}

func (c *VMs) SetOwner(ctx context.Context, name, owner string) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) { return c.Local.SetOwner(ctx, name, owner) }, "PUT", vpath(name, "/owner"), map[string]string{"owner": owner})
}

func (c *VMs) Delete(ctx context.Context, name string) error {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.Delete(ctx, name)
	}
	if err := c.call(ctx, n, "DELETE", vpath(name), nil, nil); err != nil {
		return err
	}
	c.remember(name, "")
	return nil
}

func (c *VMs) Snapshots(ctx context.Context, name string) ([]vm.Snapshot, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.Snapshots(ctx, name)
	}
	var r struct {
		Snapshots []vm.Snapshot `json:"snapshots"`
	}
	err := c.call(ctx, n, "GET", vpath(name, "/snapshots"), nil, &r)
	return r.Snapshots, err
}

func (c *VMs) CreateSnapshot(ctx context.Context, name, snap string) (vm.Snapshot, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.CreateSnapshot(ctx, name, snap)
	}
	var s vm.Snapshot
	err := c.call(ctx, n, "POST", vpath(name, "/snapshots"), map[string]string{"name": snap}, &s)
	return s, err
}

func (c *VMs) DeleteSnapshot(ctx context.Context, name, snap string) error {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.DeleteSnapshot(ctx, name, snap)
	}
	return c.call(ctx, n, "DELETE", vpath(name, "/snapshots/", url.PathEscape(snap)), nil, nil)
}

// ConsoleAddr of a VM on a worker is "cluster:<node id>/<vm>"; Dial opens it.
func (c *VMs) ConsoleAddr(ctx context.Context, name string) (string, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.ConsoleAddr(ctx, name)
	}
	var v vm.VM
	if err := c.call(ctx, n, "GET", vpath(name), nil, &v); err != nil {
		return "", err
	}
	if v.State != "running" {
		return "", fmt.Errorf("%w: vm %q is not running", vm.ErrConflict, name)
	}
	return "cluster:" + n.ID + "/" + name, nil
}

// Dial connects to a console address (local VNC port, or a worker's VM).
func (c *VMs) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if rest, ok := strings.CutPrefix(addr, "cluster:"); ok {
		id, name, _ := strings.Cut(rest, "/")
		n, ok := c.node(id)
		if !ok {
			return nil, fmt.Errorf("server %s is not in the cluster any more", id)
		}
		return c.M.DialVNC(ctx, n, name)
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

func (c *VMs) Screenshot(ctx context.Context, name string) ([]byte, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.Screenshot(ctx, name)
	}
	var b []byte
	err := c.call(ctx, n, "GET", vpath(name, "/screenshot"), nil, &b)
	return b, err
}

func (c *VMs) SetPassword(ctx context.Context, name, user, password string) error {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.SetPassword(ctx, name, user, password)
	}
	return c.call(ctx, n, "POST", vpath(name, "/password"), map[string]string{"user": user, "password": password}, nil)
}

func (c *VMs) GetFirewall(ctx context.Context, name string) (firewall.Settings, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.GetFirewall(ctx, name)
	}
	var fw firewall.Settings
	err := c.call(ctx, n, "GET", vpath(name, "/firewall"), nil, &fw)
	return fw, err
}

func (c *VMs) SetFirewall(ctx context.Context, name string, fw firewall.Settings) (firewall.Settings, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.SetFirewall(ctx, name, fw)
	}
	var out firewall.Settings
	err := c.call(ctx, n, "PUT", vpath(name, "/firewall"), fw, &out)
	return out, err
}

// --- backups and jobs ---

func (c *VMs) StartBackup(ctx context.Context, name string) (vm.Job, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		return c.Local.StartBackup(ctx, name)
	}
	var j vm.Job
	err := c.call(ctx, n, "POST", vpath(name, "/backups"), nil, &j)
	return j, err
}

// ListBackups: a VM's backups live on its server; "" = every server's.
func (c *VMs) ListBackups(name string) ([]vm.Backup, error) {
	ctx := context.Background()
	local, err := c.Local.ListBackups(name)
	if err != nil || len(c.workers()) == 0 {
		return local, err
	}
	out := append([]vm.Backup(nil), local...)
	for _, n := range c.workers() {
		var r struct {
			Backups []vm.Backup `json:"backups"`
		}
		path := "/backups"
		if name != "" {
			path = vpath(name, "/backups")
		}
		if err := c.call(ctx, n, "GET", path, nil, &r); err != nil {
			continue // offline: its backups show again when it is back
		}
		for _, b := range r.Backups {
			b.Node = n.Name
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// backupNode finds the worker that keeps a backup (ok=false: local).
func (c *VMs) backupNode(name, id string) (Node, bool) {
	if l, err := c.Local.ListBackups(name); err == nil {
		for _, b := range l {
			if b.ID == id {
				return Node{}, false
			}
		}
	}
	for _, n := range c.workers() {
		var r struct {
			Backups []vm.Backup `json:"backups"`
		}
		if c.call(context.Background(), n, "GET", vpath(name, "/backups"), nil, &r) == nil {
			for _, b := range r.Backups {
				if b.ID == id {
					return n, true
				}
			}
		}
	}
	return Node{}, false
}

func (c *VMs) DeleteBackup(name, id string) error {
	n, ok := c.backupNode(name, id)
	if !ok {
		return c.Local.DeleteBackup(name, id)
	}
	return c.call(context.Background(), n, "DELETE", "/backups/"+url.PathEscape(name)+"/"+url.PathEscape(id), nil, nil)
}

func (c *VMs) StartRestore(ctx context.Context, name, id, newName string) (vm.Job, error) {
	n, ok := c.backupNode(name, id)
	if !ok {
		return c.Local.StartRestore(ctx, name, id, newName)
	}
	var j vm.Job
	err := c.call(ctx, n, "POST", "/backups/"+url.PathEscape(name)+"/"+url.PathEscape(id)+"/restore", map[string]string{"name": newName}, &j)
	return j, err
}

func (c *VMs) GetSchedule(name string) (vm.Schedule, error) {
	if !strings.Contains(name, ":") {
		if n, ok := c.locate(context.Background(), name); ok {
			var s vm.Schedule
			err := c.call(context.Background(), n, "GET", vpath(name, "/backup-schedule"), nil, &s)
			return s, err
		}
	}
	return c.Local.GetSchedule(name)
}

func (c *VMs) SetSchedule(name string, s vm.Schedule) error {
	if !strings.Contains(name, ":") {
		if n, ok := c.locate(context.Background(), name); ok {
			return c.call(context.Background(), n, "PUT", vpath(name, "/backup-schedule"), s, nil)
		}
	}
	return c.Local.SetSchedule(name, s)
}

func (c *VMs) Job(id string) (vm.Job, bool) {
	if j, ok := c.Local.Job(id); ok {
		return j, true
	}
	for _, n := range c.workers() {
		var j vm.Job
		if c.call(context.Background(), n, "GET", "/jobs/"+url.PathEscape(id), nil, &j) == nil && j.ID != "" {
			return j, true
		}
	}
	return vm.Job{}, false
}

func (c *VMs) ListJobs() []vm.Job {
	out := c.Local.ListJobs()
	for _, n := range c.workers() {
		var r struct {
			Jobs []vm.Job `json:"jobs"`
		}
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if c.M.call(cctx, n, "GET", "/jobs", nil, &r) == nil {
			out = append(out, r.Jobs...)
		}
		cancel()
	}
	return out
}

// --- create and placement ---

// Create puts a new VM on the best server (or the one asked for).
func (c *VMs) Create(ctx context.Context, r vm.CreateRequest) (vm.VM, error) {
	ws := c.workers()
	if len(ws) == 0 {
		v, err := c.Local.Create(ctx, r)
		return c.selfTag(v), err
	}
	// names are unique in the whole cluster
	if _, err := c.Get(ctx, r.Name); err == nil {
		return vm.VM{}, fmt.Errorf("%w: vm %q", vm.ErrExists, r.Name)
	}
	if r.Storage != "" && r.Storage != "local" && r.Node == "" {
		// network storage is connected to one server: the one asked (this one by default)
		v, err := c.Local.Create(ctx, r)
		return c.selfTag(v), err
	}
	target, err := c.place(ctx, r)
	if err != nil {
		return vm.VM{}, err
	}
	if target.ID == "" {
		v, err := c.Local.Create(ctx, r)
		return c.selfTag(v), err
	}
	r.Plan, r.Node = "", "" // sized and billed by the master already
	var v vm.VM
	if err := c.call(ctx, target, "POST", "/vms", r, &v); err != nil {
		return vm.VM{}, err
	}
	c.remember(v.Name, target.ID)
	return c.tag(v, target), nil
}

// Candidate is one server a new VM could go to.
type Candidate struct {
	Node   Node
	Local  bool
	Online bool
	Cap    Capacity
	Reason string // why it can't take the VM ("" = it can)
}

func memFor(r vm.CreateRequest) int {
	if r.MemoryMiB > 0 {
		return r.MemoryMiB
	}
	return 1024
}

// Candidates scores every server for a request (best first).
func (c *VMs) Candidates(ctx context.Context, r vm.CreateRequest) []Candidate {
	var out []Candidate
	self := Candidate{Local: true, Online: true, Node: Node{Name: c.M.SelfName(), ID: c.M.SelfID()}}
	if c.M.LocalCapacity != nil {
		self.Cap = c.M.LocalCapacity(ctx)
	}
	out = append(out, self)
	for _, n := range c.workers() {
		st := c.M.NodeStatus(n.ID)
		out = append(out, Candidate{Node: n, Online: st.Online, Cap: st.Capacity})
	}
	need := memFor(r)
	for i := range out {
		cd := &out[i]
		switch {
		case !cd.Online:
			cd.Reason = "offline"
		case !cd.Local && (r.ISO != "" || r.IPPool != ""):
			cd.Reason = "ISO installs and public IPs use this server's library and pools"
		case r.Network == vm.NetLAN && !contains(cd.Cap.Networks, "lan"):
			cd.Reason = "no home network set up"
		case cd.Cap.MemTotalMiB > 0 && free(cd.Cap) < need:
			cd.Reason = fmt.Sprintf("not enough RAM (%d MB free for VMs)", max(free(cd.Cap), 0))
		case cd.Cap.PoolFreeGB > 0 && r.DiskGB > 0 && cd.Cap.PoolFreeGB < float64(r.DiskGB)/4:
			cd.Reason = "disk pool nearly full"
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Reason == "") != (out[j].Reason == "") {
			return out[i].Reason == ""
		}
		return free(out[i].Cap) > free(out[j].Cap)
	})
	return out
}

// free is RAM left for VMs: total minus what VMs got minus 1.5 GB for the host.
func free(c Capacity) int { return c.MemTotalMiB - c.MemUsedMiB - 1536 }

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (c *VMs) place(ctx context.Context, r vm.CreateRequest) (Node, error) {
	cands := c.Candidates(ctx, r)
	if r.Node != "" {
		for _, cd := range cands {
			if cd.Node.ID == r.Node || strings.EqualFold(cd.Node.Name, r.Node) {
				if cd.Reason != "" {
					return Node{}, fmt.Errorf("%w: server %s can't take it: %s", vm.ErrInvalid, cd.Node.Name, cd.Reason)
				}
				if cd.Local {
					return Node{}, nil
				}
				return cd.Node, nil
			}
		}
		return Node{}, fmt.Errorf("%w: no server %q in the cluster", vm.ErrInvalid, r.Node)
	}
	if len(cands) == 0 || cands[0].Reason != "" {
		why := []string{}
		for _, cd := range cands {
			why = append(why, cd.Node.Name+": "+cd.Reason)
		}
		return Node{}, fmt.Errorf("%w: no server has room for this VM (%s)", vm.ErrConflict, strings.Join(why, "; "))
	}
	if cands[0].Local {
		return Node{}, nil
	}
	return cands[0].Node, nil
}

// --- node status ---

// NodeStatus is the last seen status of a worker.
func (m *Manager) NodeStatus(id string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.status[id]; s != nil {
		return *s
	}
	return Status{}
}

// Poll asks every worker for its capacity (the master runs it every few seconds).
func (m *Manager) Poll(ctx context.Context) {
	hs := m.Handshakes(ctx)
	for _, n := range m.Workers() {
		var c Capacity
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := m.call(cctx, n, "GET", "/cluster/capacity", nil, &c)
		cancel()
		m.mu.Lock()
		if m.status == nil {
			m.status = map[string]*Status{}
		}
		s := m.status[n.ID]
		if s == nil {
			s = &Status{}
			m.status[n.ID] = s
		}
		if t, ok := hs[n.WGPub]; ok {
			s.Tunnel = t.UTC().Format(time.RFC3339)
		}
		if err != nil {
			if s.Online && m.Log != nil {
				m.Log.Warn("cluster server offline", "id", n.ID, "name", n.Name, "err", err)
			}
			s.Online, s.Error = false, err.Error()
		} else {
			if !s.Online && m.Log != nil {
				m.Log.Info("cluster server online", "id", n.ID, "name", n.Name)
			}
			s.Online, s.Error, s.Capacity, s.Seen = true, "", c, m.now().UTC().Format(time.RFC3339)
		}
		m.mu.Unlock()
	}
}

// Loop polls the workers while this server is the master.
func (m *Manager) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if m.Role() == "master" {
			m.Poll(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RemoteMetrics fetches a worker VM's usage points (charts); ok=false = local.
func (c *VMs) RemoteMetrics(name string) (any, bool) {
	ctx := context.Background()
	n, ok := c.locate(ctx, name)
	if !ok {
		return nil, false
	}
	var r map[string]any
	if c.call(ctx, n, "GET", vpath(name, "/metrics"), nil, &r) != nil {
		return map[string]any{"points": []any{}}, true
	}
	return r, true
}

// Firewall helpers the API reaches by type assertion (containers' firewalls
// and re-applying the table) stay on this server.
func (c *VMs) GuestFirewall(key string) (firewall.Settings, error) {
	if g, ok := c.Local.(interface {
		GuestFirewall(string) (firewall.Settings, error)
	}); ok {
		return g.GuestFirewall(key)
	}
	return firewall.Settings{}, errors.New("firewall not available")
}

func (c *VMs) SetGuestFirewall(ctx context.Context, key string, fw firewall.Settings) (firewall.Settings, error) {
	if g, ok := c.Local.(interface {
		SetGuestFirewall(context.Context, string, firewall.Settings) (firewall.Settings, error)
	}); ok {
		return g.SetGuestFirewall(ctx, key, fw)
	}
	return firewall.Settings{}, errors.New("firewall not available")
}

func (c *VMs) DropGuestFirewall(ctx context.Context, key string) error {
	if g, ok := c.Local.(interface {
		DropGuestFirewall(context.Context, string) error
	}); ok {
		return g.DropGuestFirewall(ctx, key)
	}
	return nil
}

func (c *VMs) ApplyFirewalls(ctx context.Context) error {
	if g, ok := c.Local.(interface{ ApplyFirewalls(context.Context) error }); ok {
		return g.ApplyFirewalls(ctx)
	}
	return nil
}

// Handshakes returns the last WireGuard handshake per peer key (tunnel health).
func (m *Manager) Handshakes(ctx context.Context) map[string]time.Time {
	out := map[string]time.Time{}
	if m.Run == nil {
		return out
	}
	b, err := m.Run.Run(ctx, "wg", "show", Iface, "latest-handshakes")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			var sec int64
			if _, err := fmt.Sscan(f[1], &sec); err == nil && sec > 0 {
				out[f[0]] = time.Unix(sec, 0)
			}
		}
	}
	return out
}

// SetAutostart sets whether a VM starts when its server boots.
func (c *VMs) SetAutostart(ctx context.Context, name string, on bool) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) {
		a, ok := c.Local.(interface {
			SetAutostart(context.Context, string, bool) (vm.VM, error)
		})
		if !ok {
			return vm.VM{}, fmt.Errorf("%w: autostart can't be changed here", vm.ErrConflict)
		}
		return a.SetAutostart(ctx, name, on)
	}, "PUT", vpath(name, "/autostart"), map[string]bool{"autostart": on})
}

// diskLocal is the data-disk part of the local VM service.
type diskLocal interface {
	AddDisk(ctx context.Context, name string, sizeGB int) (vm.VM, string, error)
	ResizeDisk(ctx context.Context, name string, index, sizeGB int) (vm.VM, error)
	RemoveDisk(ctx context.Context, name string, index int) (vm.VM, error)
}

var errNoDisks = fmt.Errorf("%w: extra disks aren't available on this server", vm.ErrInvalid)

// AddDisk gives a VM another disk, on whichever server has it.
func (c *VMs) AddDisk(ctx context.Context, name string, sizeGB int) (vm.VM, string, error) {
	n, ok := c.locate(ctx, name)
	if !ok {
		d, ok := c.Local.(diskLocal)
		if !ok {
			return vm.VM{}, "", errNoDisks
		}
		v, note, err := d.AddDisk(ctx, name, sizeGB)
		return c.selfTag(v), note, err
	}
	var out struct {
		vm.VM
		Note string `json:"note"`
	}
	if err := c.call(ctx, n, "POST", vpath(name, "/disks"), map[string]int{"size_gb": sizeGB}, &out); err != nil {
		return vm.VM{}, "", err
	}
	return c.tag(out.VM, n), out.Note, nil
}

func (c *VMs) ResizeDisk(ctx context.Context, name string, index, sizeGB int) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) {
		d, ok := c.Local.(diskLocal)
		if !ok {
			return vm.VM{}, errNoDisks
		}
		return d.ResizeDisk(ctx, name, index, sizeGB)
	}, "PATCH", vpath(name, "/disks/", strconv.Itoa(index)), map[string]int{"size_gb": sizeGB})
}

func (c *VMs) RemoveDisk(ctx context.Context, name string, index int) (vm.VM, error) {
	return c.remoteVM(ctx, name, func() (vm.VM, error) {
		d, ok := c.Local.(diskLocal)
		if !ok {
			return vm.VM{}, errNoDisks
		}
		return d.RemoveDisk(ctx, name, index)
	}, "DELETE", vpath(name, "/disks/", strconv.Itoa(index)), nil)
}
