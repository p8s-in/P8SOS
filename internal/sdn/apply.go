package sdn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"p8sos/internal/execx"
)

// Where is this server's place in the cluster, as the vnets need it.
type Where struct {
	Self    string   // this server's tunnel address ("" = not in a cluster)
	Peers   []string // the other servers' tunnel addresses
	Gateway bool     // this server runs the gateways (the master, or a server on its own)
}

// Manager makes the vnets real on this server.
type Manager struct {
	Store   *Store
	Run     execx.Runner
	Where   func() Where
	DataDir string // leases and the boot list (sdn.list)
	mu      sync.Mutex
}

func (m *Manager) run(ctx context.Context, args ...string) ([]byte, error) {
	return m.Run.Run(ctx, args[0], args[1:]...)
}

// BridgeOf is the bridge of the vnet called name (VMs use it).
func (m *Manager) BridgeOf(ctx context.Context, name string) (string, error) {
	v, ok := m.Store.Load().Find(name)
	if !ok {
		return "", fmt.Errorf("%w: there is no virtual network %q (Settings → Virtual networks)", ErrInvalid, name)
	}
	br := Bridge(v.VNI)
	if l, err := m.links(ctx); err != nil || l[br].IfName == "" {
		if err := m.Apply(ctx); err != nil {
			return "", err
		}
	}
	return br, nil
}

// NameOf is the vnet a bridge belongs to ("" = none).
func (m *Manager) NameOf(bridge string) string {
	vni := VNIOf(bridge)
	if vni == 0 {
		return ""
	}
	for _, v := range m.Store.Load().VNets {
		if v.VNI == vni {
			return v.Name
		}
	}
	return ""
}

// Names lists the vnets.
func (m *Manager) Names() []string {
	var out []string
	for _, v := range m.Store.Load().VNets {
		out = append(out, v.Name)
	}
	return out
}

type ipLink struct {
	IfName string `json:"ifname"`
	VNI    int    // vxlan devices: their id and local address
	Local  string
}

// links lists this server's vnet devices (vxl*, vxb*). The details of VXLAN
// devices come from the text output (some ip versions print broken JSON for them).
func (m *Manager) links(ctx context.Context) (map[string]ipLink, error) {
	out, err := m.run(ctx, "ip", "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var l []ipLink
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("ip link: %v", err)
	}
	got := map[string]ipLink{}
	for _, x := range l {
		switch {
		case VNIOf(x.IfName) > 0:
			got[x.IfName] = x
		case strings.HasPrefix(x.IfName, "vxl") && VNIOf("vxb"+x.IfName[3:]) > 0:
			if d, err := m.run(ctx, "ip", "-d", "-o", "link", "show", "dev", x.IfName); err == nil {
				f := strings.Fields(string(d))
				for i := 0; i+1 < len(f); i++ {
					switch f[i] {
					case "id":
						if x.VNI == 0 {
							x.VNI, _ = strconv.Atoi(f[i+1])
						}
					case "local":
						x.Local = f[i+1]
					}
				}
			}
			got[x.IfName] = x
		}
	}
	return got, nil
}

type fdbEntry struct {
	MAC string `json:"mac"`
	Dst string `json:"dst"`
}

const zeroMAC = "00:00:00:00:00:00"

