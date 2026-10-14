package ceph

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Peer is a server of the P8S cluster.
type Peer struct {
	ID     string
	Name   string
	Self   bool
	Online bool
}

// Peers reaches the other servers (the P8S cluster); nil = this server alone.
type Peers interface {
	List() []Peer
	Call(ctx context.Context, id, method, path string, in, out any) error
}

// NodeRequest is what a step on one server needs.
type NodeRequest struct {
	Network string `json:"network,omitempty"` // init
	Disk    string `json:"disk,omitempty"`    // osd
	OSD     int    `json:"osd,omitempty"`     // osd-remove
}

// Actions a server runs for itself (POST /ceph/node/{action}).
const (
	ActInstall   = "install"
	ActInit      = "init"
	ActMon       = "mon"
	ActMgr       = "mgr"
	ActOSD       = "osd"
	ActOSDRemove = "osd-remove"
)

// Do starts a step on this server (in the background; see NodeInfo.Task).
// Quick checks happen first, so a bad request fails right away.
func (m *Manager) Do(action string, r NodeRequest) error {
	switch action {
	case ActInstall:
		if m.Installed() {
			return fmt.Errorf("%w: Ceph is already installed here", ErrConflict)
		}
		return m.Start(action, "", m.Install)
	case ActInit:
		if _, _, err := m.CheckInit(r.Network); err != nil {
			return err
		}
		return m.Start(action, r.Network, func(ctx context.Context) error { return m.Init(ctx, r.Network) })
	case ActMon:
		if err := m.needConfigured(); err != nil {
			return err
		}
		if m.exists(m.monDir()) {
			return fmt.Errorf("%w: this server already runs a monitor", ErrConflict)
		}
		return m.Start(action, "", m.CreateMon)
	case ActMgr:
		if err := m.needConfigured(); err != nil {
			return err
		}
		if m.exists(m.mgrDir()) {
			return fmt.Errorf("%w: this server already runs a manager", ErrConflict)
		}
		return m.Start(action, "", m.CreateMgr)
	case ActOSD:
		if err := m.CheckOSD(context.Background(), r.Disk); err != nil {
			return err
		}
		return m.Start(action, r.Disk, func(ctx context.Context) error { return m.CreateOSD(ctx, r.Disk) })
	case ActOSDRemove:
		if err := m.needConfigured(); err != nil {
			return err
		}
		id := r.OSD
		return m.Start(action, fmt.Sprintf("osd.%d", id), func(ctx context.Context) error {
			done, err := m.RemoveOSD(ctx, id)
			if err == nil && !done {
				m.mu.Lock()
				m.task.Detail = fmt.Sprintf("osd.%d taken out: Ceph is moving its data to the other disks. Press Remove again when that is done", id)
				m.mu.Unlock()
			}
			return err
		})
	}
	return fmt.Errorf("%w: unknown step %q", ErrInvalid, action)
}

// Cluster drives Ceph across the servers (on the master, or a server alone).
type Cluster struct {
	Local *Manager
	Peers Peers

	mu sync.Mutex
}

