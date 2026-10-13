// Package vlan puts VMs on tagged VLANs of the home / datacenter network.
//
// For each VLAN in use there is a small bridge "<lan>v<id>" (br0v100) whose
// only uplink is "p8sv<id>", a VLAN device on the physical port behind the
// home-network bridge. A VM on VLAN 100 plugs into br0v100 and sees only
// that VLAN's traffic, untagged; the switch port must carry the VLAN tagged.
// The host itself has no address on these bridges. The list is kept in
// vlans.list so p8s-vlans.service recreates them at boot before libvirt
// starts the VMs.
package vlan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"p8sos/internal/execx"
)

var ErrInvalid = errors.New("invalid")

// Manager creates the VLAN bridges.
type Manager struct {
	Run       execx.Runner
	LANBridge string // e.g. br0
	ListFile  string // e.g. /var/lib/p8s/vlans.list
	SysNet    string // /sys/class/net (tests replace it)

	mu sync.Mutex
}

func (m *Manager) sys() string {
	if m.SysNet != "" {
		return m.SysNet
	}
	return "/sys/class/net"
}

func (m *Manager) exists(dev string) bool {
	_, err := os.Stat(filepath.Join(m.sys(), dev))
	return err == nil
}

// BridgeName is the bridge of a VLAN.
func (m *Manager) BridgeName(vid int) string { return fmt.Sprintf("%sv%d", m.LANBridge, vid) }

// VLANOf says which VLAN a bridge carries (0 = none).
func (m *Manager) VLANOf(bridge string) int {
	if m == nil || m.LANBridge == "" {
		return 0
	}
	rest, ok := strings.CutPrefix(bridge, m.LANBridge+"v")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || n > 4094 {
		return 0
	}
	return n
}

// Uplink is the physical port behind the home-network bridge.
func (m *Manager) Uplink() (string, error) {
	if m.LANBridge == "" {
		return "", fmt.Errorf("%w: the home network isn't set up (sudo p8 lan-setup)", ErrInvalid)
	}
	ents, err := os.ReadDir(filepath.Join(m.sys(), m.LANBridge, "brif"))
	if err != nil {
		return "", fmt.Errorf("%w: bridge %s not found", ErrInvalid, m.LANBridge)
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "vnet") || strings.HasPrefix(n, "tap") || strings.HasPrefix(n, "veth") || strings.HasPrefix(n, "vc") {
			continue // VMs and containers
		}
		return n, nil
	}
	return "", fmt.Errorf("%w: %s has no network port", ErrInvalid, m.LANBridge)
}

// Bridge makes sure VLAN vid's bridge exists and returns its name.
func (m *Manager) Bridge(ctx context.Context, vid int) (string, error) {
	if vid < 1 || vid > 4094 {
		return "", fmt.Errorf("%w: VLAN must be 1-4094", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	br := m.BridgeName(vid)
	if len(br) > 15 {
		return "", fmt.Errorf("%w: bridge name %s too long", ErrInvalid, br)
	}
	up, err := m.Uplink()
	if err != nil {
		return "", err
	}
	dev := fmt.Sprintf("p8sv%d", vid)
	if !m.exists(br) || !m.exists(dev) {
		steps := [][]string{}
		if !m.exists(dev) {
			steps = append(steps, []string{"ip", "link", "add", "link", up, "name", dev, "type", "vlan", "id", strconv.Itoa(vid)})
		}
		if !m.exists(br) {
			steps = append(steps,
				[]string{"ip", "link", "add", br, "type", "bridge", "stp_state", "0", "forward_delay", "0"},
				[]string{"sysctl", "-qw", "net.ipv6.conf." + br + ".disable_ipv6=1"}) // the host stays off this VLAN
		}
		steps = append(steps,
			[]string{"ip", "link", "set", dev, "master", br},
			[]string{"ip", "link", "set", dev, "up"},
			[]string{"ip", "link", "set", br, "up"})
		for _, s := range steps {
			if _, err := m.Run.Run(ctx, s[0], s[1:]...); err != nil {
				return "", fmt.Errorf("VLAN %d: %w", vid, err)
			}
		}
	}
	if err := m.remember(up, vid); err != nil {
		return "", err
	}
	return br, nil
}

// remember adds the VLAN to the list recreated at boot ("uplink vid bridge" per line).
func (m *Manager) remember(up string, vid int) error {
	if m.ListFile == "" {
		return nil
	}
	have := m.listLocked()
	for _, v := range have {
		if v == vid {
			return nil
		}
	}
	have = append(have, vid)
	sort.Ints(have)
	var b strings.Builder
	b.WriteString("# P8S OS VLAN bridges (p8s-vlans.service makes them at boot): uplink vlan bridge\n")
	for _, v := range have {
		fmt.Fprintf(&b, "%s %d %s\n", up, v, m.BridgeName(v))
	}
	if err := os.MkdirAll(filepath.Dir(m.ListFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.ListFile+".tmp", []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(m.ListFile+".tmp", m.ListFile)
}

func (m *Manager) listLocked() []int {
	b, err := os.ReadFile(m.ListFile)
	if err != nil {
		return nil
	}
	var out []int
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && !strings.HasPrefix(l, "#") {
			if n, err := strconv.Atoi(f[1]); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// List is the VLANs with bridges.
func (m *Manager) List() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.listLocked()
	if l == nil {
		l = []int{}
	}
	return l
}
