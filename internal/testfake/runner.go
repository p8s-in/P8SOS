// Package testfake provides in-memory fakes shared by tests.
package testfake

import (
	"context"
	"strings"
	"sync"

	"p8sos/internal/execx"
)

// Runner records commands and returns scripted failures by command prefix.
type Runner struct {
	mu    sync.Mutex
	Calls []string
	Fail  map[string]string // "zfs set" -> stderr
	Out   map[string]string // "zfs list" -> stdout
	// Hook sees every command first (with the lock held: it may change Out and Fail).
	Hook func(r *Runner, line string)
}

func (r *Runner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := strings.Join(append([]string{name}, args...), " ")
	r.Calls = append(r.Calls, line)
	if r.Hook != nil {
		r.Hook(r, line)
	}
	for prefix, stderr := range r.Fail {
		if strings.HasPrefix(line, prefix) {
			return nil, &execx.Error{Cmd: name, Args: args, Stderr: stderr, Err: context.Canceled}
		}
	}
	for prefix, out := range r.Out {
		if strings.HasPrefix(line, prefix) {
			return []byte(out), nil
		}
	}
	return nil, nil
}
