package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/vm"
)

// High availability (HA). The master keeps a copy of every protected VM on a
// second server (ZFS replication every few minutes, also while the VM runs).
// If the VM's server dies, the master starts the copy on the second server.
//
// A VM must never run twice. So:
//   - a worker that hasn't heard from the master for FenceAfter seconds AND
//     can't reach its own network (gateway) switches its protected VMs off;
//   - the master starts a copy only when the worker has been unreachable for
//     FailoverAfter seconds, its tunnel is silent too (the host is down, not
//     only p8sd), the master itself still reaches its network, and the
//     worker is on the master's local network. Workers on other networks
//     (internet) get copies but are failed over by hand ("Start the copy").
//   - when the dead server comes back, its old VM is switched off and kept as
//     the copy; copies then go the other way.
// The master's own VMs can be protected too. With 3+ servers, if the master
// dies the others elect a new master (quorum.go), which starts their copies;
// with 2, start them by hand in the other server's dashboard.

// LocalHA is the local VM service's side (vm.Service).
type LocalHA interface {
	HACheck(ctx context.Context, name string) (string, error)
	ReplicaSnaps(ctx context.Context, name string) ([]string, error)
	SendReplica(ctx context.Context, name string, have []string, from string, w io.Writer) error
	RecvReplica(ctx context.Context, r io.Reader) (vm.Replica, error)
	Replicas(ctx context.Context) []vm.Replica
	PromoteReplica(ctx context.Context, name string) (vm.VM, string, error)
	DropReplica(ctx context.Context, name string) error
	RetireVM(ctx context.Context, name, from string) error
	Fence(ctx context.Context, names []string) []string
	SetAutostart(ctx context.Context, name string, on bool) (vm.VM, error)
}

// HASettings are the timings (seconds).
type HASettings struct {
	FenceAfter    int `json:"fence_after_s"`    // worker without master + network → protected VMs off
	FailoverAfter int `json:"failover_after_s"` // master starts copies of an unreachable server's VMs
}

func (s HASettings) defaults() HASettings {
	if s.FenceAfter < 20 {
		s.FenceAfter = 60
	}
	if s.FailoverAfter < s.FenceAfter+60 {
		s.FailoverAfter = max(180, s.FenceAfter+60)
	}
	return s
}

// HAVM is one protected VM.
type HAVM struct {
	Name      string   `json:"name"`
	Target    string   `json:"target"`    // server (node id) that keeps the copy
	EveryMin  int      `json:"every_min"` // minutes between copies
	Node      string   `json:"node"`      // server it runs on (last seen)
	LastState string   `json:"last_state,omitempty"`
	LastCopy  string   `json:"last_copy,omitempty"` // when the newest copy was taken
	CopyBytes int64    `json:"copy_bytes,omitempty"`
	LastTry   string   `json:"last_try,omitempty"`
	Error     string   `json:"error,omitempty"`
	FailedAt  string   `json:"failed_over_at,omitempty"`
	FailedOf  string   `json:"failed_over_from,omitempty"` // server name
	Retire    []string `json:"retire,omitempty"`           // servers with an old copy of the VM to switch off
	Autostart bool     `json:"autostart,omitempty"`        // its autostart before HA (set back when HA is turned off)
}

// HAVMView is a protected VM as the dashboard shows it.
type HAVMView struct {
	HAVM
	NodeName   string `json:"node_name"`
	TargetName string `json:"target_name"`
	Auto       bool   `json:"auto"`              // automatic failover possible
	Why        string `json:"why,omitempty"`     // why not automatic
	Copying    bool   `json:"copying,omitempty"` // a copy is being made now
	NodeOnline bool   `json:"node_online"`
}

// HAEvent is one line of the HA log.
type HAEvent struct {
	Time     string `json:"time"`
	Severity string `json:"severity"` // info | warning | critical
	Text     string `json:"text"`
}

// HAStatus is the Server → High availability page.
type HAStatus struct {
	Role     string       `json:"role"`
	Settings HASettings   `json:"settings"`
	VMs      []HAVMView   `json:"vms"`
	Replicas []vm.Replica `json:"replicas"` // copies held on this server
	Events   []HAEvent    `json:"events"`
	Lease    string       `json:"lease,omitempty"` // worker: last word from the master
	Fenced   []string     `json:"fenced,omitempty"`
}

type haState struct {
	Settings HASettings       `json:"settings"`
	VMs      map[string]*HAVM `json:"vms"`
	Events   []HAEvent        `json:"events"`
	Boots    map[string]int64 `json:"boots,omitempty"` // server → host boot time last seen (a reboot = came back)
}

