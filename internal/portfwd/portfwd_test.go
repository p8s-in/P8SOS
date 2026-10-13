package portfwd

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestForward(t *testing.T) {
	// a fake SSH server
	ssh, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ssh.Close()
	go func() {
		for {
			c, err := ssh.Accept()
			if err != nil {
				return
			}
			go func() { fmt.Fprint(c, "SSH-2.0-OpenSSH_9.9\r\n"); c.Close() }()
		}
	}()
	sshAddr := ssh.Addr().String()

	// find a free base port
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	base := l.Addr().(*net.TCPAddr).Port
	l.Close()

	guests := []Guest{{Name: "web1", IP: "10.0.0.5", NAT: true}, {Name: "lan1", IP: "192.168.0.50"}, {Name: "ct1", NAT: true}}
	file := filepath.Join(t.TempDir(), "ports.json")
	m := &Manager{File: file, Base: base, Listen: "127.0.0.1",
		Guests: func(context.Context) ([]Guest, error) { return guests, nil },
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			if addr != "10.0.0.5:22" {
				return nil, fmt.Errorf("unexpected %s", addr)
			}
			return net.Dial("tcp", sshAddr)
		}}
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := m.Ports()
	if len(p) != 2 || p["ct1"] != base || p["web1"] != base+1 || p["lan1"] != 0 {
		t.Fatalf("ports %v", p)
	}
	read := func(port int) string {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		s, _ := bufio.NewReader(c).ReadString('\n')
		return s
	}
	if s := read(p["web1"]); !strings.HasPrefix(s, "SSH-2.0") {
		t.Fatalf("relay got %q", s)
	}
	if s := read(p["ct1"]); !strings.Contains(s, "not running") {
		t.Fatalf("stopped guest got %q", s)
	}
	// ports survive a restart; deleted guests give theirs back
	m.Close()
	guests = guests[:2]
	m2 := &Manager{File: file, Base: base, Listen: "127.0.0.1", Guests: m.Guests, Dial: m.Dial}
	if err := m2.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if p := m2.Ports(); len(p) != 1 || p["web1"] != base+1 {
		t.Fatalf("after restart %v", p)
	}
}

func TestForwardRDP(t *testing.T) {
	srv, _ := net.Listen("tcp", "127.0.0.1:0")
	defer srv.Close()
	go func() {
		for {
			c, err := srv.Accept()
			if err != nil {
				return
			}
			go func() { fmt.Fprint(c, "rdp\n"); c.Close() }()
		}
	}()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	base := l.Addr().(*net.TCPAddr).Port
	l.Close()
	guests := []Guest{{Name: "win1", IP: "10.0.0.9", NAT: true}, {Name: "off", NAT: true}}
	m := &Manager{File: filepath.Join(t.TempDir(), "rdp.json"), Base: base, Listen: "127.0.0.1", Target: 3389,
		Guests: func(context.Context) ([]Guest, error) { return guests, nil },
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			if addr != "10.0.0.9:3389" {
				return nil, fmt.Errorf("unexpected %s", addr)
			}
			return net.Dial("tcp", srv.Addr().String())
		}}
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	read := func(port int) string {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		s, _ := bufio.NewReader(c).ReadString('\n')
		return s
	}
	if s := read(m.Ports()["win1"]); s != "rdp\n" {
		t.Fatalf("relay got %q", s)
	}
	// no text banner for non-SSH protocols when the guest is down
	if s := read(m.Ports()["off"]); s != "" {
		t.Fatalf("stopped guest got %q", s)
	}
}
