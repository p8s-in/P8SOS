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
	MirrorPrepare(ctx context.Context, p vm.MirrorPlan, from, host string, catchUp bool) (vm.MirrorTarget, error)
	MirrorSynced(ctx context.Context, name string) error
	MirrorPause(ctx context.Context, name string, max time.Duration) error
	MirrorResume(ctx context.Context, name string) error
	MirrorSnap(ctx context.Context, name, snap string) error
	MirrorUnserve(ctx context.Context, name string) error
}

var _ LocalMirror = (*vm.Service)(nil)

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

// mPost runs a mirror step on a server: here through local, there through
// POST /cluster/mirror/{name}/{what}.
func (h *HA) mPost(ctx context.Context, node, name, what string, in any, local func(LocalMirror) error) error {
	if node == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return err
		}
		return local(lm)
	}
	n, ok := h.C.node(node)
	if !ok {
		return fmt.Errorf("%w: server %s", vm.ErrNotFound, node)
	}
	return h.C.call(ctx, n, "POST", mpath(name, "/"+what), in, nil)
}

func (h *HA) mirrorPlan(ctx context.Context, c HAVM) (vm.MirrorPlan, error) {
	var plan vm.MirrorPlan
	if c.Node == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return plan, err
		}
		return lm.MirrorPlan(ctx, c.Name)
	}
	n, _ := h.C.node(c.Node)
	err := h.C.call(ctx, n, "GET", mpath(c.Name, "/plan"), nil, &plan)
	return plan, err
}

// mirrorPrepare gets the copy's server ready and returns where to mirror to.
func (h *HA) mirrorPrepare(ctx context.Context, c HAVM, plan vm.MirrorPlan, catchUp bool) (vm.MirrorTarget, error) {
	from := h.nodeName(c.Node)
	var t vm.MirrorTarget
	if c.Target == h.self() {
		lm, err := h.mirror()
		if err != nil {
			return t, err
		}
		t, err = lm.MirrorPrepare(ctx, plan, from, h.C.M.SelfWGIP(), catchUp)
		return t, err
	}
	n, _ := h.C.node(c.Target)
	body := map[string]any{"plan": plan, "from": from, "catch_up": catchUp}
	err := h.C.call(ctx, n, "POST", "/cluster/mirror-target", body, &t)
	return t, err
}

func (h *HA) mirrorStart(ctx context.Context, c HAVM, t vm.MirrorTarget) error {
	return h.mPost(ctx, c.Node, c.Name, "start", t, func(lm LocalMirror) error { return lm.MirrorStart(ctx, c.Name, t) })
}

// targetSnaps are the copy snapshots the copy's server has.
func (h *HA) targetSnaps(ctx context.Context, c HAVM) []string {
	if c.Target == h.self() {
		l, _ := h.Local.ReplicaSnaps(ctx, c.Name)
		return l
	}
	n, _ := h.C.node(c.Target)
	var r struct {
		Snaps []string `json:"snaps"`
	}
	_ = h.C.call(ctx, n, "GET", "/cluster/replica/"+url.PathEscape(c.Name), nil, &r)
	return r.Snaps
}

// pauseMax bounds a catch-up's pause: the VM resumes by itself after it.
const pauseMax = 60 * time.Second

func (h *HA) pause(ctx context.Context, c HAVM) error {
	return h.mPost(ctx, c.Node, c.Name, "pause", map[string]int{"max_s": int(pauseMax / time.Second)},
		func(lm LocalMirror) error { return lm.MirrorPause(ctx, c.Name, pauseMax) })
}

func (h *HA) resume(ctx context.Context, c HAVM) error {
	return h.mPost(context.WithoutCancel(ctx), c.Node, c.Name, "resume", nil, func(lm LocalMirror) error {
		return lm.MirrorResume(context.WithoutCancel(ctx), c.Name)
	})
}