// HA runs high availability (on the master) and fencing (on workers).
type HA struct {
	C     *VMs
	Local LocalHA
	Dir   string // data_dir/cluster
	Log   *slog.Logger
	Now   func() time.Time
	// Notify sends an alert (Health & alerts channels); nil = only the HA log.
	Notify func(severity, title, detail string)
	// GatewayOK says whether this server reaches its own network; nil = ping the default gateway.
	GatewayOK func(ctx context.Context) bool
	// SameLAN says whether a worker is on this server's local network; nil = from its tunnel address.
	SameLAN func(ctx context.Context, n Node) bool
	// BootTime is when this host booted (unix seconds); nil = /proc/stat.
	BootTime func() int64

	mu      sync.Mutex
	st      *haState
	offline map[string]time.Time // worker → offline since
	back    map[string]bool      // worker (or self) came back: restart what was running
	told    map[string]bool      // "needs a manual start" sent for this outage
	busy    map[string]bool      // copy running
	booted  bool
	// master without a majority: its protected VMs were switched off
	quorumFenced bool
	// worker side
	lease    time.Time
	leaseVMs []string
	fenceSec int
	fenced   []string
	fencedAt time.Time
}

func (h *HA) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *HA) file() string { return filepath.Join(h.Dir, "ha.json") }

