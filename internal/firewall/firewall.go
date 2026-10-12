// Package firewall is the per-VM firewall: when it is on for a VM, only the
// incoming traffic its rules allow reaches the VM; everything the VM itself
// starts (and the replies) still works.
//
// It is one nftables table in the bridge family, matching frames by the VM's
// MAC address, so it works the same for VMs on the private network (traffic
// the host routes or port-forwards to them) and on the home network (traffic
// bridged from the LAN):
//
//	forward hook - frames bridged to the VM (home network, other VMs)
//	output hook  - frames the host sends to the VM (private network)
package firewall

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"p8sos/internal/execx"
)

// Rule allows one kind of incoming traffic.
type Rule struct {
	Proto   string `json:"proto"`             // tcp, udp or icmp (ping)
	Ports   string `json:"ports,omitempty"`   // "22", "80,443", "8000-8100" (tcp/udp)
	Source  string `json:"source,omitempty"`  // only from this address or network (IPv4/IPv6 CIDR); "" = anywhere
	Comment string `json:"comment,omitempty"` // shown in the dashboard
}

// Settings is one VM's firewall.
type Settings struct {
	Enabled bool   `json:"enabled"` // off = everything may come in (the default)
	Rules   []Rule `json:"rules"`
}

// VM is what Rules needs to know about a VM.
type VM struct {
	Name string
	MAC  string
	FW   Settings
	// LockIP pins a VM with a public address: frames from its ports must
	// carry its own MAC and this IPv4 (no taking a neighbour's address).
	LockIP string
	Ports  []string // the VM's ports on the host (vnetN) while it runs
}

// ErrInvalid wraps every validation problem.
var ErrInvalid = errors.New("invalid firewall rule")

const MaxRules = 32

var (
	reMAC     = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	rePort    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
	reComment = regexp.MustCompile(`^[\pL\pN .,:;()_/+-]{0,60}$`)
)

// Validate checks settings and returns them tidied up (ports and sources normalized).
func Validate(s Settings) (Settings, error) {
	if len(s.Rules) > MaxRules {
		return s, fmt.Errorf("%w: at most %d rules", ErrInvalid, MaxRules)
	}
	out := Settings{Enabled: s.Enabled, Rules: []Rule{}}
	for i, r := range s.Rules {
		n := i + 1
		r.Proto = strings.ToLower(strings.TrimSpace(r.Proto))
		r.Comment = strings.TrimSpace(r.Comment)
		if !reComment.MatchString(r.Comment) {
			return s, fmt.Errorf("%w: rule %d: comment may use letters, digits and . , : ; ( ) _ / + - (60 max)", ErrInvalid, n)
		}
		switch r.Proto {
		case "tcp", "udp":
			p, err := normPorts(r.Ports)
			if err != nil {
				return s, fmt.Errorf("%w: rule %d: %v", ErrInvalid, n, err)
			}
			r.Ports = p
		case "icmp":
			r.Ports = ""
		default:
			return s, fmt.Errorf("%w: rule %d: protocol must be tcp, udp or icmp", ErrInvalid, n)
		}
		src, err := normSource(r.Source)
		if err != nil {
			return s, fmt.Errorf("%w: rule %d: %v", ErrInvalid, n, err)
		}
		r.Source = src
		out.Rules = append(out.Rules, r)
	}
	return out, nil
}

// normPorts turns "22, 80,443 ,8000-8100" into "22,80,443,8000-8100".
func normPorts(s string) (string, error) {
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return "", errors.New("give a port, e.g. 22 or 80,443 or 8000-8100")
	}
	parts := strings.Split(s, ",")
	if len(parts) > 15 {
		return "", errors.New("at most 15 ports or ranges per rule")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		lo, hi, isRange := strings.Cut(p, "-")
		a, err1 := strconv.Atoi(lo)
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(hi)
		}
		if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
			return "", fmt.Errorf("%q is not a port (1-65535) or range like 8000-8100", p)
		}
		if a == b {
			out = append(out, strconv.Itoa(a))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", a, b))
		}
	}
	return strings.Join(out, ","), nil
}

func normSource(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "any" || s == "0.0.0.0/0" {
		return "", nil
	}
	if ip := net.ParseIP(s); ip != nil {
		if ip.To4() != nil {
			return ip.String() + "/32", nil
		}
		return ip.String() + "/128", nil
	}
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		return "", fmt.Errorf("source %q is not an IP address or network like 203.0.113.0/24", s)
	}
	return n.String(), nil
}

const Table = "p8s_fw"

