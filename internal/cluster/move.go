package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync/atomic"

	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// Moving VMs between the servers of a cluster (shut down; the disk is copied
// over the tunnel, then the original is deleted).

// LocalMover is the local VM service's side of a move.
type LocalMover interface {
	MoveCheck(ctx context.Context, name string) (string, error)
	ExportVM(ctx context.Context, name string, w io.Writer) error
	ImportMovedVM(ctx context.Context, r io.Reader) (vm.VM, string, error)
}

// MoveTarget is a server a VM can be moved to.
type MoveTarget struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

// MoveInfo says whether and where a VM can be moved.
type MoveInfo struct {
	From    string       `json:"from"` // node id
	Why     string       `json:"why,omitempty"`
	Targets []MoveTarget `json:"targets"`
}

func (c *VMs) mover() (LocalMover, bool) {
	m, ok := c.Local.(LocalMover)
	return m, ok
}

// MoveInfo tells the dashboard what a move would do.
func (c *VMs) MoveInfo(ctx context.Context, name string) (MoveInfo, error) {
	lm, ok := c.mover()
	if !ok || c.M == nil || c.M.Role() != "master" {
		return MoveInfo{Why: "moving VMs needs a cluster (Server → Cluster → Add a server)", Targets: []MoveTarget{}}, nil
	}
	info := MoveInfo{Targets: []MoveTarget{}}
	n, remote := c.locate(ctx, name)
	if remote {
		info.From = n.ID
		var r struct {
			Why string `json:"why"`
		}
		if err := c.call(ctx, n, "GET", vpath(name, "/move-check"), nil, &r); err != nil {
			return info, err
		}
		info.Why = r.Why
	} else {
		if _, err := c.Local.Get(ctx, name); err != nil {
			return info, err
		}
		info.From = c.M.SelfID()
		why, err := lm.MoveCheck(ctx, name)
		if err != nil {
			return info, err
		}
		info.Why = why
	}
	if info.From != c.M.SelfID() {
		info.Targets = append(info.Targets, MoveTarget{ID: c.M.SelfID(), Name: c.M.SelfName(), Online: true})
	}
	for _, w := range c.workers() {
		if w.ID != info.From {
			info.Targets = append(info.Targets, MoveTarget{ID: w.ID, Name: w.Name, Online: c.M.NodeStatus(w.ID).Online})
		}
	}
	return info, nil
}

// StartMove copies a shut-down VM to another server and deletes the original.
func (c *VMs) StartMove(ctx context.Context, name, to string, start bool) (vm.Job, error) {
	if c.Jobs == nil {
		return vm.Job{}, fmt.Errorf("%w: moving isn't set up", vm.ErrConflict)
	}
	info, err := c.MoveInfo(ctx, name)
	if err != nil {
		return vm.Job{}, err
	}
	if info.Why != "" {
		return vm.Job{}, fmt.Errorf("%w: %s can't be moved: %s", vm.ErrConflict, name, info.Why)
	}
	var target *MoveTarget
	for i := range info.Targets {
		if info.Targets[i].ID == to {
			target = &info.Targets[i]
		}
	}
	if target == nil {
		return vm.Job{}, fmt.Errorf("%w: no server %q to move to (it may already be there)", vm.ErrInvalid, to)
	}
	if !target.Online {
		return vm.Job{}, fmt.Errorf("%w: server %s is offline", vm.ErrConflict, target.Name)
	}
	self := c.M.SelfID()
	j, err := c.Jobs.Start("move", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return c.move(ctx, name, info.From, to, self, start, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return vm.Job{}, fmt.Errorf("%w: %v", vm.ErrConflict, err)
	}
	return j, err
}

// counter reports progress while the disk goes through.
type counter struct {
	r    io.Reader
	n    atomic.Int64
	tick func(int64)
	last int64
}

func (c *counter) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	n := c.n.Add(int64(k))
	if n-c.last >= 256<<20 {
		c.last = n
		c.tick(n)
	}
	return k, err
}

type moved struct {
	VM   vm.VM  `json:"vm"`
	Note string `json:"note"`
}

func (c *VMs) move(ctx context.Context, name, from, to, self string, start bool, p jobs.Progress) (string, error) {
	lm, _ := c.mover()
	src, _ := c.node(from)
	dst, _ := c.node(to)
	tick := func(n int64) { p(fmt.Sprintf("copying the disk: %.1f GB sent", float64(n)/1e9)) }
	var res moved
	p("copying the disk")
	switch {
	case from == self: // here → worker
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(lm.ExportVM(ctx, name, pw)) }()
		resp, err := c.M.stream(ctx, dst, "POST", "/cluster/vm-import", &counter{r: pr, tick: tick})
		pr.CloseWithError(errors.New("move ended"))
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return "", err
		}
	case to == self: // worker → here
		resp, err := c.M.stream(ctx, src, "GET", "/cluster/vm-export/"+url.PathEscape(name), nil)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		v, note, err := lm.ImportMovedVM(ctx, &counter{r: resp.Body, tick: tick})
		if err != nil {
			return "", err
		}
		res = moved{VM: v, Note: note}
	default: // worker → worker, through this server
		in, err := c.M.stream(ctx, src, "GET", "/cluster/vm-export/"+url.PathEscape(name), nil)
		if err != nil {
			return "", err
		}
		defer in.Body.Close()
		resp, err := c.M.stream(ctx, dst, "POST", "/cluster/vm-import", &counter{r: in.Body, tick: tick})
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return "", err
		}
	}
	// the copy is defined on the target: delete the original
	p("removing the original")
	var derr error
	if from == self {
		derr = c.Local.Delete(context.WithoutCancel(ctx), name)
	} else {
		derr = c.call(context.WithoutCancel(ctx), src, "DELETE", vpath(name), nil, nil)
	}
	if to == self {
		c.remember(name, "")
	} else {
		c.remember(name, to)
	}
	dstName := map[bool]string{true: c.M.SelfName(), false: dst.Name}[to == self]
	msg := fmt.Sprintf("%s moved to %s%s", name, dstName, res.Note)
	if derr != nil {
		msg += fmt.Sprintf(" (the original couldn't be deleted: %v — delete it by hand on the old server)", derr)
	}
	if start {
		p("starting")
		if _, err := c.Start(context.WithoutCancel(ctx), name); err != nil {
			return msg + "; starting it failed: " + err.Error(), nil
		}
		msg += " and started"
	}
	return msg, nil
}