func (h *HA) loadLocked() {
	if h.st != nil {
		return
	}
	st := &haState{}
	if b, err := os.ReadFile(h.file()); err == nil {
		_ = json.Unmarshal(b, st)
	}
	if st.VMs == nil {
		st.VMs = map[string]*HAVM{}
	}
	if st.Boots == nil {
		st.Boots = map[string]int64{}
	}
	st.Settings = st.Settings.defaults()
	h.st = st
	h.offline, h.back, h.told, h.busy = map[string]time.Time{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
}

func (h *HA) saveLocked() {
	b, _ := json.MarshalIndent(h.st, "", "  ")
	_ = os.MkdirAll(h.Dir, 0o700)
	if os.WriteFile(h.file()+".tmp", b, 0o600) == nil {
		_ = os.Rename(h.file()+".tmp", h.file())
	}
}

func (h *HA) eventLocked(sev, text string) {
	h.st.Events = append(h.st.Events, HAEvent{Time: h.now().UTC().Format(time.RFC3339), Severity: sev, Text: text})
	if len(h.st.Events) > 200 {
		h.st.Events = h.st.Events[len(h.st.Events)-200:]
	}
	if h.Log != nil {
		h.Log.Info("ha", "severity", sev, "event", text)
	}
}

func (h *HA) event(sev, text, detail string) {
	h.mu.Lock()
	h.loadLocked()
	h.eventLocked(sev, text)
	h.saveLocked()
	h.mu.Unlock()
	if h.Notify != nil && sev != "info" {
		h.Notify(sev, text, detail)
	}
}

func (h *HA) self() string { return h.C.M.SelfID() }

func (h *HA) nodeName(id string) string {
	if id == h.self() {
		return h.C.M.SelfName()
	}
	if n, ok := h.C.node(id); ok {
		return n.Name
	}
	return id
}

func (h *HA) online(id string) bool {
	return id == h.self() || h.C.M.NodeStatus(id).Online
}

func (h *HA) known(id string) bool {
	_, ok := h.C.node(id)
	return id == h.self() || ok
}

// --- settings from the dashboard ---

// Protect turns HA on for a VM (target "" = pick the best other server).
func (h *HA) Protect(ctx context.Context, name, target string, everyMin int) (HAVMView, error) {
	if h.C.M.Role() != "master" {
		return HAVMView{}, fmt.Errorf("%w: high availability needs a cluster (Server → Cluster → Add a server)", vm.ErrConflict)
	}
	v, err := h.C.Get(ctx, name)
	if err != nil {
		return HAVMView{}, err
	}
	node := v.NodeID
	if node == "" {
		node = h.self()
	}
	why, err := h.haCheck(ctx, node, name)
	if err != nil {
		return HAVMView{}, err
	}
	if why != "" {
		return HAVMView{}, fmt.Errorf("%w: %s can't be protected: %s", vm.ErrConflict, name, why)
	}
	if everyMin == 0 {
		everyMin = 15
	}
	if everyMin < 1 || everyMin > 24*60 {
		return HAVMView{}, fmt.Errorf("%w: copy every 1 to 1440 minutes", vm.ErrInvalid)
	}
	if target == "" {
		target = h.pickTarget(ctx, node)
		if target == "" {
			return HAVMView{}, fmt.Errorf("%w: no other server is online to keep the copy", vm.ErrConflict)
		}
	}
	if target == node {
		return HAVMView{}, fmt.Errorf("%w: the copy must be on another server than the VM", vm.ErrInvalid)
	}
	if !h.known(target) {
		return HAVMView{}, fmt.Errorf("%w: no server %q", vm.ErrInvalid, target)
	}
	// HA starts the VM after outages; the host's own autostart would start it
	// even where it must not run (a server that was cut off).
	if err := h.setAutostart(ctx, node, name, false); err != nil {
		return HAVMView{}, err
	}
	h.mu.Lock()
	h.loadLocked()
	old := h.st.VMs[name]
	c := &HAVM{Name: name, Target: target, EveryMin: everyMin, Node: node, LastState: string(v.State), Autostart: v.Autostart}
	if old != nil {
		c.LastCopy, c.CopyBytes, c.Retire, c.Autostart = old.LastCopy, old.CopyBytes, old.Retire, old.Autostart
		if old.Target != target {
			c.LastCopy, c.CopyBytes = "", 0
			go h.dropCopy(context.WithoutCancel(ctx), old.Target, name)
		}
	}
	h.st.VMs[name] = c
	h.eventLocked("info", fmt.Sprintf("%s protected: copies to %s every %d min", name, h.nodeName(target), everyMin))
	h.saveLocked()
	h.mu.Unlock()
	go h.replicate(context.WithoutCancel(ctx), name) // first copy now
	return h.view(ctx, c), nil
}

// Unprotect turns HA off (the copy is deleted; the VM keeps running).
func (h *HA) Unprotect(ctx context.Context, name string) error {
	h.mu.Lock()
	h.loadLocked()
	c := h.st.VMs[name]
	if c == nil {
		h.mu.Unlock()
		return fmt.Errorf("%w: %s isn't protected", vm.ErrNotFound, name)
	}
	delete(h.st.VMs, name)
	h.eventLocked("info", name+" no longer protected")
	h.saveLocked()
	h.mu.Unlock()
	if err := h.dropCopy(ctx, c.Target, name); err != nil {
		return fmt.Errorf("turned off, but the copy on %s couldn't be deleted: %w", h.nodeName(c.Target), err)
	}
	if c.Autostart {
		_ = h.setAutostart(ctx, c.Node, name, true)
	}
	return nil
}

// SetSettings changes the timings.
func (h *HA) SetSettings(s HASettings) HASettings {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked()
	h.st.Settings = s.defaults()
	h.saveLocked()
	return h.st.Settings
}

// Config returns a VM's HA settings (ok=false: not protected).
func (h *HA) Config(ctx context.Context, name string) (HAVMView, bool) {
	h.mu.Lock()
	h.loadLocked()
	c := h.st.VMs[name]
	var cp HAVM
	if c != nil {
		cp = *c
	}
	h.mu.Unlock()
	if c == nil {
		return HAVMView{}, false
	}
	return h.view(ctx, &cp), true
}

func (h *HA) view(ctx context.Context, c *HAVM) HAVMView {
	v := HAVMView{HAVM: *c, NodeName: h.nodeName(c.Node), TargetName: h.nodeName(c.Target), NodeOnline: h.online(c.Node)}
	h.mu.Lock()
	v.Copying = h.busy[c.Name]
	h.mu.Unlock()
	switch {
	case c.Node == h.self():
		v.Why = "it runs on the master; if the master dies, the other servers elect a new master which starts the copy (with 2 servers: start it by hand on " + v.TargetName + ")"
	case c.Target != h.self() && !h.online(c.Target):
		v.Why = "the server with the copy is offline"
	default:
		n, _ := h.C.node(c.Node)
		if !h.sameLAN(ctx, n) {
			v.Why = "its server is on another network; if it goes offline, start the copy by hand"
		} else {
			v.Auto = true
		}
	}
	return v
}

// Status is everything for the Server → High availability page.
func (h *HA) Status(ctx context.Context) HAStatus {
	h.mu.Lock()
	h.loadLocked()
	st := HAStatus{Role: h.C.M.Role(), Settings: h.st.Settings, VMs: []HAVMView{}, Events: append([]HAEvent{}, h.st.Events...)}
	var cfgs []HAVM
	for _, c := range h.st.VMs {
		cfgs = append(cfgs, *c)
	}
	if !h.lease.IsZero() {
		st.Lease = h.lease.UTC().Format(time.RFC3339)
	}
	st.Fenced = append([]string{}, h.fenced...)
	h.mu.Unlock()
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Name < cfgs[j].Name })
	if st.Role == "master" {
		for i := range cfgs {
			st.VMs = append(st.VMs, h.view(ctx, &cfgs[i]))
		}
	}
	st.Replicas = []vm.Replica{}
	if h.Local != nil {
		st.Replicas = h.Local.Replicas(ctx)
	}
	for i, j := 0, len(st.Events)-1; i < j; i, j = i+1, j-1 {
		st.Events[i], st.Events[j] = st.Events[j], st.Events[i]
	}
	return st
}

// --- the loop ---

// Loop runs HA every few seconds.
func (h *HA) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.Tick(ctx)
	}
}

// Tick is one round: on the master watch, copy and fail over; on a worker fence.
func (h *HA) Tick(ctx context.Context) {
	switch h.C.M.Role() {
	case "master":
		h.masterTick(ctx)
	case "worker":
		h.workerTick(ctx)
	}
}

