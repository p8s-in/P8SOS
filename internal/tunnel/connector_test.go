package tunnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitState(t *testing.T, c *Connector, want string) ConnectorStatus {
	t.Helper()
	var st ConnectorStatus
	for i := 0; i < 200; i++ {
		if st = c.Status(); st.State == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state %q, want %q (%s)", st.State, want, st.Detail)
	return st
}

func TestConnector(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	c := &Connector{Command: func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			`echo "$TUNNEL_TOKEN" > `+envFile+`; echo "2026-10-12T10:00:00Z INF Registered tunnel connection connIndex=0" >&2; exec sleep 30`)
	}}
	if c.Status().State != "off" {
		t.Fatal("not off at start")
	}
	c.Run("tok-1")
	waitState(t, c, "connected")
	if b, _ := os.ReadFile(envFile); string(b) != "tok-1\n" {
		t.Fatalf("token passed as %q", b)
	}
	c.Run("tok-1") // same token: keeps running
	if c.Status().State != "connected" {
		t.Fatal("restarted for the same token")
	}
	c.Run("tok-2") // new tunnel
	waitState(t, c, "connected")
	if b, _ := os.ReadFile(envFile); string(b) != "tok-2\n" {
		t.Fatalf("new token passed as %q", b)
	}
	c.Stop()
	if st := c.Status(); st.State != "off" {
		t.Fatalf("after stop: %+v", st)
	}

	// a cloudflared that keeps failing shows the error and is retried
	bad := &Connector{Command: func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `echo "2026-10-12T10:00:00Z ERR Provided Tunnel token is not valid." >&2; exit 1`)
	}}
	bad.Run("x")
	st := waitState(t, bad, "error")
	if st.Detail == "" || !strings.Contains(st.Detail, "token is not valid") {
		t.Fatalf("error detail %q", st.Detail)
	}
	bad.Stop()
}
