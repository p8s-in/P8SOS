// Package vpn is the remote-access VPN for admins: WireGuard on UDP 51830.
// Each phone or laptop gets its own key and address in 10.251.0.0/24; from
// there it reaches the dashboard (https://10.251.0.1:8006), the SSH / RDP
// ports of the VMs, and optionally the home network behind this server.
package vpn

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"p8sos/internal/execx"
)

const (
	Iface       = "p8s-vpn"
	DefaultPort = 51830
	Subnet      = "10.251.0.0/24"
	ServerIP    = "10.251.0.1"
)

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
)

// Device is a phone or laptop that may connect.
type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Pub     string `json:"pub"`
	PSK     string `json:"psk,omitempty"`
	IP      string `json:"ip"`
	Created string `json:"created_at"`
	By      string `json:"by,omitempty"`
}

// Settings is what the admin chooses.
type Settings struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"` // public name or IP the devices dial (router forwards UDP Port here)
	Port     int    `json:"port"`
	LAN      bool   `json:"lan"` // devices also reach the home network behind this server
	LANNet   string `json:"lan_net,omitempty"`
	MTU      int    `json:"mtu,omitempty"`
}

type state struct {
	Settings
	Priv    string   `json:"priv"`
	Pub     string   `json:"pub"`
	Devices []Device `json:"devices"`
}

// Status is the Remote access section.
type Status struct {
	Settings
	Pub     string       `json:"server_pub"`
	Devices []DeviceView `json:"devices"`
	Up      bool         `json:"up"`
}

// DeviceView is a device with its last handshake.
type DeviceView struct {
	Device
	LastSeen string `json:"last_seen,omitempty"`
}

// Manager keeps the VPN.
type Manager struct {
	Dir string
	Run execx.Runner // nil = don't touch the system (tests)
	Now func() time.Time

	mu sync.Mutex
	st *state
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) file() string { return filepath.Join(m.Dir, "vpn.json") }

func (m *Manager) loadLocked() error {
	if m.st != nil {
		return nil
	}
	st := &state{}
	if b, err := os.ReadFile(m.file()); err == nil {
		if err := json.Unmarshal(b, st); err != nil {
			return err
		}
	}
	if st.Port == 0 {
		st.Port = DefaultPort
	}
	if st.Priv == "" {
		var err error
		if st.Priv, st.Pub, err = keys(); err != nil {
			return err
		}
	}
	m.st = st
	return nil
}

func (m *Manager) saveLocked() error {
	b, _ := json.MarshalIndent(m.st, "", "  ")
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(m.file()+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(m.file()+".tmp", m.file())
}

func keys() (priv, pub string, err error) {
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

func psk() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// Status returns the settings and devices.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return Status{}, err
	}
	st := *m.st
	devs := append([]Device{}, m.st.Devices...)
	m.mu.Unlock()
	out := Status{Settings: st.Settings, Pub: st.Pub, Devices: []DeviceView{}}
	hs := m.handshakes(ctx)
	out.Up = hs != nil
	for _, d := range devs {
		v := DeviceView{Device: d}
		v.PSK = ""
		if t, ok := hs[d.Pub]; ok {
			v.LastSeen = t.UTC().Format(time.RFC3339)
		}
		out.Devices = append(out.Devices, v)
	}
	return out, nil
}

var reEndpoint = regexp.MustCompile(`^[A-Za-z0-9.:\-\[\]]{1,253}$`)

// SetSettings changes the settings and applies them.
func (m *Manager) SetSettings(ctx context.Context, s Settings) (Settings, error) {
	s.Endpoint = strings.TrimSpace(s.Endpoint)
	if s.Port == 0 {
		s.Port = DefaultPort
	}
	if s.Port < 1024 || s.Port > 65535 {
		return Settings{}, fmt.Errorf("%w: port 1024-65535", ErrInvalid)
	}
	if s.Enabled && (s.Endpoint == "" || !reEndpoint.MatchString(s.Endpoint)) {
		return Settings{}, fmt.Errorf("%w: give the public name or IP your devices should dial (e.g. myhome.duckdns.org)", ErrInvalid)
	}
	if s.LANNet != "" {
		if _, _, err := net.ParseCIDR(s.LANNet); err != nil {
			return Settings{}, fmt.Errorf("%w: home network must look like 192.168.0.0/24", ErrInvalid)
		}
	}
	if s.MTU != 0 && (s.MTU < 1280 || s.MTU > 1420) {
		return Settings{}, fmt.Errorf("%w: MTU 1280-1420", ErrInvalid)
	}
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return Settings{}, err
	}
	m.st.Settings = s
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return Settings{}, err
	}
	return s, m.Apply(ctx)
}

var reDevName = regexp.MustCompile(`^[\p{L}\p{N} ._'()-]{1,40}$`)

// AddDevice makes keys for a new device and returns its config (shown once:
// the device's private key isn't kept).
func (m *Manager) AddDevice(ctx context.Context, name, by string) (Device, string, error) {
	name = strings.TrimSpace(name)
	if !reDevName.MatchString(name) {
		return Device{}, "", fmt.Errorf("%w: name the device (e.g. Ravi's phone)", ErrInvalid)
	}
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return Device{}, "", err
	}
	if !m.st.Enabled {
		m.mu.Unlock()
		return Device{}, "", fmt.Errorf("%w: turn the VPN on first", ErrInvalid)
	}
	used := map[string]bool{}
	for _, d := range m.st.Devices {
		used[d.IP] = true
	}
	ip := ""
	for i := 2; i < 255; i++ {
		c := fmt.Sprintf("10.251.0.%d", i)
		if !used[c] {
			ip = c
			break
		}
	}
	if ip == "" {
		m.mu.Unlock()
		return Device{}, "", fmt.Errorf("%w: 253 devices is the limit", ErrInvalid)
	}
	priv, pub, err := keys()
	if err != nil {
		m.mu.Unlock()
		return Device{}, "", err
	}
	idb := make([]byte, 4)
	_, _ = rand.Read(idb)
	d := Device{ID: hex.EncodeToString(idb), Name: name, Pub: pub, PSK: psk(), IP: ip, Created: m.now().UTC().Format(time.RFC3339), By: by}
	m.st.Devices = append(m.st.Devices, d)
	st := *m.st
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return Device{}, "", err
	}
	m.mu.Unlock()
	if err := m.Apply(ctx); err != nil {
		return Device{}, "", err
	}
	return d, clientConfig(st, d, priv), nil
}