func (h *HA) masterTick(ctx context.Context) {
	if !h.C.M.HasQuorum() {
		h.noQuorum(ctx)
		return
	}
	h.mu.Lock()
	h.quorumFenced = false
	h.mu.Unlock()
	now := h.now()
	h.mu.Lock()
	h.loadLocked()
	set := h.st.Settings
	if !h.booted { // after a reboot of this server: like coming back after an outage
		h.booted = true
		h.sawBoot(h.self(), h.bootTime())
	}
	for _, w := range h.C.workers() {
		if h.C.M.NodeStatus(w.ID).Online {
			if _, was := h.offline[w.ID]; was {
				delete(h.offline, w.ID)
				delete(h.told, w.ID)
				h.back[w.ID] = true
			}
		} else if _, ok := h.offline[w.ID]; !ok {
			h.offline[w.ID] = now
		}
	}
	n := len(h.st.VMs)
	h.mu.Unlock()
	if n == 0 {
		h.mu.Lock()
		h.back = map[string]bool{}
		h.mu.Unlock()
		return
	}

	h.retireOld(ctx)
	h.syncWorkers(ctx, set)
	all, _ := h.C.List(ctx)
	where := map[string][]vm.VM{}
	for _, v := range all {
		where[v.Name] = append(where[v.Name], v)
	}
	allOnline := len(h.offline) == 0

	h.mu.Lock()
	type todo struct {
		name, node string
		why        string
	}
	var starts []todo
	var drops []struct{ node, name string }
	for name, c := range h.st.VMs {
		vs := where[name]
		if len(vs) == 0 {
			if allOnline && len(c.Retire) == 0 {
				delete(h.st.VMs, name)
				drops = append(drops, struct{ node, name string }{c.Target, name})
				h.eventLocked("info", name+" was deleted; its copy is deleted too")
			}
			continue
		}
		idOf := func(x vm.VM) string {
			if x.NodeID == "" {
				return h.self()
			}
			return x.NodeID
		}
		v := vs[0]
		for _, x := range vs {
			if idOf(x) == c.Node {
				v = x
			}
		}
		// started from its copy by hand on another server (e.g. while this
		// master was down): that one is the VM now, the old one becomes the copy
		for _, x := range vs {
			if idOf(x) != c.Node && x.State == "running" && v.State != "running" {
				old := c.Node
				c.Node, c.Target = idOf(x), old
				c.Retire = appendOnce(c.Retire, old)
				c.LastCopy, c.CopyBytes = "", 0
				h.eventLocked("warning", fmt.Sprintf("%s runs on %s (its copy was started there); the old one on %s is switched off", name, h.nodeName(c.Node), h.nodeName(old)))
				v = x
				break
			}
		}
		node := idOf(v)
		if v.State == "unknown" {
			continue
		}
		if node != c.Node { // moved by hand
			c.Node = node
			if c.Target == node {
				drops = append(drops, struct{ node, name string }{node, name})
				c.Target, c.LastCopy, c.CopyBytes = "", "", 0
			}
		}
		if c.Target == "" || !h.known(c.Target) {
			c.Target = h.pickTargetLocked(node)
			c.LastCopy, c.CopyBytes = "", 0
		}
		switch {
		case h.back[node] && v.State == "stopped" && c.LastState == "running":
			starts = append(starts, todo{name, node, "restarted after its server came back"})
		case v.State == "crashed":
			starts = append(starts, todo{name, node, "restarted after it crashed"})
		default:
			c.LastState = string(v.State)
		}
	}
	h.back = map[string]bool{}
	h.saveLocked()
	h.mu.Unlock()
	for _, d := range drops {
		_ = h.dropCopy(ctx, d.node, d.name)
	}
	for _, s := range starts {
		h.start(ctx, s.name, s.why)
	}

	h.failovers(ctx, set)
	h.replicateDue(ctx)
}

func (h *HA) start(ctx context.Context, name, why string) {
	if _, err := h.C.Start(ctx, name); err != nil {
		h.event("warning", fmt.Sprintf("%s couldn't be started (%s): %v", name, why, err), "")
		return
	}
	h.mu.Lock()
	if c := h.st.VMs[name]; c != nil {
		c.LastState = "running"
	}
	h.mu.Unlock()
	h.event("warning", name+" "+why, "High availability started the VM again.")
}

// syncWorkers renews every online worker's lease and learns what it fenced.
func (h *HA) syncWorkers(ctx context.Context, set HASettings) {
	h.mu.Lock()
	per := map[string][]string{}
	for name, c := range h.st.VMs {
		per[c.Node] = append(per[c.Node], name)
		for _, r := range c.Retire { // old copies still running there must be fenced too
			per[r] = append(per[r], name)
		}
	}
	h.mu.Unlock()
	for _, w := range h.C.workers() {
		if !h.C.M.NodeStatus(w.ID).Online {
			continue
		}
		names := per[w.ID]
		sort.Strings(names)
		q := url.Values{"vms": {strings.Join(names, ",")}, "fence": {fmt.Sprint(set.FenceAfter)}}
		var r struct {
			Fenced []string `json:"fenced"`
			Boot   int64    `json:"boot"`
		}
		if err := h.C.call(ctx, w, "GET", "/cluster/ha-sync?"+q.Encode(), nil, &r); err != nil {
			continue
		}
		h.mu.Lock()
		h.sawBoot(w.ID, r.Boot)
		h.mu.Unlock()
		for _, name := range r.Fenced {
			h.mu.Lock()
			c := h.st.VMs[name]
			ok := c != nil && c.Node == w.ID
			h.mu.Unlock()
			if ok {
				h.start(ctx, name, "restarted: "+w.Name+" had switched it off while it was cut off from the cluster")
			} else {
				h.event("info", fmt.Sprintf("%s switched off its old copy of %s (it runs elsewhere now)", w.Name, name), "")
			}
		}
	}
}

