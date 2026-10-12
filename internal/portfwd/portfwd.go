// Package portfwd gives every VM and container on the private (NAT) network
// its own SSH port on the server, e.g. server:2201 -> web1:22, so they can be
// reached from other computers (NAT addresses exist only inside the server).
//
// p8sd itself relays the connections (no firewall NAT rules needed, so the
// VM guard and libvirt's rules stay as they are). Ports are stable: a guest
// keeps its port until it is deleted.
package portfwd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Guest is a VM or container that can get a port.
type Guest struct {
	Name string
	IP   string // current address ("" = not running / unknown)
	NAT  bool   // only on the private network (LAN/public guests are reachable directly)
}

type Manager struct {
	File   string                                     // ports.json
	Base   int                                        // first port, e.g. 2201
	Listen string                                     // address to listen on ("" = all)
	Guests func(ctx context.Context) ([]Guest, error) // VMs + containers
	Log    *slog.Logger
	Dial   func(ctx context.Context, addr string) (net.Conn, error) // nil = TCP (tests replace it)

	mu        sync.Mutex
	ports     map[string]int // guest -> port
	listeners map[string]net.Listener
	ips       map[string]string
}

func (m *Manager) load() {
	if m.ports != nil {
		return
	}
	m.ports, m.listeners, m.ips = map[string]int{}, map[string]net.Listener{}, map[string]string{}
	if b, err := os.ReadFile(m.File); err == nil {
		_ = json.Unmarshal(b, &m.ports)
	}
}

func (m *Manager) save() {
	b, _ := json.MarshalIndent(m.ports, "", "  ")
	_ = os.MkdirAll(filepath.Dir(m.File), 0o750)
	if os.WriteFile(m.File+".tmp", b, 0o600) == nil {
		_ = os.Rename(m.File+".tmp", m.File)
	}
}

// Ports returns guest -> SSH port.
func (m *Manager) Ports() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	out := map[string]int{}
	for n, p := range m.ports {
		if _, open := m.listeners[n]; open {
			out[n] = p
		}
	}
	return out
}

func (m *Manager) free() int {
	used := map[int]bool{}
	for _, p := range m.ports {
		used[p] = true
	}
	for p := m.Base; p < m.Base+5000; p++ {
		if !used[p] {
			return p
		}
	}
	return 0
}

// Sync opens ports for new NAT guests, follows address changes and closes
// ports of guests that are gone.
func (m *Manager) Sync(ctx context.Context) error {
	gs, err := m.Guests(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	seen := map[string]bool{}
	names := make([]string, 0, len(gs))
	byName := map[string]Guest{}
	for _, g := range gs {
		names = append(names, g.Name)
		byName[g.Name] = g
	}
	sort.Strings(names) // stable order for new ports
	changed := false
	for _, n := range names {
		g := byName[n]
		if !g.NAT {
			continue
		}
		seen[n] = true
		m.ips[n] = g.IP
		if _, ok := m.ports[n]; !ok {
			p := m.free()
			if p == 0 {
				continue
			}
			m.ports[n] = p
			changed = true
		}
		if _, open := m.listeners[n]; !open {
			l, err := net.Listen("tcp", net.JoinHostPort(m.Listen, fmt.Sprint(m.ports[n])))
			if err != nil {
				if m.Log != nil {
					m.Log.Warn("ssh port", "guest", n, "port", m.ports[n], "err", err)
				}
				continue
			}
			m.listeners[n] = l
			go m.serve(n, l)
		}
	}
	for n, l := range m.listeners {
		if !seen[n] {
			_ = l.Close()
			delete(m.listeners, n)
		}
	}
	for n := range m.ports {
		if _, exists := byName[n]; !exists { // deleted: give the port back
			delete(m.ports, n)
			delete(m.ips, n)
			changed = true
		}
	}
	if changed {
		m.save()
	}
	return nil
}

func (m *Manager) serve(name string, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go m.relay(name, c)
	}
}

func (m *Manager) relay(name string, c net.Conn) {
	defer c.Close()
	m.mu.Lock()
	ip := m.ips[name]
	m.mu.Unlock()
	if ip == "" {
		_, _ = io.WriteString(c, "P8S OS: "+name+" is not running or has no address yet\r\n")
		return
	}
	dial := m.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t, err := dial(ctx, net.JoinHostPort(ip, "22"))
	cancel()
	if err != nil {
		_, _ = io.WriteString(c, "P8S OS: can't reach SSH on "+name+" ("+err.Error()+")\r\n")
		return
	}
	defer t.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(t, c); closeWrite(t); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, t); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(interface{ CloseWrite() error }); ok {
		_ = tc.CloseWrite()
	}
}

// Close stops all listeners.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, l := range m.listeners {
		_ = l.Close()
		delete(m.listeners, n)
	}
}

// Run syncs every 15 seconds.
func (m *Manager) Run(ctx context.Context) {
	for {
		if err := m.Sync(ctx); err != nil && m.Log != nil && !errors.Is(err, context.Canceled) {
			m.Log.Warn("ssh ports", "err", err)
		}
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-time.After(15 * time.Second):
		}
	}
}
