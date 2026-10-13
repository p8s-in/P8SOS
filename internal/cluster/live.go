package cluster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"p8sos/internal/firewall"
	"p8sos/internal/jobs"
	"p8sos/internal/virt"
	"p8sos/internal/vm"
)

// Live moves: a running VM goes to another server and keeps running (see
// vm.LiveHeader). The master runs the steps on both servers; libvirt on the
// source sends memory and disks straight to the target over the tunnel.

// LocalLive is the local VM service's side of a live move.
type LocalLive interface {
	LiveCheck(ctx context.Context, name string) (string, error)
	LiveHeader(ctx context.Context, name string) (vm.LiveHeader, error)
	LivePrepare(ctx context.Context, h vm.LiveHeader) (vm.LivePlan, error)
	LiveSend(ctx context.Context, name, ip string, plan vm.LivePlan, progress func(int)) error
	LiveFinish(ctx context.Context, name string, autostart bool, fw *firewall.Settings, sch *vm.Schedule) (vm.VM, string, error)
	LiveAbort(ctx context.Context, name string) error
	LiveCleanup(ctx context.Context, name string) error
}

// LiveSendRequest asks the source server to send a VM.
type LiveSendRequest struct {
	IP   string      `json:"ip"`
	Plan vm.LivePlan `json:"plan"`
}

// LiveFinishRequest tells the target what the VM had.
type LiveFinishRequest struct {
	Autostart bool               `json:"autostart"`
	Firewall  *firewall.Settings `json:"firewall,omitempty"`
	Schedule  *vm.Schedule       `json:"schedule,omitempty"`
}

func (c *VMs) live() (LocalLive, bool) {
	l, ok := c.Local.(LocalLive)
	return l, ok
}

// liveWhy says whether the VM (on node from) can move while running.
func (c *VMs) liveWhy(ctx context.Context, name, from string) string {
	if from == c.M.SelfID() {
		l, ok := c.live()
		if !ok {
			return "this server can't move running VMs"
		}
		why, err := l.LiveCheck(ctx, name)
		if err != nil {
			return err.Error()
		}
		return why
	}
	n, _ := c.node(from)
	var r struct {
		Why string `json:"why"`
	}
	if err := c.call(ctx, n, "GET", vpath(name, "/live-check"), nil, &r); err != nil {
		return "server " + n.Name + " can't move running VMs (update it): " + err.Error()
	}
	return r.Why
}

func (c *VMs) startLive(ctx context.Context, name string, info MoveInfo, to string) (vm.Job, error) {
	if info.LiveWhy != "" {
		return vm.Job{}, fmt.Errorf("%w: %s can't move while running: %s", vm.ErrConflict, name, info.LiveWhy)
	}
	self := c.M.SelfID()
	j, err := c.Jobs.Start("move", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return c.liveMove(ctx, name, info.From, to, self, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return vm.Job{}, fmt.Errorf("%w: %v", vm.ErrConflict, err)
	}
	return j, err
}

func (c *VMs) tunnelIP(id string) string {
	if id == c.M.SelfID() {
		return c.M.State().Self.WGIP
	}
	n, _ := c.node(id)
	return n.WGIP
}

func (c *VMs) liveMove(ctx context.Context, name, from, to, self string, p jobs.Progress) (string, error) {
	l, _ := c.live()
	src, _ := c.node(from)
	dst, _ := c.node(to)
	bg := context.WithoutCancel(ctx)

	p("describing the VM")
	var h vm.LiveHeader
	var err error
	if from == self {
		h, err = l.LiveHeader(ctx, name)
	} else {
		err = c.call(ctx, src, "GET", "/cluster/live-header/"+url.PathEscape(name), nil, &h)
	}
	if err != nil {
		return "", err
	}

	p("preparing the other server")
	var plan vm.LivePlan
	if to == self {
		plan, err = l.LivePrepare(ctx, h)
	} else {
		err = c.call(ctx, dst, "POST", "/cluster/live-prepare", h, &plan)
	}
	if err != nil {
		return "", err
	}
	abort := func(cause error) (string, error) {
		var aerr error
		if to == self {
			aerr = l.LiveAbort(bg, name)
		} else {
			aerr = c.call(bg, dst, "POST", "/cluster/live-abort/"+url.PathEscape(name), nil, nil)
		}
		if aerr != nil {
			return "", fmt.Errorf("%w (cleaning up the other server also failed: %v)", cause, aerr)
		}
		return "", fmt.Errorf("%w — %s keeps running where it was", cause, name)
	}

	gb := 0.0
	for _, d := range h.Disks {
		gb += float64(d.Bytes) / 1e9
	}
	p(fmt.Sprintf("copying memory and disks (%.0f GB of disks) while it runs", gb))
	prog := func(pct int) { p(fmt.Sprintf("copying while it runs: %d%%", pct)) }
	ip := c.tunnelIP(to)
	if from == self {
		err = l.LiveSend(ctx, name, ip, plan, prog)
	} else {
		err = c.remoteLiveSend(ctx, src, name, LiveSendRequest{IP: ip, Plan: plan}, prog)
	}
	if err != nil {
		return abort(err)
	}

	p("finishing")
	fin := LiveFinishRequest{Autostart: h.Autostart, Firewall: h.Firewall, Schedule: h.Schedule}
	var res moved
	if to == self {
		res.VM, res.Note, err = l.LiveFinish(bg, name, fin.Autostart, fin.Firewall, fin.Schedule)
	} else {
		err = c.call(bg, dst, "POST", "/cluster/live-finish/"+url.PathEscape(name), fin, &res)
	}
	note := res.Note
	if err != nil {
		note += "; settings after the move: " + err.Error()
	}
	if from == self {
		err = l.LiveCleanup(bg, name)
	} else {
		err = c.call(bg, src, "POST", "/cluster/live-cleanup/"+url.PathEscape(name), nil, nil)
	}
	if err != nil {
		note += fmt.Sprintf("; the old disks couldn't be removed (%v) — delete p8s-%s-disk* there by hand", err, name)
	}
	if to == self {
		c.remember(name, "")
	} else {
		c.remember(name, to)
	}
	dstName := map[bool]string{true: c.M.SelfName(), false: dst.Name}[to == self]
	return fmt.Sprintf("%s moved to %s while running%s", name, dstName, note), nil
}

// remoteLiveSend runs the send on a worker and follows its progress lines
// ("progress N", then "done" or "error: …").
func (c *VMs) remoteLiveSend(ctx context.Context, n Node, name string, req LiveSendRequest, prog func(int)) error {
	b, _ := json.Marshal(req)
	resp, err := c.M.stream(ctx, n, "POST", "/cluster/live-send/"+url.PathEscape(name), bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "progress "):
			if v, err := strconv.Atoi(strings.TrimPrefix(line, "progress ")); err == nil {
				prog(v)
			}
		case line == "done":
			return nil
		case strings.HasPrefix(line, "error: "):
			return errors.New(strings.TrimPrefix(line, "error: "))
		}
	}
	return fmt.Errorf("server %s stopped answering during the live move", n.Name)
}