// sawBoot notes a server's boot time; a new one means it rebooted (caller holds mu).
func (h *HA) sawBoot(id string, boot int64) {
	if boot == 0 {
		return
	}
	if old := h.st.Boots[id]; old != 0 && old != boot {
		h.back[id] = true
	}
	if h.st.Boots[id] != boot {
		h.st.Boots[id] = boot
		h.saveLocked()
	}
}

func (h *HA) bootTime() int64 {
	if h.BootTime != nil {
		return h.BootTime()
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			var n int64
			fmt.Sscan(v, &n)
			return n
		}
	}
	return 0
}

// failovers starts the copies of VMs whose server is gone.
func (h *HA) failovers(ctx context.Context, set HASettings) {
	now := h.now()
	h.mu.Lock()
	type cand struct {
		node Node
		vms  []string
	}
	var due []cand
	for id, since := range h.offline {
		if now.Sub(since) < time.Duration(set.FailoverAfter)*time.Second {
			continue
		}
		n, ok := h.C.node(id)
		if !ok {
			continue
		}
		var names []string
		for name, c := range h.st.VMs {
			if c.Node == id && !h.busy[name] {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			due = append(due, cand{n, names})
		}
	}
	h.mu.Unlock()
	for _, d := range due {
		st := h.C.M.NodeStatus(d.node.ID)
		if t, err := time.Parse(time.RFC3339, st.Tunnel); err == nil && now.Sub(t) < time.Duration(set.FailoverAfter)*time.Second {
			h.tellOnce(d.node.ID, "warning", d.node.Name+" doesn't answer, but its tunnel is up (P8S OS on it may be stuck); its VMs are not started elsewhere",
				"Check the server. If it is really off, use “Start the copy” on each VM.")
			continue
		}
		if !h.sameLAN(ctx, d.node) {
			h.tellOnce(d.node.ID, "critical", d.node.Name+" is offline; its protected VMs need a manual start ("+strings.Join(d.vms, ", ")+")",
				"The server is on another network, so P8S OS can't be sure it is really off. When you are sure, open each VM → Options → High availability → Start the copy.")
			continue
		}
		if !h.gatewayOK(ctx) {
			h.tellOnce(d.node.ID, "warning", "this server can't reach its own network; no failover for "+d.node.Name, "")
			continue
		}
		for _, name := range d.vms {
			if err := h.failover(ctx, name, true); err != nil {
				h.event("critical", fmt.Sprintf("failover of %s failed: %v", name, err), "")
			}
		}
	}
}

func (h *HA) tellOnce(id, sev, text, detail string) {
	h.mu.Lock()
	told := h.told[id]
	h.told[id] = true
	h.mu.Unlock()
	if !told {
		h.event(sev, text, detail)
	}
}

// Failover starts the copy of a VM whose server is offline (by hand).
func (h *HA) Failover(ctx context.Context, name string) (string, error) {
	h.mu.Lock()
	h.loadLocked()
	c := h.st.VMs[name]
	h.mu.Unlock()
	if c == nil {
		return "", fmt.Errorf("%w: %s isn't protected", vm.ErrNotFound, name)
	}
	if h.online(c.Node) {
		return "", fmt.Errorf("%w: %s is online; shut the VM down and use Move instead", vm.ErrConflict, h.nodeName(c.Node))
	}
	if err := h.failover(ctx, name, false); err != nil {
		return "", err
	}
	h.mu.Lock()
	c = h.st.VMs[name]
	msg := name + " started on " + h.nodeName(c.Node)
	h.mu.Unlock()
	return msg, nil
}

func (h *HA) failover(ctx context.Context, name string, auto bool) error {
	h.mu.Lock()
	c := h.st.VMs[name]
	if c == nil || h.busy[name] {
		h.mu.Unlock()
		return errors.New("busy")
	}
	h.busy[name] = true
	cp := *c
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.busy, name); h.mu.Unlock() }()
	if cp.LastCopy == "" {
		return fmt.Errorf("%s has no copy yet", name)
	}
	if !h.online(cp.Target) {
		return fmt.Errorf("the server with the copy (%s) is offline too", h.nodeName(cp.Target))
	}
	var res moved
	if cp.Target == h.self() {
		v, note, err := h.Local.PromoteReplica(ctx, name)
		if err != nil {
			return err
		}
		res = moved{VM: v, Note: note}
		h.C.remember(name, "")
	} else {
		n, _ := h.C.node(cp.Target)
		if err := h.C.call(ctx, n, "POST", "/cluster/replica/"+url.PathEscape(name)+"/promote", nil, &res); err != nil {
			return err
		}
		h.C.remember(name, n.ID)
	}
	h.mu.Lock()
	if c = h.st.VMs[name]; c != nil {
		old := c.Node
		c.FailedAt, c.FailedOf = h.now().UTC().Format(time.RFC3339), h.nodeName(old)
		c.Node, c.Target = cp.Target, old // copies go back once the old server returns
		c.Retire = appendOnce(c.Retire, old)
		c.LastCopy, c.CopyBytes = "", 0
	}
	h.saveLocked()
	h.mu.Unlock()
	how := map[bool]string{true: "automatically", false: "by hand"}[auto]
	msg := fmt.Sprintf("%s started on %s %s (%s is offline)", name, h.nodeName(cp.Target), how, h.nodeName(cp.Node))
	detail := fmt.Sprintf("The VM runs from its copy of %s; changes after that were on %s and are lost unless it comes back.", cp.LastCopy, h.nodeName(cp.Node))
	if cp.LastState == "running" {
		if _, err := h.C.Start(ctx, name); err != nil {
			msg += "; starting it failed: " + err.Error()
		}
	} else {
		msg = strings.Replace(msg, " started on ", " moved to ", 1) + " (it was shut down, so it stays off)"
	}
	h.event("critical", msg+res.Note, detail)
	return nil
}

