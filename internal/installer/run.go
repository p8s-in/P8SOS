package installer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExecFunc runs one command, writing its output to log.
type ExecFunc func(ctx context.Context, stdin string, argv []string, log io.Writer) error

// Runner carries out a plan.
type Runner struct {
	Plan     *Plan
	Exec     ExecFunc  // nil: run for real
	Log      io.Writer // everything the commands print
	Progress func(pct int, title string)
	Exists   func(path string) bool // nil: os.Stat
	Wait     time.Duration          // how long to wait for new partitions (default 60 s)
}

// ChrootEnv is the environment of commands run inside the new system.
var ChrootEnv = []string{"DEBIAN_FRONTEND=noninteractive", "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

// Argv is the command line a step runs.
func (r *Runner) Argv(s Step) []string {
	if !s.Chroot {
		return s.Cmd
	}
	return append(append([]string{"chroot", r.Plan.Target, "env"}, ChrootEnv...), s.Cmd...)
}

// Run does every step; it stops at the first one that fails (unless the step may fail).
func (r *Runner) Run(ctx context.Context) error {
	steps := r.Plan.Steps()
	total := 0
	for _, s := range steps {
		total += weight(s)
	}
	done := 0
	logw := r.Log
	if logw == nil {
		logw = io.Discard
	}
	for i, s := range steps {
		if r.Progress != nil {
			r.Progress(done*100/total, s.Title)
		}
		fmt.Fprintf(logw, "\n### [%d/%d] %s\n### %s\n", i+1, len(steps), s.Title, s)
		err := r.step(ctx, s, logw)
		if err != nil && s.MayFail {
			fmt.Fprintf(logw, "### (ignored) %v\n", err)
			err = nil
		}
		if err != nil {
			fmt.Fprintf(logw, "### FAILED: %v\n", err)
			return fmt.Errorf("%s: %w", s.Title, err)
		}
		done += weight(s)
	}
	if r.Progress != nil {
		r.Progress(100, "Done")
	}
	return nil
}

func weight(s Step) int {
	if s.Weight > 0 {
		return s.Weight
	}
	return 1
}

func (r *Runner) step(ctx context.Context, s Step, logw io.Writer) error {
	switch {
	case s.File != nil:
		return writeFile(r.Plan.Target, *s.File)
	case len(s.Wait) > 0:
		return r.waitFor(ctx, s.Wait)
	}
	ex := r.Exec
	if ex == nil {
		ex = SystemExec
	}
	return ex(ctx, s.Stdin, r.Argv(s), logw)
}

func (r *Runner) waitFor(ctx context.Context, paths []string) error {
	exists := r.Exists
	if exists == nil {
		exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	limit := r.Wait
	if limit == 0 {
		limit = 60 * time.Second
	}
	deadline := time.Now().Add(limit)
	for {
		var missing []string
		for _, p := range paths {
			if !exists(p) {
				missing = append(missing, p)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("these did not appear: %s", strings.Join(missing, " "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// writeFile writes f below root, replacing whatever is there (a symlink too).
func writeFile(root string, f File) error {
	if !strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") {
		return fmt.Errorf("bad path %q", f.Path)
	}
	p := filepath.Join(root, f.Path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	_ = os.Remove(p)
	if err := os.WriteFile(p, []byte(f.Data), f.Mode); err != nil {
		return err
	}
	return os.Chmod(p, f.Mode)
}

// SystemExec runs the command for real.
func SystemExec(ctx context.Context, stdin string, argv []string, log io.Writer) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdout, c.Stderr = log, log
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	c.Env = append(os.Environ(), "LANG=C.UTF-8")
	return c.Run()
}
