package api

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/audit"
	"p8sos/internal/ct"
)

func TestHostShell(t *testing.T) {
	m := newMultiUserCfg(t, func(s *Server) {
		s.Audit = &audit.Log{Path: filepath.Join(t.TempDir(), "audit.log")}
		s.HostShell = func() (*ct.Terminal, error) {
			return ct.StartPTY(exec.Command("sh", "-c", "echo hello-from-root-shell; sleep 2"))
		}
	})
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	m.req("POST", "/api/v1/node/shell", alice, "", 403) // customers never
	tk := m.req("POST", "/api/v1/node/shell", secret, "", 201)
	path := tk["path"].(string)

	c, err := net.Dial("tcp", strings.TrimPrefix(m.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	host := strings.TrimPrefix(m.srv.URL, "http://")
	io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: "+host+"\r\nOrigin: "+m.srv.URL+"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: MDEyMzQ1Njc4OWFiY2RlZg==\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade %v %v", resp, err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var out strings.Builder
	for !strings.Contains(out.String(), "hello-from-root-shell") {
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(br, hdr); err != nil {
			t.Fatalf("read: %v (got %q)", err, out.String())
		}
		p := make([]byte, hdr[1]&0x7f)
		io.ReadFull(br, p)
		out.Write(p)
	}
	c.Close()
	time.Sleep(300 * time.Millisecond)
	// opened (and closed) with the admin's name in the activity log
	l := m.req("GET", "/api/v1/audit?limit=20", secret, "", 200)
	found := false
	for _, e := range l["entries"].([]any) {
		if d, _ := e.(map[string]any)["detail"].(string); strings.Contains(d, "server shell opened") {
			found = true
		}
	}
	if !found {
		t.Fatalf("not audited: %v", l)
	}
	// turned off: no tickets
	m.server.HostShell = nil
	m.req("POST", "/api/v1/node/shell", secret, "", 404)
}
