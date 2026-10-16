package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"p8sos/internal/ippool"
	"p8sos/internal/vm"
)

// Public IP pools across the cluster: the master's pools are the cluster's.
// A worker reads them from the copy the master pushes with its settings and
// asks the master for every address it hands out or gives back.

// PoolsFile is the pools file's name in the data directory (and in the book).
const PoolsFile = "ippools.json"

// PoolsLeader makes a worker's ippool.Store follow the master's pools.
type PoolsLeader struct{ M *Manager }

var _ ippool.Leader = PoolsLeader{}

// Following: this server is a worker now.
func (p PoolsLeader) Following() bool { return p.M != nil && p.M.Role() == "worker" }

// MasterPools is the master's pools file as last received (nil: none yet).
func (p PoolsLeader) MasterPools() ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(p.M.standbyDir(), PoolsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// Apply runs a change on the master's pools and keeps its new pools file.
func (p PoolsLeader) Apply(op ippool.Op) (ippool.Result, error) {
	m := p.M
	m.mu.Lock()
	_ = m.load()
	var master Node
	if m.st.Role == "worker" && len(m.st.Nodes) > 0 {
		master = m.st.Nodes[0]
	}
	m.mu.Unlock()
	if master.ID == "" || master.Token == "" {
		return ippool.Result{}, errors.New("public addresses come from the master's pools, and this server hasn't heard from the master yet: try again in a minute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var r ippool.Result
	if err := m.call(ctx, master, "POST", "/cluster/ippool", op, &r); err != nil {
		return ippool.Result{}, errors.New("public addresses come from the master's pools, and the master " + master.Name + " didn't answer: " + err.Error())
	}
	if r.File != nil {
		m.mu.Lock()
		m.saveStandby(map[string][]byte{PoolsFile: r.File})
		m.mu.Unlock()
	}
	return r, nil
}

// LocalBridges lists this server's network bridges (/sys/class/net/*/bridge).
func LocalBridges() []string {
	m, _ := filepath.Glob("/sys/class/net/*/bridge")
	out := []string{}
	for _, p := range m {
		out = append(out, filepath.Base(filepath.Dir(p)))
	}
	return out
}

// poolReason says why server cd can't give a VM or container an address
// from pool ("" = it can).
func (c *VMs) poolReason(cd Candidate, pool string) string {
	if pool == "" || cd.Local {
		return ""
	}
	if c.PoolBridge == nil || !cd.Cap.SharedPools {
		return "public IPs from the pools need P8S OS updated on it"
	}
	br, ok := c.PoolBridge(pool)
	if !ok {
		return "" // an unknown pool: the create says so
	}
	if !contains(cd.Cap.Bridges, br) {
		return "no bridge " + br + " for public IP pool " + pool + " (run: sudo p8 lan-setup there)"
	}
	return ""
}

type publicIPChanger interface {
	ChangePublicIPs(ctx context.Context, name string, r vm.PublicIPRequest) (vm.PublicIPResult, error)
}

var errNoPublicIPs = fmt.Errorf("%w: public IPs can't be changed on this server", vm.ErrConflict)

// ChangePublicIPs changes a VM's public addresses on the server that has it.
func (c *VMs) ChangePublicIPs(ctx context.Context, name string, r vm.PublicIPRequest) (vm.PublicIPResult, error) {
	if n, remote := c.locate(ctx, name); remote {
		var res vm.PublicIPResult
		err := c.call(ctx, n, "POST", vpath(name, "/public-ips"), r, &res)
		return res, err
	}
	l, ok := c.Local.(publicIPChanger)
	if !ok {
		return vm.PublicIPResult{}, errNoPublicIPs
	}
	return l.ChangePublicIPs(ctx, name, r)
}

// ChangePublicIPs changes a container's public addresses on the server that has it.
func (c *CTs) ChangePublicIPs(ctx context.Context, name string, r vm.PublicIPRequest) (vm.PublicIPResult, error) {
	if n, remote := c.Where(ctx, name); remote {
		var res vm.PublicIPResult
		err := c.call(ctx, n, "POST", cpath(name, "/public-ips"), r, &res)
		return res, err
	}
	l, ok := any(c.Local).(publicIPChanger)
	if !ok {
		return vm.PublicIPResult{}, errNoPublicIPs
	}
	return l.ChangePublicIPs(ctx, name, r)
}
