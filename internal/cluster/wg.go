package cluster

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WireGuard keys are X25519 keys, base64 encoded (what `wg genkey` makes).
func wgKeys() (priv, pub string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	b[0] &= 248
	b[31] = (b[31] & 127) | 64
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(b), base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

func wgPubOK(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("bad WireGuard key")
	}
	return b, nil
}

// config renders the tunnel in `wg setconf` format.
func (st State) wgConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\n", st.WGPriv)
	if st.Role == "master" {
		fmt.Fprintf(&b, "ListenPort = %d\n", WGPort)
	}
	for _, n := range st.Nodes {
		allowed := n.WGIP + "/32"
		if st.Role == "worker" { // the master passes traffic between workers (live moves)
			allowed = Subnet + ".0/24"
		}
		fmt.Fprintf(&b, "\n[Peer]\n# %s %s\nPublicKey = %s\nAllowedIPs = %s\n", n.ID, strings.ReplaceAll(n.Name, "\n", " "), n.WGPub, allowed)
		if st.Role == "worker" { // dial out to the master; keep NAT mappings open
			fmt.Fprintf(&b, "Endpoint = %s\nPersistentKeepalive = 25\n", st.Endpoint)
		}
	}
	return b.String()
}

// Apply brings the tunnel up as the saved state says (p8sd start, joins).
func (m *Manager) Apply(ctx context.Context) error { return m.applyWG(ctx) }

func (m *Manager) applyWG(ctx context.Context) error {
	m.mu.Lock()
	_ = m.load()
	st := m.st
	m.mu.Unlock()
	if m.Run == nil {
		return nil
	}
	if st.Role == "" {
		return m.downWG(ctx)
	}
	conf := filepath.Join(m.Dir, "wg.conf")
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(conf, []byte(st.wgConfig()), 0o600); err != nil {
		return err
	}
	if _, err := m.Run.Run(ctx, "ip", "link", "show", Iface); err != nil {
		if _, err := m.Run.Run(ctx, "ip", "link", "add", Iface, "type", "wireguard"); err != nil {
			return fmt.Errorf("create %s (is WireGuard available? sudo apt install wireguard-tools): %w", Iface, err)
		}
	}
	steps := [][]string{
		{"wg", "setconf", Iface, conf},
		{"ip", "address", "replace", st.Self.WGIP + "/24", "dev", Iface},
		{"ip", "link", "set", Iface, "mtu", "1380", "up"},
	}
	for _, s := range steps {
		if _, err := m.Run.Run(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	if st.Role == "master" { // workers reach each other through the master
		_, _ = m.Run.Run(ctx, "sysctl", "-qw", "net.ipv4.conf."+Iface+".forwarding=1")
		_, _ = m.Run.Run(ctx, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	}
	if m.OnTunnel != nil {
		m.OnTunnel(ctx, st.Self.WGIP)
	}
	return nil
}

func (m *Manager) downWG(ctx context.Context) error {
	if m.Run == nil {
		return nil
	}
	if m.OnTunnel != nil {
		m.OnTunnel(ctx, "")
	}
	if _, err := m.Run.Run(ctx, "ip", "link", "show", Iface); err == nil {
		_, err := m.Run.Run(ctx, "ip", "link", "del", Iface)
		return err
	}
	return nil
}