func appendOnce(l []string, s string) []string {
	for _, x := range l {
		if x == s {
			return l
		}
	}
	return append(l, s)
}

// retireOld switches off the old VM on servers that came back after a failover.
func (h *HA) retireOld(ctx context.Context) {
	h.mu.Lock()
	type job struct{ name, node, from string }
	var jobs []job
	for name, c := range h.st.VMs {
		for _, r := range c.Retire {
			if h.online(r) {
				jobs = append(jobs, job{name, r, h.nodeName(c.Node)})
			}
		}
	}
	h.mu.Unlock()
	for _, j := range jobs {
		var err error
		if j.node == h.self() {
			err = h.Local.RetireVM(ctx, j.name, j.from)
		} else {
			n, _ := h.C.node(j.node)
			err = h.C.call(ctx, n, "POST", "/cluster/replica/"+url.PathEscape(j.name)+"/retire?from="+url.QueryEscape(j.from), nil, nil)
		}
		if err != nil {
			h.event("warning", fmt.Sprintf("the old %s on %s couldn't be switched off: %v", j.name, h.nodeName(j.node), err), "")
			continue
		}
		h.mu.Lock()
		if c := h.st.VMs[j.name]; c != nil {
			var keep []string
			for _, r := range c.Retire {
				if r != j.node {
					keep = append(keep, r)
				}
			}
			c.Retire = keep
		}
		h.saveLocked()
		h.mu.Unlock()
		h.event("info", fmt.Sprintf("%s is back: its old %s is switched off and becomes the copy", h.nodeName(j.node), j.name), "")
	}
}

// --- copies ---

func (h *HA) replicateDue(ctx context.Context) {
	now := h.now()
	h.mu.Lock()
	var due []string
	running := len(h.busy)
	for name, c := range h.st.VMs {
		if h.busy[name] || len(c.Retire) > 0 && contains(c.Retire, c.Target) {
			continue
		}
		last, _ := time.Parse(time.RFC3339, c.LastTry)
		if now.Sub(last) >= time.Duration(c.EveryMin)*time.Minute && h.online(c.Node) && h.online(c.Target) {
			due = append(due, name)
		}
	}
	h.mu.Unlock()
	sort.Strings(due)
	for _, name := range due {
		if running >= 2 {
			break
		}
		running++
		go h.replicate(context.WithoutCancel(ctx), name)
	}
}

// replicate makes one copy of a VM (its server → the copy's server).
func (h *HA) replicate(ctx context.Context, name string) {
	h.mu.Lock()
	h.loadLocked()
	c := h.st.VMs[name]
	if c == nil || h.busy[name] {
		h.mu.Unlock()
		return
	}
	h.busy[name] = true
	c.LastTry = h.now().UTC().Format(time.RFC3339)
	cp := *c
	h.mu.Unlock()
	rep, err := h.copyOnce(ctx, cp)
	h.mu.Lock()
	delete(h.busy, name)
	if c := h.st.VMs[name]; c != nil && c.Node == cp.Node && c.Target == cp.Target {
		if err != nil {
			if c.Error == "" {
				h.eventLocked("warning", fmt.Sprintf("copy of %s to %s failed: %v", name, h.nodeName(cp.Target), err))
			}
			c.Error = err.Error()
		} else {
			if c.Error != "" {
				h.eventLocked("info", "copies of "+name+" work again")
			}
			c.Error, c.LastCopy, c.CopyBytes = "", rep.At, rep.Bytes
			if c.LastCopy == "" {
				c.LastCopy = h.now().UTC().Format(time.RFC3339)
			}
		}
	}
	h.saveLocked()
	h.mu.Unlock()
}