// Rules renders the whole table. VMs with the firewall off are left out
// (everything passes). The script replaces the table atomically.
func Rules(vms []VM) (string, error) {
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	var b strings.Builder
	fmt.Fprintf(&b, "table bridge %[1]s\ndelete table bridge %[1]s\ntable bridge %[1]s {\n", Table)
	var jumps, locks []string
	n := 0
	for _, v := range vms {
		mac := strings.ToLower(v.MAC)
		if (v.FW.Enabled || v.LockIP != "") && !reMAC.MatchString(mac) {
			return "", fmt.Errorf("firewall: vm %q has a bad MAC %q", v.Name, v.MAC)
		}
		if v.LockIP != "" {
			ip, err := netip.ParseAddr(v.LockIP)
			if err != nil || !ip.Is4() {
				return "", fmt.Errorf("firewall: vm %q: bad address %q", v.Name, v.LockIP)
			}
			for _, p := range v.Ports {
				if !rePort.MatchString(p) {
					return "", fmt.Errorf("firewall: vm %q: bad port name %q", v.Name, p)
				}
				locks = append(locks,
					fmt.Sprintf("\t\tiifname %q ether saddr != %s counter drop comment %q\n", p, mac, v.Name+": not its MAC"),
					fmt.Sprintf("\t\tiifname %q ether type ip ip saddr != %s counter drop comment %q\n", p, ip, v.Name+": not its IP"),
					fmt.Sprintf("\t\tiifname %q ether type arp arp saddr ip != %s counter drop comment %q\n", p, ip, v.Name+": ARP for another IP"))
			}
		}
		if !v.FW.Enabled {
			continue
		}
		fw, err := Validate(v.FW)
		if err != nil {
			return "", fmt.Errorf("firewall: vm %q: %w", v.Name, err)
		}
		n++
		chain := fmt.Sprintf("vm%d", n)
		fmt.Fprintf(&b, "\tchain %s {\n", chain)
		fmt.Fprintf(&b, "\t\tct state established,related accept\n")
		fmt.Fprintf(&b, "\t\tether type arp accept\n")
		fmt.Fprintf(&b, "\t\tmeta l4proto ipv6-icmp accept comment \"IPv6 neighbor discovery\"\n")
		fmt.Fprintf(&b, "\t\tmeta l4proto udp th sport 67 th dport 68 accept comment \"DHCP\"\n")
		fmt.Fprintf(&b, "\t\tmeta l4proto udp th sport 547 th dport 546 accept comment \"DHCPv6\"\n")
		for _, r := range fw.Rules {
			b.WriteString("\t\t")
			if r.Source != "" {
				if strings.Contains(r.Source, ":") {
					fmt.Fprintf(&b, "ip6 saddr %s ", r.Source)
				} else {
					fmt.Fprintf(&b, "ip saddr %s ", r.Source)
				}
			}
			if r.Proto == "icmp" {
				b.WriteString("icmp type echo-request accept\n")
				continue
			}
			ports := r.Ports
			if strings.ContainsAny(ports, ",") {
				ports = "{ " + strings.ReplaceAll(ports, ",", ", ") + " }"
			}
			fmt.Fprintf(&b, "meta l4proto %s th dport %s accept\n", r.Proto, ports)
		}
		fmt.Fprintf(&b, "\t\tcounter drop comment %q\n", "firewall of "+v.Name)
		b.WriteString("\t}\n")
		jumps = append(jumps, fmt.Sprintf("\t\tether daddr %s jump %s\n", mac, chain))
	}
	// Anti-spoofing for VMs with public addresses, before anything else.
	b.WriteString("\tchain spoof {\n\t\ttype filter hook prerouting priority -200; policy accept;\n")
	for _, l := range locks {
		b.WriteString(l)
	}
	b.WriteString("\t}\n")
	for _, hook := range []string{"forward", "output"} {
		fmt.Fprintf(&b, "\tchain %s {\n\t\ttype filter hook %s priority 0; policy accept;\n", hook, hook)
		for _, j := range jumps {
			b.WriteString(j)
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// Apply loads the table (bridge conntrack is needed for "replies are allowed").
func Apply(ctx context.Context, run execx.Runner, vms []VM) error {
	script, err := Rules(vms)
	if err != nil {
		return err
	}
	_, _ = run.Run(ctx, "modprobe", "nf_conntrack_bridge")
	f, err := os.CreateTemp("", "p8s-fw-*.nft")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if _, err := run.Run(ctx, "nft", "-f", f.Name()); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	return nil
}
