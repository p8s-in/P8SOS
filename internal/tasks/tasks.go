// Package tasks runs host jobs the dashboard starts (add an OS template, grow
// storage, set up the home network, Debian updates, reboot) as their own
// systemd units, like Proxmox's task log: each keeps running if p8sd restarts,
// and its output is read back from the journal.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"p8sos/internal/execx"
)

// Kinds of task.
const (
	Template    = "template"     // arg: catalog name, e.g. ubuntu-24.04
	StorageGrow = "storage-grow" // use the system disk's free space for VMs
	LANSetup    = "lan-setup"    // bridge VMs onto the home network
	OSCheck     = "os-check"     // apt-get update + list what can be upgraded
	OSUpgrade   = "os-upgrade"   // install Debian updates
	Reboot      = "reboot"       // restart the server
)

// Titles for the task log.
var Titles = map[string]string{
	Template: "Add OS template", StorageGrow: "Grow VM storage", LANSetup: "Set up the home network",
	OSCheck: "Check for Debian updates", OSUpgrade: "Install Debian updates", Reboot: "Reboot the server",
}

// Task is one run.
type Task struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Arg     string `json:"arg,omitempty"`
	By      string `json:"by"`
	Unit    string `json:"unit"`
	Started string `json:"started"`
	State   string `json:"state"`          // running, ok, failed
	Exit    *int   `json:"exit,omitempty"` // exit code when finished
	Log     string `json:"log,omitempty"`
}

var (
	ErrBusy    = errors.New("another server task is running; wait for it to finish")
	ErrInvalid = errors.New("invalid")
)

var reArg = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,30}$`)

type Manager struct {
	Dir     string // data_dir/tasks: tasks.json and exit codes
	Scripts string // folder with make-template.sh, storage.sh, setup-bridge.sh, os-upgrade.sh
	Run     execx.Runner
	Now     func() time.Time

	mu   sync.Mutex
	list []Task
	load bool
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) loadList() {
	if m.load {
		return
	}
	m.load = true
	if b, err := os.ReadFile(filepath.Join(m.Dir, "tasks.json")); err == nil {
		_ = json.Unmarshal(b, &m.list)
	}
}

func (m *Manager) save() error {
	if len(m.list) > 50 {
		m.list = m.list[len(m.list)-50:]
	}
	b, _ := json.MarshalIndent(m.list, "", "  ")
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(m.Dir, "tasks.json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// command is what a task runs.
func (m *Manager) command(kind, arg string) ([]string, error) {
	sh := func(name string, args ...string) []string {
		return append([]string{"/bin/bash", filepath.Join(m.Scripts, name)}, args...)
	}
	switch kind {
	case Template:
		if !reArg.MatchString(arg) {
			return nil, fmt.Errorf("%w: template name", ErrInvalid)
		}
		return sh("make-template.sh", arg, "--register"), nil
	case StorageGrow:
		return sh("storage.sh", "grow"), nil
	case LANSetup:
		return sh("setup-bridge.sh"), nil
	case OSCheck:
		return sh("os-upgrade.sh", "check"), nil
	case OSUpgrade:
		return sh("os-upgrade.sh", "upgrade"), nil
	case Reboot:
		return []string{"/bin/systemctl", "reboot"}, nil
	}
	return nil, fmt.Errorf("%w: task %q", ErrInvalid, kind)
}

// Start runs a task in its own systemd unit. Only one runs at a time.
func (m *Manager) Start(ctx context.Context, kind, arg, by string) (Task, error) {
	cmd, err := m.command(kind, arg)
	if err != nil {
		return Task{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadList()
	for i := range m.list {
		if m.list[i].State == "running" && m.refresh(ctx, &m.list[i]) == "running" {
			return Task{}, ErrBusy
		}
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return Task{}, err
	}
	now := m.now()
	id := strconv.FormatInt(now.UnixNano(), 36)
	t := Task{ID: id, Kind: kind, Title: Titles[kind], Arg: arg, By: by, Unit: "p8s-task-" + kind + "-" + id,
		Started: now.UTC().Format(time.RFC3339), State: "running"}
	rc := filepath.Join(m.Dir, id+".rc")
	// the wrapper records the exit code (the unit is gone once it finishes)
	args := []string{"--unit=" + t.Unit, "--collect", "--quiet", "--setenv=HOME=/root", "--setenv=P8S_YES=1",
		"--setenv=DEBIAN_FRONTEND=noninteractive", "/bin/bash", "-c", `"$@"; echo $? > ` + rc, "p8s-task"}
	if _, err := m.Run.Run(ctx, "systemd-run", append(args, cmd...)...); err != nil {
		return Task{}, fmt.Errorf("start task: %w", err)
	}
	m.list = append(m.list, t)
	return t, m.save()
}

// refresh updates a running task's state from its exit-code file and unit.
func (m *Manager) refresh(ctx context.Context, t *Task) string {
	if t.State != "running" {
		return t.State
	}
	if b, err := os.ReadFile(filepath.Join(m.Dir, t.ID+".rc")); err == nil {
		code, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		t.Exit = &code
		t.State = "ok"
		if code != 0 {
			t.State = "failed"
		}
		return t.State
	}
	out, _ := m.Run.Run(ctx, "systemctl", "is-active", t.Unit)
	if s := strings.TrimSpace(string(out)); s != "active" && s != "activating" && s != "" {
		// gone without an exit code: killed, or the server restarted under it
		if started, err := time.Parse(time.RFC3339, t.Started); err == nil && m.now().Sub(started) > 10*time.Second {
			t.State = "failed"
			if t.Kind == Reboot {
				t.State = "ok"
			}
		}
	}
	return t.State
}

// List returns recent tasks, newest first, without logs.
func (m *Manager) List(ctx context.Context) []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadList()
	changed := false
	for i := range m.list {
		before := m.list[i].State
		if m.refresh(ctx, &m.list[i]) != before {
			changed = true
		}
	}
	if changed {
		_ = m.save()
	}
	out := append([]Task{}, m.list...)
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	return out
}

// Get returns one task with its log.
func (m *Manager) Get(ctx context.Context, id string) (Task, bool) {
	for _, t := range m.List(ctx) {
		if t.ID == id {
			if log, err := m.Run.Run(ctx, "journalctl", "-u", t.Unit, "-o", "cat", "-n", "400", "--no-pager"); err == nil {
				t.Log = string(log)
			}
			return t, true
		}
	}
	return Task{}, false
}

// Running is the task in progress, if any.
func (m *Manager) Running(ctx context.Context) (Task, bool) {
	for _, t := range m.List(ctx) {
		if t.State == "running" {
			return t, true
		}
	}
	return Task{}, false
}
