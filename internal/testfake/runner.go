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
}

func (r *Runner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := strings.Join(append([]string{name}, args...), " ")
	r.Calls = append(r.Calls, line)
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