// NodeView is one server in the overview.
type NodeView struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Self   bool      `json:"self"`
	Online bool      `json:"online"`
	Info   *NodeInfo `json:"info,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// Overview is everything the Ceph page shows.
type Overview struct {
	Installed   bool       `json:"installed"`  // on this server
	Configured  bool       `json:"configured"` // Ceph is set up
	Network     string     `json:"network,omitempty"`
	Status      *Status    `json:"status,omitempty"`
	StatusError string     `json:"status_error,omitempty"`
	PoolReady   bool       `json:"pool_ready"`
	Pool        string     `json:"pool"`
	Nodes       []NodeView `json:"nodes"`
	Synced      []string   `json:"synced,omitempty"` // servers that just got the configuration
}

func (c *Cluster) peers() []Peer {
	if c.Peers == nil {
		return []Peer{{ID: "", Name: c.Local.Name, Self: true, Online: true}}
	}
	l := c.Peers.List()
	if len(l) == 0 {
		return []Peer{{ID: "", Name: c.Local.Name, Self: true, Online: true}}
	}
	return l
}

func (c *Cluster) peer(id string) (Peer, error) {
	for _, p := range c.peers() {
		if p.ID == id || (id == "" && p.Self) || p.Name == id {
			return p, nil
		}
	}
	return Peer{}, fmt.Errorf("%w: no server %q", ErrInvalid, id)
}

// Overview collects every server's state and brings their configuration up
// to date (a new monitor changes it).
func (c *Cluster) Overview(ctx context.Context) Overview {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := Overview{Installed: c.Local.Installed(), Configured: c.Local.Configured(), Pool: Pool, Nodes: []NodeView{}}
	var bundle Bundle
	if o.Configured {
		_, _ = c.Local.RefreshConf(ctx)
		if b, err := c.Local.Bundle(); err == nil {
			bundle = b
		}
		if n, err := NetworkOf(bundle.Conf); err == nil {
			o.Network = n.String()
		}
	}
	for _, p := range c.peers() {
		v := NodeView{ID: p.ID, Name: p.Name, Self: p.Self, Online: p.Online}
		if p.Self {
			in := c.Local.Info(ctx)
			v.Info, v.Online = &in, true
		} else if p.Online {
			var in NodeInfo
			if err := c.Peers.Call(ctx, p.ID, "GET", "/ceph/node", nil, &in); err != nil {
				v.Error = err.Error()
			} else {
				v.Info = &in
			}
		}
		// hand the configuration to servers that have Ceph but an old (or no) copy
		if bundle.Conf != "" && !p.Self && v.Info != nil && v.Info.Installed && v.Info.Hash != bundle.Hash() {
			if err := c.Peers.Call(ctx, p.ID, "POST", "/ceph/node/config", bundle, nil); err != nil {
				v.Error = "sending the Ceph configuration: " + err.Error()
			} else {
				v.Info.Configured, v.Info.Hash = true, bundle.Hash()
				o.Synced = append(o.Synced, p.Name)
			}
		}
		o.Nodes = append(o.Nodes, v)
	}
	if o.Configured {
		if st, err := c.Local.Status(ctx); err != nil {
			o.StatusError = err.Error()
		} else {
			o.Status = &st
			for _, p := range st.Pools {
				if p.Name == Pool {
					o.PoolReady = true
				}
			}
		}
	}
	return o
}

// Run starts a step on a server ("" = this one).
func (c *Cluster) Run(ctx context.Context, node, action string, r NodeRequest) error {
	p, err := c.peer(node)
	if err != nil {
		return err
	}
	if action == ActInit && !p.Self {
		return fmt.Errorf("%w: start Ceph on the master (this server); add the others after", ErrInvalid)
	}
	if action != ActInstall && action != ActInit && !c.Local.Configured() {
		return fmt.Errorf("%w: set Ceph up first", ErrConflict)
	}
	if p.Self {
		return c.Local.Do(action, r)
	}
	if !p.Online {
		return fmt.Errorf("%w: server %s is offline", ErrConflict, p.Name)
	}
	if action != ActInstall { // it needs the configuration first
		_ = c.Overview(ctx)
	}
	return c.Peers.Call(ctx, p.ID, "POST", "/ceph/node/"+action, r, nil)
}

// RemoveOSD takes a disk out of Ceph on the server that has it.
func (c *Cluster) RemoveOSD(ctx context.Context, id int) error {
	o := c.Overview(ctx)
	for _, n := range o.Nodes {
		if n.Info == nil {
			continue
		}
		for _, x := range n.Info.OSDs {
			if x == id {
				return c.Run(ctx, n.ID, ActOSDRemove, NodeRequest{OSD: id})
			}
		}
	}
	if o.Status != nil {
		for _, x := range o.Status.OSDs {
			if x.ID == id {
				return fmt.Errorf("%w: osd.%d is on %s, which can't be reached", ErrConflict, id, x.Host)
			}
		}
	}
	return fmt.Errorf("%w: no osd.%d", ErrInvalid, id)
}

// CreatePool makes the VM pool. size 0 = 3 copies, or one per server with disks when fewer.
func (c *Cluster) CreatePool(ctx context.Context, size int) (int, error) {
	if !c.Local.Configured() {
		return 0, fmt.Errorf("%w: set Ceph up first", ErrConflict)
	}
	st, err := c.Local.Status(ctx)
	if err != nil {
		return 0, err
	}
	hosts := map[string]bool{}
	for _, o := range st.OSDs {
		if o.Up && o.In && o.Host != "" {
			hosts[o.Host] = true
		}
	}
	if len(hosts) == 0 {
		return 0, fmt.Errorf("%w: give Ceph at least one disk first", ErrConflict)
	}
	if size == 0 {
		size = min(3, len(hosts))
	}
	if size > len(hosts) {
		return 0, fmt.Errorf("%w: %d copies need disks on %d servers (there are %d)", ErrInvalid, size, size, len(hosts))
	}
	return size, c.Local.CreatePool(ctx, size)
}

// Advice is what the page suggests doing next.
func (o Overview) Advice() []string {
	var a []string
	if !o.Configured {
		return []string{"Install Ceph on this server, then set it up with the network the servers share."}
	}
	mons, osdHosts := 0, map[string]bool{}
	for _, n := range o.Nodes {
		if n.Info != nil && n.Info.Mon {
			mons++
		}
	}
	if o.Status != nil {
		for _, x := range o.Status.OSDs {
			osdHosts[x.Host] = true
		}
	}
	if mons < 3 && len(o.Nodes) >= 3 {
		a = append(a, fmt.Sprintf("Add monitors until there are 3 (now %d), each on a different server, so Ceph keeps working when one is off.", mons))
	}
	if len(osdHosts) < 3 {
		a = append(a, "Give Ceph empty disks on at least 3 servers for 3 copies of every block.")
	}
	if !o.PoolReady {
		a = append(a, "Create the VM pool; then pick storage \"ceph\" when making a VM.")
	}
	if o.Status != nil && o.Status.Health != "HEALTH_OK" && len(o.Status.Problems) > 0 {
		a = append(a, "Ceph reports: "+strings.Join(o.Status.Problems, "; "))
	}
	return a
}
