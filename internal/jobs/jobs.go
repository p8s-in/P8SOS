// Package jobs runs long operations (backups, restores) in the background and
// keeps their status so the dashboard and CLI can follow them.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

type State string

const (
	Running State = "running"
	Done    State = "done"
	Failed  State = "failed"
)

// Job is one background operation.
type Job struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`   // "backup", "restore", ...
	Target   string `json:"target"` // usually the VM name
	State    State  `json:"state"`
	Message  string `json:"message"` // progress, or the result
	Error    string `json:"error,omitempty"`
	Started  string `json:"started_at"`
	Finished string `json:"finished_at,omitempty"`
}

// ErrBusy means the target already has a running job.
var ErrBusy = errors.New("busy")

// Manager tracks jobs. The zero value is ready to use.
type Manager struct {
	Log  *slog.Logger
	Keep int // finished jobs remembered; 0 = 100

	mu   sync.Mutex
	jobs map[string]*Job
	wg   sync.WaitGroup
}

// Progress lets a running job report what it is doing.
type Progress func(msg string)

// Start runs fn in the background. Only one job per target runs at a time.
func (m *Manager) Start(kind, target string, fn func(ctx context.Context, p Progress) (string, error)) (Job, error) {
	m.mu.Lock()
	if m.jobs == nil {
		m.jobs = map[string]*Job{}
	}
	for _, j := range m.jobs {
		if j.Target == target && j.State == Running {
			m.mu.Unlock()
			return Job{}, fmt.Errorf("%w: %s %q is already running for %s", ErrBusy, j.Kind, j.ID, target)
		}
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	j := &Job{ID: hex.EncodeToString(b), Kind: kind, Target: target, State: Running,
		Message: "starting", Started: time.Now().UTC().Format(time.RFC3339)}
	m.jobs[j.ID] = j
	m.prune()
	snapshot := *j
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		msg, err := fn(context.Background(), func(s string) {
			m.mu.Lock()
			j.Message = s
			m.mu.Unlock()
		})
		m.mu.Lock()
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		if err != nil {
			j.State, j.Error = Failed, err.Error()
		} else {
			j.State, j.Message = Done, msg
		}
		done := *j
		m.mu.Unlock()
		if m.Log != nil {
			m.Log.Info("job finished", "id", done.ID, "kind", done.Kind, "target", done.Target, "state", done.State, "error", done.Error)
		}
	}()
	return snapshot, nil
}

// Get returns one job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// List returns jobs, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Started != out[b].Started {
			return out[a].Started > out[b].Started
		}
		return out[a].ID > out[b].ID
	})
	return out
}

// Wait blocks until all running jobs end (tests, shutdown).
func (m *Manager) Wait() { m.wg.Wait() }

// prune forgets the oldest finished jobs beyond Keep. Caller holds mu.
func (m *Manager) prune() {
	keep := m.Keep
	if keep == 0 {
		keep = 100
	}
	var finished []*Job
	for _, j := range m.jobs {
		if j.State != Running {
			finished = append(finished, j)
		}
	}
	if len(finished) <= keep {
		return
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].Finished < finished[b].Finished })
	for _, j := range finished[:len(finished)-keep] {
		delete(m.jobs, j.ID)
	}
}
