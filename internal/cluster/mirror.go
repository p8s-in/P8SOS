package cluster

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"p8sos/internal/vm"
)

// P8S Mirror in high availability (HAVM.Mode "mirror"): instead of a copy
// every few minutes, the VM's disks are mirrored live to the second server
// (vm/mirror.go): nothing is lost when the VM's server dies. The master keeps
// the mirror going: it starts it when the VM runs (the first sync copies
// everything), tells the copy's server when it caught up, and starts it again
// when it broke. Failover, coming back and fencing are those of HA.

// ModeMirror is a VM protected by a live mirror.
const ModeMirror = "mirror"

// LocalMirror is the local VM service's mirror side (vm.Service).
type LocalMirror interface {
	MirrorCheck(ctx context.Context, name string) (string, error)
	MirrorPlan(ctx context.Context, name string) (vm.MirrorPlan, error)
	MirrorStatus(ctx context.Context, name string) (vm.MirrorState, error)
	MirrorStart(ctx context.Context, name string, t vm.MirrorTarget) error
	MirrorStop(ctx context.Context, name string) error
	MirrorPrepare(ctx context.Context, p vm.MirrorPlan, from, host string) (vm.MirrorTarget, error)
	MirrorSynced(ctx context.Context, name string) error
}

func (h *HA) mirror() (LocalMirror, error) {
	lm, ok := h.Local.(LocalMirror)
	if !ok {
		return nil, fmt.Errorf("%w: this server can't mirror VMs", vm.ErrConflict)
	}
	return lm, nil
}

func mpath(name string, rest string) string { return "/cluster/mirror/" + url.PathEscape(name) + rest }

// mirrorCheck asks the VM's server whether it can be mirrored ("" = yes).
func (h *HA) mirrorCheck(ctx context.Context, node, name string) (string, error) {
	if node == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return "", err
		}
		return lm.MirrorCheck(ctx, name)
	}
	n, _ := h.C.node(node)
	var r struct {
		Why string `json:"why"`
	}
	err := h.C.call(ctx, n, "GET", mpath(name, "/check"), nil, &r)
	return r.Why, err
}

func (h *HA) mirrorStatus(ctx context.Context, node, name string) (vm.MirrorState, error) {
	if node == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return vm.MirrorState{}, err
		}
		return lm.MirrorStatus(ctx, name)
	}
	n, _ := h.C.node(node)
	var st vm.MirrorState
	err := h.C.call(ctx, n, "GET", mpath(name, ""), nil, &st)
	return st, err
}

func (h *HA) mirrorStop(ctx context.Context, node, name string) error {
	if node == "" {
		return nil
	}
	if node == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return err
		}
		return lm.MirrorStop(ctx, name)
	}
	n, ok := h.C.node(node)
	if !ok {
		return nil
	}
	return h.C.call(ctx, n, "POST", mpath(name, "/stop"), nil, nil)
}

func (h *HA) tunnelIP(id string) string {
	if id == h.self() {
		return h.C.M.SelfWGIP()
	}
	n, _ := h.C.node(id)
	return n.WGIP
}

// mirrorOnce keeps one VM's mirror going. It returns how it stands and
// whether every write goes to both servers now.
func (h *HA) mirrorOnce(ctx context.Context, c HAVM) (state string, inSync bool, err error) {
	st, err := h.mirrorStatus(ctx, c.Node, c.Name)
	if err != nil {
		return "", false, err
	}
	if !st.Running {
		return "the VM is off: the mirror waits (it starts with the VM)", false, nil
	}
	if st.Moving {
		return "the VM is moving to another server", false, nil
	}
	if st.InSync() {
		if !c.Synced { // the copy's server can drop its "last good" snapshot
			if err := h.mirrorSynced(ctx, c.Target, c.Name); err != nil {
				return "in sync (the other server wasn't told yet)", true, err
			}
		}
		return "in sync: every write is on both servers", true, nil
	}
	missing := false
	for _, d := range st.Disks {
		missing = missing || !d.Job
	}
	if !missing {
		return fmt.Sprintf("first sync %d%%", st.Progress()), false, nil
	}
	// (re)start: everything stops, the copy's server gets ready, the mirror starts
	_ = h.mirrorStop(ctx, c.Node, c.Name)
	var plan vm.MirrorPlan
	if c.Node == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return "", false, err
		}
		if plan, err = lm.MirrorPlan(ctx, c.Name); err != nil {
			return "", false, err
		}
	} else {
		n, _ := h.C.node(c.Node)
		if err := h.C.call(ctx, n, "GET", mpath(c.Name, "/plan"), nil, &plan); err != nil {
			return "", false, err
		}
	}
	from := h.nodeName(c.Node)
	var t vm.MirrorTarget
	if c.Target == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return "", false, err
		}
		if t, err = lm.MirrorPrepare(ctx, plan, from, h.C.M.SelfWGIP()); err != nil {
			return "", false, fmt.Errorf("%s: %w", h.nodeName(c.Target), err)
		}
	} else {
		n, _ := h.C.node(c.Target)
		body := map[string]any{"plan": plan, "from": from}
		if err := h.C.call(ctx, n, "POST", "/cluster/mirror-target", body, &t); err != nil {
			return "", false, fmt.Errorf("%s: %w", h.nodeName(c.Target), err)
		}
	}
	if c.Node == h.self() {
		lm, _ := h.mirror()
		err = lm.MirrorStart(ctx, c.Name, t)
	} else {
		n, _ := h.C.node(c.Node)
		err = h.C.call(ctx, n, "POST", mpath(c.Name, "/start"), t, nil)
	}
	if err != nil {
		return "", false, err
	}
	h.mu.Lock()
	if x := h.st.VMs[c.Name]; x != nil {
		x.Synced = false // a new first sync: the copy is half old, half new until it catches up
	}
	h.mu.Unlock()
	return "first sync started", false, nil
}

func (h *HA) mirrorSynced(ctx context.Context, node, name string) error {
	if node == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return err
		}
		return lm.MirrorSynced(ctx, name)
	}
	n, _ := h.C.node(node)
	return h.C.call(ctx, n, "POST", mpath(name, "/synced"), nil, nil)
}

// mirrorRound is replicate() for a mirrored VM.
func (h *HA) mirrorRound(ctx context.Context, cp HAVM) {
	state, inSync, err := h.mirrorOnce(ctx, cp)
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.st.VMs[cp.Name]
	if c == nil || c.Node != cp.Node || c.Target != cp.Target {
		return
	}
	if err != nil {
		if c.Error == "" {
			h.eventLocked("warning", fmt.Sprintf("mirror of %s to %s: %v", cp.Name, h.nodeName(cp.Target), err))
		}
		c.Error = err.Error()
		if c.MirrorState == "in sync: every write is on both servers" {
			c.MirrorState = "not mirrored right now"
		}
	} else {
		if c.Error != "" {
			h.eventLocked("info", "mirror of "+cp.Name+" works again")
		}
		was := c.MirrorState
		c.Error, c.MirrorState = "", state
		if inSync {
			c.LastCopy, c.Synced = h.now().UTC().Format(time.RFC3339), true
			if was != state {
				h.eventLocked("info", cp.Name+" is mirrored: every write is on "+h.nodeName(cp.Node)+" and "+h.nodeName(cp.Target))
			}
		} else if was == "in sync: every write is on both servers" && state != was && cp.LastState == "running" && !stoppedState(state) {
			h.eventLocked("warning", cp.Name+": the mirror broke; it starts again (a full sync)")
		}
	}
	h.saveLocked()
}

func stoppedState(s string) bool { return len(s) > 10 && s[:10] == "the VM is " }