func clientConfig(st state, d Device, priv string) string {
	allowed := Subnet
	if st.LAN && st.LANNet != "" {
		allowed += ", " + st.LANNet
	}
	mtu := st.MTU
	if mtu == 0 {
		mtu = 1380
	}
	host := st.Endpoint
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf(`# P8S OS remote access — %s
[Interface]
PrivateKey = %s
Address = %s/32
MTU = %d

[Peer]
PublicKey = %s
PresharedKey = %s
Endpoint = %s:%d
AllowedIPs = %s
PersistentKeepalive = 25
`, d.Name, priv, d.IP, mtu, st.Pub, d.PSK, host, st.Port, allowed)
}

// RemoveDevice stops a device from connecting.
func (m *Manager) RemoveDevice(ctx context.Context, id string) error {
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	keep, found := []Device{}, false
	for _, d := range m.st.Devices {
		if d.ID == id {
			found = true
			continue
		}
		keep = append(keep, d)
	}
	if !found {
		m.mu.Unlock()
		return fmt.Errorf("%w: device %s", ErrNotFound, id)
	}
	m.st.Devices = keep
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return m.Apply(ctx)
}

func (st state) serverConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\nListenPort = %d\n", st.Priv, st.Port)
	for _, d := range st.Devices {
		fmt.Fprintf(&b, "\n[Peer]\n# %s %s\nPublicKey = %s\nPresharedKey = %s\nAllowedIPs = %s/32\n",
			d.ID, strings.ReplaceAll(d.Name, "\n", " "), d.Pub, d.PSK, d.IP)
	}
	return b.String()
}

// nft rules: home-network access is masqueraded (devices look like the server).
func (st state) nftRules() string {
	var b strings.Builder
	b.WriteString("table inet p8s_vpn\ndelete table inet p8s_vpn\ntable inet p8s_vpn {\n")
	b.WriteString("  chain forward {\n    type filter hook forward priority -5; policy accept;\n")
	b.WriteString("    iifname \"" + Iface + "\" oifname \"" + Iface + "\" drop\n") // devices can't reach each other
	b.WriteString("  }\n")
	if st.LAN && st.LANNet != "" {
		fmt.Fprintf(&b, "  chain post {\n    type nat hook postrouting priority 100; policy accept;\n    ip saddr %s ip daddr %s masquerade\n  }\n", Subnet, st.LANNet)
	}
	b.WriteString("}\n")
	return b.String()
}

// Apply brings the interface in line with the state (down when off).
func (m *Manager) Apply(ctx context.Context) error {
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	st := *m.st
	st.Devices = append([]Device{}, m.st.Devices...)
	m.mu.Unlock()
	if m.Run == nil {
		return nil
	}
	if !st.Enabled {
		_, _ = m.Run.Run(ctx, "ip", "link", "del", Iface)
		_, _ = m.Run.Run(ctx, "nft", "delete", "table", "inet", "p8s_vpn")
		return nil
	}
	conf := filepath.Join(m.Dir, "vpn-wg.conf")
	if err := os.WriteFile(conf, []byte(st.serverConfig()), 0o600); err != nil {
		return err
	}
	if _, err := m.Run.Run(ctx, "ip", "link", "show", Iface); err != nil {
		if _, err := m.Run.Run(ctx, "ip", "link", "add", Iface, "type", "wireguard"); err != nil {
			return fmt.Errorf("create %s (is WireGuard available? sudo apt install wireguard-tools): %w", Iface, err)
		}
	}
	mtu := st.MTU
	if mtu == 0 {
		mtu = 1380
	}
	for _, s := range [][]string{
		{"wg", "setconf", Iface, conf},
		{"ip", "address", "replace", ServerIP + "/24", "dev", Iface},
		{"ip", "link", "set", Iface, "mtu", fmt.Sprint(mtu), "up"},
	} {
		if _, err := m.Run.Run(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	if st.LAN {
		_, _ = m.Run.Run(ctx, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	}
	nf := filepath.Join(m.Dir, "vpn.nft")
	if err := os.WriteFile(nf, []byte(st.nftRules()), 0o600); err != nil {
		return err
	}
	_, err := m.Run.Run(ctx, "nft", "-f", nf)
	return err
}

// handshakes: last handshake per device key (nil = interface down).
func (m *Manager) handshakes(ctx context.Context) map[string]time.Time {
	if m.Run == nil {
		return nil
	}
	b, err := m.Run.Run(ctx, "wg", "show", Iface, "latest-handshakes")
	if err != nil {
		return nil
	}
	out := map[string]time.Time{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			var sec int64
			if _, err := fmt.Sscan(f[1], &sec); err == nil && sec > 0 {
				out[f[0]] = time.Unix(sec, 0)
			}
		}
	}
	return out
}

// Contains says whether ip is a VPN device address (always allowed in).
func Contains(ip net.IP) bool {
	_, n, _ := net.ParseCIDR(Subnet)
	return n.Contains(ip)
}