// catchUp brings the copy up to date with ZFS (only what changed since the
// snapshot both servers have) instead of a full copy: once while the VM
// runs, then once more for the last changes with the VM paused for a moment,
// and the mirror starts from there with only new writes.
func (h *HA) catchUp(ctx context.Context, c HAVM, plan vm.MirrorPlan) (err error) {
	if err := h.mPost(ctx, c.Target, c.Name, "unserve", nil, func(lm LocalMirror) error { return lm.MirrorUnserve(ctx, c.Name) }); err != nil {
		return err
	}
	if _, err := h.copyOnce(ctx, c); err != nil { // the bulk, while the VM runs
		return fmt.Errorf("catch up: %w", err)
	}
	if err := h.pause(ctx, c); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	defer func() {
		if rerr := h.resume(ctx, c); rerr != nil && err == nil {
			err = fmt.Errorf("resume: %w", rerr)
		}
	}()
	if _, err := h.copyOnce(ctx, c); err != nil { // the last changes, paused
		return fmt.Errorf("catch up (paused): %w", err)
	}
	t, err := h.mirrorPrepare(ctx, c, plan, true)
	if err != nil {
		return fmt.Errorf("%s: %w", h.nodeName(c.Target), err)
	}
	// still paused? (it resumes by itself after pauseMax: then the copy may
	// already be behind, and only a full copy is safe)
	if st, err := h.mirrorStatus(ctx, c.Node, c.Name); err != nil || !st.Paused {
		return fmt.Errorf("the VM didn't stay paused (%v)", err)
	}
	t.Sync = "none" // the copy equals the disk now
	return h.mirrorStart(ctx, c, t)
}

// setBase gives both servers a snapshot of the same instant (the VM paused,
// the mirror in sync): later restarts catch up from it.
func (h *HA) setBase(ctx context.Context, c HAVM) (err error) {
	snap := fmt.Sprintf("p8s-rep-%d", h.now().UnixNano())
	if err := h.pause(ctx, c); err != nil {
		return err
	}
	defer func() {
		if rerr := h.resume(ctx, c); rerr != nil && err == nil {
			err = rerr
		}
	}()
	st, err := h.mirrorStatus(ctx, c.Node, c.Name)
	if err != nil {
		return err
	}
	if !st.InSync() {
		return fmt.Errorf("the mirror isn't in sync")
	}
	for _, node := range []string{c.Node, c.Target} {
		if err := h.mPost(ctx, node, c.Name, "snap", map[string]string{"snap": snap},
			func(lm LocalMirror) error { return lm.MirrorSnap(ctx, c.Name, snap) }); err != nil {
			return fmt.Errorf("%s: %w", h.nodeName(node), err)
		}
	}
	return nil
}

func common(a, b []string) bool {
	have := map[string]bool{}
	for _, x := range b {
		have[x] = true
	}
	for _, x := range a {
		if have[x] {
			return true
		}
	}
	return false
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
		if !c.Based { // a common snapshot, so the next start sends only the changes
			if err := h.setBase(ctx, c); err != nil {
				return "in sync (no starting point for quick restarts yet)", true, nil
			}
			h.setMirror(c.Name, func(x *HAVM) { x.Based = true })
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
	plan, err := h.mirrorPlan(ctx, c)
	if err != nil {
		return "", false, err
	}
	if common(plan.Snaps, h.targetSnaps(ctx, c)) {
		h.setMirror(c.Name, func(x *HAVM) { x.MirrorState = "catching up: only the changes since the last time" })
		if cerr := h.catchUp(ctx, c, plan); cerr == nil {
			h.setMirror(c.Name, func(x *HAVM) { x.Synced, x.Based = true, true })
			return "in sync: every write is on both servers", true, nil
		} else {
			h.event("warning", fmt.Sprintf("%s: quick catch-up failed (%v); copying the whole disk instead", c.Name, cerr), "")
			_ = h.mirrorStop(ctx, c.Node, c.Name)
		}
	}
	t, err := h.mirrorPrepare(ctx, c, plan, false)
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", h.nodeName(c.Target), err)
	}
	if err := h.mirrorStart(ctx, c, t); err != nil {
		return "", false, err
	}
	// a new first sync: the copy is half old, half new until it catches up
	h.setMirror(c.Name, func(x *HAVM) { x.Synced, x.Based = false, false })
	return "first sync started", false, nil
}

func (h *HA) setMirror(name string, f func(*HAVM)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if x := h.st.VMs[name]; x != nil {
		f(x)
		h.saveLocked()
	}
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