func (h *HA) copyOnce(ctx context.Context, c HAVM) (vm.Replica, error) {
	self := h.self()
	src, _ := h.C.node(c.Node)
	dst, _ := h.C.node(c.Target)
	var have []string
	var err error
	if c.Target == self {
		have, err = h.Local.ReplicaSnaps(ctx, c.Name)
	} else {
		var r struct {
			Snaps []string `json:"snaps"`
		}
		err = h.C.call(ctx, dst, "GET", "/cluster/replica/"+url.PathEscape(c.Name), nil, &r)
		have = r.Snaps
	}
	if err != nil {
		return vm.Replica{}, err
	}
	from := h.nodeName(c.Node)
	var rep vm.Replica
	sendPath := "/cluster/replica-send/" + url.PathEscape(c.Name) + "?" + url.Values{"have": {strings.Join(have, ",")}, "from": {from}}.Encode()
	switch {
	case c.Node == self:
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(h.Local.SendReplica(ctx, c.Name, have, from, pw)) }()
		resp, err := h.C.M.stream(ctx, dst, "POST", "/cluster/replica-recv", pr)
		pr.CloseWithError(errors.New("copy ended"))
		if err != nil {
			return rep, err
		}
		defer resp.Body.Close()
		err = json.NewDecoder(resp.Body).Decode(&rep)
		return rep, err
	case c.Target == self:
		resp, err := h.C.M.stream(ctx, src, "GET", sendPath, nil)
		if err != nil {
			return rep, err
		}
		defer resp.Body.Close()
		return h.Local.RecvReplica(ctx, resp.Body)
	default:
		in, err := h.C.M.stream(ctx, src, "GET", sendPath, nil)
		if err != nil {
			return rep, err
		}
		defer in.Body.Close()
		resp, err := h.C.M.stream(ctx, dst, "POST", "/cluster/replica-recv", in.Body)
		if err != nil {
			return rep, err
		}
		defer resp.Body.Close()
		err = json.NewDecoder(resp.Body).Decode(&rep)
		return rep, err
	}
}

func (h *HA) dropCopy(ctx context.Context, node, name string) error {
	if node == "" {
		return nil
	}
	if node == h.self() {
		return h.Local.DropReplica(ctx, name)
	}
	n, ok := h.C.node(node)
	if !ok {
		return nil
	}
	return h.C.call(ctx, n, "DELETE", "/cluster/replica/"+url.PathEscape(name), nil, nil)
}

func (h *HA) setAutostart(ctx context.Context, node, name string, on bool) error {
	if node == h.self() {
		_, err := h.Local.SetAutostart(ctx, name, on)
		return err
	}
	n, ok := h.C.node(node)
	if !ok {
		return fmt.Errorf("%w: server %s", vm.ErrNotFound, node)
	}
	return h.C.call(ctx, n, "PUT", vpath(name, "/autostart"), map[string]bool{"autostart": on}, nil)
}

func (h *HA) haCheck(ctx context.Context, node, name string) (string, error) {
	if node == h.self() {
		return h.Local.HACheck(ctx, name)
	}
	n, _ := h.C.node(node)
	var r struct {
		Why string `json:"why"`
	}
	err := h.C.call(ctx, n, "GET", vpath(name, "/ha-check"), nil, &r)
	return r.Why, err
}

// pickTarget: the other online server with the most free memory.
func (h *HA) pickTarget(ctx context.Context, node string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pickTargetLocked(node)
}

func (h *HA) pickTargetLocked(node string) string {
	best, bestFree := "", -1<<31
	if node != h.self() && h.C.M.LocalCapacity != nil {
		best, bestFree = h.self(), free(h.C.M.LocalCapacity(context.Background()))
	}
	for _, w := range h.C.workers() {
		st := h.C.M.NodeStatus(w.ID)
		if w.ID != node && st.Online && free(st.Capacity) > bestFree {
			best, bestFree = w.ID, free(st.Capacity)
		}
	}
	return best
}

func (h *HA) sameLAN(ctx context.Context, n Node) bool {
	if h.SameLAN != nil {
		return h.SameLAN(ctx, n)
	}
	return h.C.M.PeerOnLAN(ctx, n)
}

func (h *HA) gatewayOK(ctx context.Context) bool {
	if h.GatewayOK != nil {
		return h.GatewayOK(ctx)
	}
	return h.C.M.GatewayOK(ctx)
}

