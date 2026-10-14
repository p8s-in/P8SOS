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

// config renders the tunnel in `wg setconf` format. Every server listens, and
// peers with every other server it can dial (or that can dial it); pairs
// that can't reach each other directly go through the master.
func (st State) wgConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\nListenPort = %d\n", st.WGPriv, WGPort)
	peers := append([]Node(nil), st.Nodes...)
	peers = append(peers, st.Mesh...)
	via := map[string][]string{} // worker side: addresses reached through the master
	var masterID string
	if st.Role == "worker" && len(st.Nodes) > 0 {
		masterID = st.Nodes[0].ID
	}
	direct := func(n Node) bool {
		return n.ID == masterID || st.Role == "master" || n.Endpoint != "" || st.Self.Endpoint != ""
	}
	for _, n := range peers {
		if !direct(n) {
			via[masterID] = append(via[masterID], n.WGIP+"/32")
		}
	}
	for _, n := range peers {
		if !direct(n) {
			continue
		}
		allowed := []string{n.WGIP + "/32"}
		if n.ID == masterID {
			allowed = append(allowed, via[masterID]...)
			if len(st.Mesh) == 0 { // no book yet: the master passes traffic to the other workers
				allowed = []string{Subnet + ".0/24"}
			}
		}
		fmt.Fprintf(&b, "\n[Peer]\n# %s %s\nPublicKey = %s\nAllowedIPs = %s\n", n.ID, strings.ReplaceAll(n.Name, "\n", " "), n.WGPub, strings.Join(allowed, ", "))
		ep := n.Endpoint
		if n.ID == masterID && st.Endpoint != "" {
			ep = st.Endpoint // the address the join code was made for
		}
		if ep != "" {
			fmt.Fprintf(&b, "Endpoint = %s\n", ep)
		}
		if st.Role == "worker" || ep != "" { // keep NAT mappings open
			fmt.Fprintf(&b, "PersistentKeepalive = 25\n")
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
