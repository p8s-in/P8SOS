package tunnel

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Connector runs cloudflared for the tunnel as a child of p8sd (no separate
// service to install): it is started when public access is set up, restarted
// if it exits, and stopped when public access is switched off. The tunnel
// token goes in through the environment, not the command line.
type Connector struct {
	// Command builds the process; nil = cloudflared from PATH (tests replace it).
	Command func(ctx context.Context) *exec.Cmd

	mu      sync.Mutex
	token   string
	cancel  context.CancelFunc
	done    chan struct{}
	state   string // "off", "starting", "connected", "error"
	detail  string // last useful log line
	since   time.Time
	missing bool
}

// ConnectorStatus is shown in the dashboard.
type ConnectorStatus struct {
	State  string `json:"state"`            // off, starting, connected, error, not_installed
	Detail string `json:"detail,omitempty"` // last cloudflared message
	Since  string `json:"since,omitempty"`
}

func (c *Connector) Status() ConnectorStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := ConnectorStatus{State: c.state, Detail: c.detail}
	if st.State == "" {
		st.State = "off"
	}
	if c.missing {
		st.State = "not_installed"
		st.Detail = "cloudflared is not installed on this server (it comes with install.sh / update.sh)"
	}
	if !c.since.IsZero() {
		st.Since = c.since.UTC().Format(time.RFC3339)
	}
	return st
}

// Installed reports whether cloudflared can be run.
func (c *Connector) Installed() bool {
	if c.Command != nil {
		return true
	}
	_, err := exec.LookPath("cloudflared")
	return err == nil
}

// Run makes sure cloudflared runs with this token ("" = stop it).
func (c *Connector) Run(token string) {
	c.mu.Lock()
	if token == c.token && (token == "" || c.cancel != nil) {
		c.mu.Unlock()
		return
	}
	cancel, done := c.cancel, c.done
	c.token, c.cancel, c.done = token, nil, nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if token == "" {
		c.state, c.detail, c.since = "off", "", time.Time{}
		return
	}
	ctx, cancelFn := context.WithCancel(context.Background())
	c.cancel, c.done = cancelFn, make(chan struct{})
	c.state, c.detail, c.since = "starting", "", time.Now()
	go c.loop(ctx, token, c.done)
}

// Stop ends cloudflared (p8sd shutting down).
func (c *Connector) Stop() { c.Run("") }

func (c *Connector) set(state, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state != c.state {
		c.since = time.Now()
	}
	c.state = state
	if detail != "" {
		c.detail = detail
	}
}

func (c *Connector) loop(ctx context.Context, token string, done chan struct{}) {
	defer close(done)
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		c.mu.Lock()
		c.missing = !c.Installed()
		c.mu.Unlock()
		if !c.missing {
			start := time.Now()
			c.once(ctx, token)
			if time.Since(start) > time.Minute {
				backoff = 2 * time.Second
			}
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (c *Connector) once(ctx context.Context, token string) {
	var cmd *exec.Cmd
	if c.Command != nil {
		cmd = c.Command(ctx)
	} else {
		cmd = exec.CommandContext(ctx, "cloudflared", "tunnel", "--no-autoupdate", "run")
	}
	cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+token)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.StderrPipe()
	if err != nil {
		c.set("error", err.Error())
		return
	}
	cmd.Stdout = nil
	c.set("starting", "")
	if err := cmd.Start(); err != nil {
		c.set("error", err.Error())
		return
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "Registered tunnel connection"):
			c.set("connected", "connected to Cloudflare")
		case strings.Contains(line, " ERR "), strings.Contains(line, "error="):
			if i := strings.Index(line, " ERR "); i >= 0 {
				line = strings.TrimSpace(line[i+5:])
			}
			c.mu.Lock()
			c.detail = line
			c.mu.Unlock()
		}
	}
	err = cmd.Wait()
	if ctx.Err() == nil {
		msg := "cloudflared stopped"
		if err != nil {
			msg += ": " + err.Error()
		}
		c.mu.Lock()
		if c.detail != "" && c.detail != "connected to Cloudflare" {
			msg += " (" + c.detail + ")"
		}
		c.mu.Unlock()
		c.set("error", msg+"; restarting")
	}
}