// Apply makes this server's vnet devices, gateways and firewall match the
// settings. It is safe to run any time (p8sd runs it at start, after
// changes and every minute, so new or removed servers are picked up).
func (m *Manager) Apply(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.Store.Load()
	w := Where{}
	if m.Where != nil {
		w = m.Where()
	}
	have, err := m.links(ctx)
	if err != nil {
		return fmt.Errorf("virtual networks: %w", err)
	}
	want := map[string]bool{}
	var errs []string
	for _, v := range c.VNets {
		want[Bridge(v.VNI)], want[Dev(v.VNI)] = true, true
		if err := m.applyOne(ctx, v, w, have); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", v.Name, err))
		}
	}
	// gone vnets: devices and gateways away
	names := make([]string, 0, len(have))
	for n := range have {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !want[n] {
			if _, err := m.run(ctx, "ip", "link", "del", n); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if err := m.gateways(ctx, c, w); err != nil {
		errs = append(errs, err.Error())
	}
	if err := m.writeList(c, w); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("virtual networks: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (m *Manager) applyOne(ctx context.Context, v VNet, w Where, have map[string]ipLink) error {
	br, dev := Bridge(v.VNI), Dev(v.VNI)
	mtu := strconv.Itoa(MTU)
	if _, ok := have[br]; !ok {
		if _, err := m.run(ctx, "ip", "link", "add", br, "type", "bridge", "stp_state", "0", "forward_delay", "0"); err != nil {
			return err
		}
		_, _ = m.run(ctx, "sysctl", "-qw", "net.ipv6.conf."+br+".disable_ipv6=1") // the host stays off it (no IPv6: nothing to do)
	}
	for _, s := range [][]string{{"ip", "link", "set", br, "mtu", mtu}, {"ip", "link", "set", br, "up"}} {
		if _, err := m.run(ctx, s...); err != nil {
			return err
		}
	}
	l, ok := have[dev]
	if w.Self == "" { // on its own: the bridge is the whole network
		if ok {
			_, err := m.run(ctx, "ip", "link", "del", dev)
			return err
		}
		return nil
	}
	if ok && (l.VNI != v.VNI || l.Local != w.Self) {
		if _, err := m.run(ctx, "ip", "link", "del", dev); err != nil {
			return err
		}
		ok = false
	}
	if !ok {
		if _, err := m.run(ctx, "ip", "link", "add", dev, "type", "vxlan", "id", strconv.Itoa(v.VNI),
			"local", w.Self, "dstport", strconv.Itoa(Port)); err != nil {
			return err
		}
	}
	for _, s := range [][]string{
		{"ip", "link", "set", dev, "mtu", mtu},
		{"ip", "link", "set", dev, "master", br},
		{"ip", "link", "set", dev, "up"},
	} {
		if _, err := m.run(ctx, s...); err != nil {
			return err
		}
	}
	return m.fdb(ctx, dev, w.Peers)
}

// fdb points broadcasts (and unknown destinations) at every other server.
func (m *Manager) fdb(ctx context.Context, dev string, peers []string) error {
	out, err := m.run(ctx, "bridge", "-j", "fdb", "show", "dev", dev)
	if err != nil {
		return err
	}
	var l []fdbEntry
	_ = json.Unmarshal(out, &l)
	cur := map[string]bool{}
	for _, e := range l {
		if e.MAC == zeroMAC && e.Dst != "" {
			cur[e.Dst] = true
		}
	}
	want := map[string]bool{}
	for _, p := range peers {
		want[p] = true
		if !cur[p] {
			if _, err := m.run(ctx, "bridge", "fdb", "append", zeroMAC, "dev", dev, "dst", p); err != nil {
				return err
			}
		}
	}
	for d := range cur {
		if !want[d] {
			_, _ = m.run(ctx, "bridge", "fdb", "del", zeroMAC, "dev", dev, "dst", d)
		}
	}
	return nil
}

// unit is the dnsmasq service of a vnet.
func unit(v VNet) string { return "p8s-vnet-" + v.Name }

// gateways sets the gateway address, DHCP/DNS and NAT of vnets with a subnet
// on the gateway server, and removes them everywhere else.
func (m *Manager) gateways(ctx context.Context, c Config, w Where) error {
	var errs []string
	wantUnits := map[string]bool{}
	var gw []VNet
	for _, v := range c.VNets {
		br := Bridge(v.VNI)
		if v.Subnet == "" || !w.Gateway {
			_, _ = m.run(ctx, "ip", "-4", "addr", "flush", "dev", br)
			continue
		}
		pl, err := PlanOf(v.Subnet)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		gw = append(gw, v)
		addr := fmt.Sprintf("%s/%d", pl.Gateway, pl.Prefix.Bits())
		if _, err := m.run(ctx, "ip", "-4", "addr", "replace", addr, "dev", br); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		wantUnits[unit(v)] = true
		if err := m.dnsmasq(ctx, v, pl); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// stop dnsmasq of vnets that are gone (or not ours to serve)
	if out, err := m.run(ctx, "systemctl", "list-units", "--all", "--plain", "--no-legend", "p8s-vnet-*"); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			u := strings.TrimSuffix(f[0], ".service")
			if strings.HasPrefix(u, "p8s-vnet-") && !wantUnits[u] {
				_, _ = m.run(ctx, "systemctl", "stop", u)
			}
		}
	}
	if err := m.nft(ctx, len(c.VNets) > 0, gw); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// dnsmasqArgs is the DHCP / DNS server of a vnet.
func (m *Manager) dnsmasqArgs(v VNet, pl Plan) []string {
	return []string{"/usr/sbin/dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null",
		"--pid-file=/run/" + unit(v) + ".pid",
		"--bind-interfaces", "--interface=" + Bridge(v.VNI), "--except-interface=lo",
		"--listen-address=" + pl.Gateway.String(),
		fmt.Sprintf("--dhcp-range=%s,%s,%s,12h", pl.First, pl.Last, maskOf(pl)),
		"--dhcp-option=option:router," + pl.Gateway.String(),
		"--dhcp-option=option:dns-server," + pl.Gateway.String(),
		fmt.Sprintf("--dhcp-option=option:mtu,%d", MTU),
		"--dhcp-authoritative", "--domain-needed", "--bogus-priv",
		"--dhcp-leasefile=/var/lib/misc/" + unit(v) + ".leases"}
}

func maskOf(pl Plan) string {
	bits := pl.Prefix.Bits()
	var b [4]byte
	for i := 0; i < bits; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}

func (m *Manager) dnsmasq(ctx context.Context, v VNet, pl Plan) error {
	args := m.dnsmasqArgs(v, pl)
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	desc := "P8S virtual network " + v.Name + " DHCP/DNS " + hex.EncodeToString(sum[:6])
	u := unit(v)
	out, _ := m.run(ctx, "systemctl", "show", "-p", "Description", "-p", "ActiveState", "--value", u)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 2 && strings.TrimSpace(lines[0]) == "active" && strings.TrimSpace(lines[1]) == desc ||
		len(lines) == 2 && strings.TrimSpace(lines[1]) == "active" && strings.TrimSpace(lines[0]) == desc {
		return nil // running with these settings
	}
	_, _ = m.run(ctx, "systemctl", "stop", u)
	_, _ = m.run(ctx, "systemctl", "reset-failed", u)
	run := append([]string{"systemd-run", "--unit=" + u, "--description=" + desc, "--collect",
		"-p", "Restart=always", "-p", "RestartSec=3"}, args...)
	if _, err := m.run(ctx, run...); err != nil {
		return fmt.Errorf("DHCP for %s: %w", v.Name, err)
	}
	return nil
}

// private ranges vnet VMs don't reach through the gateway (as NAT VMs).
var private = "10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16"

// Tunnel is the cluster's WireGuard interface: VXLAN packets are only
// accepted from it (no one on the home network can inject frames).
const Tunnel = "p8s-wg"

// Rules is the nftables table: VXLAN only through the tunnel, and the gateways.
func Rules(any bool, gw []VNet) string {
	var b strings.Builder
	b.WriteString("table inet p8s_sdn {}\ndelete table inet p8s_sdn\n")
	if !any {
		return b.String()
	}
	b.WriteString("table inet p8s_sdn {\n")
	b.WriteString("\tchain input {\n\t\ttype filter hook input priority -5; policy accept;\n")
	fmt.Fprintf(&b, "\t\tudp dport %d iifname != %q drop\n", Port, Tunnel)
	if len(gw) == 0 {
		b.WriteString("\t}\n}\n")
		return b.String()
	}
	b.WriteString("\t\tiifname \"vxb*\" ct state established,related accept\n")
	b.WriteString("\t\tiifname \"vxb*\" udp dport { 53, 67 } accept\n")
	b.WriteString("\t\tiifname \"vxb*\" tcp dport 53 accept\n")
	b.WriteString("\t\tiifname \"vxb*\" icmp type echo-request accept\n")
	b.WriteString("\t\tiifname \"vxb*\" drop\n\t}\n")
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority -5; policy accept;\n")
	b.WriteString("\t\toifname \"vxb*\" ct state established,related accept\n")
	for _, v := range gw {
		fmt.Fprintf(&b, "\t\tiifname %q ip daddr %s accept\n", Bridge(v.VNI), v.Subnet)
	}
	fmt.Fprintf(&b, "\t\tiifname \"vxb*\" ip daddr { %s } drop\n", private)
	for _, v := range gw {
		if !v.NATOn() {
			fmt.Fprintf(&b, "\t\tiifname %q drop\n", Bridge(v.VNI))
		}
	}
	b.WriteString("\t\toifname \"vxb*\" ct state new drop\n\t}\n")
	b.WriteString("\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	for _, v := range gw {
		if v.NATOn() {
			fmt.Fprintf(&b, "\t\tip saddr %s oifname != %q masquerade\n", v.Subnet, Bridge(v.VNI))
		}
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

func (m *Manager) nft(ctx context.Context, any bool, gw []VNet) error {
	if len(gw) > 0 {
		_, _ = m.run(ctx, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	}
	f, err := os.CreateTemp("", "p8s-sdn-*.nft")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, _ = f.WriteString(Rules(any, gw))
	f.Close()
	if _, err := m.run(ctx, "nft", "-f", f.Name()); err != nil {
		return fmt.Errorf("gateway firewall: %w", err)
	}
	return nil
}

// writeList keeps sdn.list for the boot script (devices before libvirt
// starts the VMs): "vni local" per line ("-" = no cluster).
func (m *Manager) writeList(c Config, w Where) error {
	if m.DataDir == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("# P8S virtual networks (made by p8sd; read by sdn.sh at boot): vni local-address\n")
	local := w.Self
	if local == "" {
		local = "-"
	}
	for _, v := range c.VNets {
		fmt.Fprintf(&b, "%d %s\n", v.VNI, local)
	}
	p := filepath.Join(m.DataDir, "sdn.list")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