// LocalCPU is the local VM service's CPU-type part.
type LocalCPU interface {
	HostCPU(ctx context.Context) ([]byte, error)
	CPUBaseline(ctx context.Context, caps [][]byte) (virt.CPU, error)
	SetCPU(ctx context.Context, name string, cpu virt.CPU) (vm.VM, error)
}

// SetCPUType sets a VM's CPU: "host" (this server's CPU as it is, fastest) or
// "compatible" (the newest CPU model every server of the cluster has, so it
// can move live between them). It takes effect at the VM's next cold start.
func (c *VMs) SetCPUType(ctx context.Context, name, typ string) (vm.VM, error) {
	lc, ok := c.Local.(LocalCPU)
	if !ok {
		return vm.VM{}, fmt.Errorf("%w: CPU types aren't available here", vm.ErrConflict)
	}
	var cpu virt.CPU
	switch typ {
	case "host":
	case "compatible":
		var err error
		if cpu, err = c.Baseline(ctx); err != nil {
			return vm.VM{}, err
		}
	default:
		return vm.VM{}, fmt.Errorf("%w: type must be host or compatible", vm.ErrInvalid)
	}
	return c.remoteVM(ctx, name, func() (vm.VM, error) { return lc.SetCPU(ctx, name, cpu) },
		"PUT", vpath(name, "/cpu"), cpu)
}

// Baseline is the common CPU of this server and every online worker.
func (c *VMs) Baseline(ctx context.Context) (virt.CPU, error) {
	lc, ok := c.Local.(LocalCPU)
	if !ok {
		return virt.CPU{}, fmt.Errorf("%w: CPU types aren't available here", vm.ErrConflict)
	}
	mine, err := lc.HostCPU(ctx)
	if err != nil {
		return virt.CPU{}, err
	}
	caps := [][]byte{mine}
	for _, w := range c.workers() {
		if c.M != nil && !c.M.NodeStatus(w.ID).Online {
			return virt.CPU{}, fmt.Errorf("%w: server %s is offline; its CPU must be known to pick a common one", vm.ErrConflict, w.Name)
		}
		var r struct {
			XML string `json:"xml"`
		}
		if err := c.call(ctx, w, "GET", "/cluster/cpu", nil, &r); err != nil {
			return virt.CPU{}, fmt.Errorf("CPU of %s: %w", w.Name, err)
		}
		caps = append(caps, []byte(r.XML))
	}
	return lc.CPUBaseline(ctx, caps)
}

// LocalVerify is the local VM service's restore-test part.
type LocalVerify interface {
	StartVerify(ctx context.Context, name, id string) (vm.Job, error)
}

// StartVerify runs a restore test of a backup on the server that keeps it.
func (c *VMs) StartVerify(ctx context.Context, name, id string) (vm.Job, error) {
	n, ok := c.backupNode(name, id)
	if !ok {
		lv, ok := c.Local.(LocalVerify)
		if !ok {
			return vm.Job{}, fmt.Errorf("%w: restore tests aren't available here", vm.ErrConflict)
		}
		return lv.StartVerify(ctx, name, id)
	}
	var j vm.Job
	err := c.call(ctx, n, "POST", "/backups/"+url.PathEscape(name)+"/"+url.PathEscape(id)+"/verify", nil, &j)
	return j, err
}