// noQuorum: this master reaches no majority of the servers. The others may
// elect a new master and start copies of its protected VMs, so it switches
// them off (once per outage), renews nobody's lease and fails nothing over.
func (h *HA) noQuorum(ctx context.Context) {
	h.mu.Lock()
	h.loadLocked()
	if h.quorumFenced {
		h.mu.Unlock()
		return
	}
	h.quorumFenced = true
	var names []string
	for name, c := range h.st.VMs {
		if c.Node == h.self() {
			names = append(names, name)
		}
	}
	h.mu.Unlock()
	sort.Strings(names)
	stopped := h.Local.Fence(ctx, names)
	h.event("critical", "this master can't reach most of the cluster: high availability paused"+
		map[bool]string{true: ", switched off " + strings.Join(stopped, ", "), false: ""}[len(stopped) > 0],
		"The other servers may choose a new master and start the copies there. This server follows the new master when the network is back.")
}

// --- worker side ---

// Sync is the master's heartbeat: names are this server's protected VMs.
// It returns the VMs this server switched off while cut off.
func (h *HA) Sync(names []string, fenceSec int) (fenced []string, boot int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lease, h.leaseVMs, h.fenceSec = h.now(), names, fenceSec
	f := h.fenced
	h.fenced = nil
	if f == nil {
		f = []string{}
	}
	return f, h.bootTime()
}

func (h *HA) workerTick(ctx context.Context) {
	h.mu.Lock()
	h.loadLocked()
	lease, names, sec := h.lease, h.leaseVMs, h.fenceSec
	h.mu.Unlock()
	if lease.IsZero() || len(names) == 0 || sec <= 0 || h.now().Sub(lease) < time.Duration(sec)*time.Second {
		return
	}
	if h.C.M.Voters() >= 3 {
		// with 3+ voters (servers, or 2 servers and a witness) a majority decides: on the majority side the VMs
		// keep running (a new master takes over); a cut-off server switches off
		if h.gatewayOK(ctx) && h.C.M.ReachesMajority(ctx) {
			return
		}
	} else if h.gatewayOK(ctx) {
		return // the master is gone, not us: keep running (it can't fail us over without its network)
	}
	stopped := h.Local.Fence(ctx, names)
	h.mu.Lock()
	h.fenced = append(h.fenced, stopped...)
	h.leaseVMs = nil // once per outage
	h.mu.Unlock()
	if len(stopped) > 0 {
		h.event("critical", "cut off from the cluster and the network: switched off "+strings.Join(stopped, ", "),
			"So they never run twice: the master starts their copies elsewhere. They start here again when the master says so.")
	}
}

// --- network checks (Manager) ---

// GatewayOK pings this server's default gateway.
func (m *Manager) GatewayOK(ctx context.Context) bool {
	if m.Run == nil {
		return true
	}
	out, err := m.Run.Run(ctx, "ip", "-4", "route", "show", "default")
	if err != nil {
		return false
	}
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "via" {
			for try := 0; try < 3; try++ {
				if _, err := m.Run.Run(ctx, "ping", "-c", "1", "-W", "1", f[i+1]); err == nil {
					return true
				}
			}
			return false
		}
	}
	return false
}

// PeerOnLAN says whether a worker's tunnel comes from this server's local network.
func (m *Manager) PeerOnLAN(ctx context.Context, n Node) bool {
	if m.Run == nil {
		return false
	}
	out, err := m.Run.Run(ctx, "wg", "show", Iface, "endpoints")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != n.WGPub {
			continue
		}
		host, _, err := net.SplitHostPort(f[1])
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsPrivate() {
			return false
		}
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.Contains(ip) && !strings.HasPrefix(ipn.IP.String(), "10.250.0.") {
				return true
			}
		}
	}
	return false
}

// HAInfo is the VM's High availability box.
type HAInfo struct {
	Protected bool         `json:"protected"`
	Config    *HAVMView    `json:"config,omitempty"`
	Why       string       `json:"why,omitempty"` // why it can't be protected
	Targets   []MoveTarget `json:"targets"`       // servers that can keep the copy
}

// Info tells the dashboard about one VM.
func (h *HA) Info(ctx context.Context, name string) (HAInfo, error) {
	info := HAInfo{Targets: []MoveTarget{}}
	if h.C.M == nil || h.C.M.Role() != "master" {
		info.Why = "high availability needs a cluster (Server → Cluster → Add a server)"
		if _, err := h.C.Get(ctx, name); err != nil {
			return info, err
		}
		return info, nil
	}
	v, err := h.C.Get(ctx, name)
	if err != nil {
		return info, err
	}
	node := v.NodeID
	if node == "" {
		node = h.self()
	}
	if c, ok := h.Config(ctx, name); ok {
		info.Protected, info.Config = true, &c
		node = c.Node
	} else if v.State != "unknown" {
		if info.Why, err = h.haCheck(ctx, node, name); err != nil {
			return info, err
		}
	}
	if node != h.self() {
		info.Targets = append(info.Targets, MoveTarget{ID: h.self(), Name: h.C.M.SelfName(), Online: true})
	}
	for _, w := range h.C.workers() {
		if w.ID != node {
			info.Targets = append(info.Targets, MoveTarget{ID: w.ID, Name: w.Name, Online: h.C.M.NodeStatus(w.ID).Online})
		}
	}
	return info, nil
}
