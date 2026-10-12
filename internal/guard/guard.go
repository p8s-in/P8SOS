// Package guard keeps VMs away from the P8S server itself and from the home
// network, with nftables rules p8sd installs at start-up:
//
//   - VMs on the private network (NAT bridge) can use DHCP, DNS and ping on the
//     host, nothing else on it (not the dashboard, not SSH), and can't reach
//     private address ranges (your home LAN, other private networks). The
//     internet works.
//   - VMs on the home network (LAN bridge) can't reach the host either.
//
// The host can still reach every VM (p8 ssh, console) because replies to
// connections the host opened are allowed.
package guard

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"p8sos/internal/execx"
)

// Config says which bridges to guard and what stays reachable.
type Config struct {
	NATBridge string   // e.g. virbr0
	LANBridge string   // e.g. br0; empty = no home-network bridge
	Allow     []string // CIDRs VMs may still reach on private ranges (e.g. a NAS)
}

var (
	reIface = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
	reCIDR  = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}(/[0-9]{1,2})?$`)
)

const table = "p8s_guard"

// Rules returns the nft scripts: one for the inet family (always) and one for
// the bridge family (only with a LAN bridge). Each replaces its table atomically.
func Rules(c Config) (inet, bridge string, err error) {
	if !reIface.MatchString(c.NATBridge) || (c.LANBridge != "" && !reIface.MatchString(c.LANBridge)) {
		return "", "", fmt.Errorf("guard: bad bridge name %q/%q", c.NATBridge, c.LANBridge)
	}
	for _, a := range c.Allow {
		if !reCIDR.MatchString(a) {
			return "", "", fmt.Errorf("guard: vm_allow entry %q is not an IPv4 address or CIDR", a)
		}
	}
	nat := c.NATBridge
	allow := ""
	if len(c.Allow) > 0 {
		allow = fmt.Sprintf("\t\tiifname %q ip daddr { %s } accept\n", nat, strings.Join(c.Allow, ", "))
	}
	inet = fmt.Sprintf(`table inet %[1]s
delete table inet %[1]s
table inet %[1]s {
	set private4 {
		type ipv4_addr; flags interval
		elements = { 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16 }
	}
	chain input {
		type filter hook input priority -5; policy accept;
		iifname %[2]q ct state established,related accept
		iifname %[2]q udp dport { 53, 67 } accept
		iifname %[2]q tcp dport 53 accept
		iifname %[2]q icmp type echo-request accept
		iifname %[2]q counter drop comment "VMs may not use the P8S server"
	}
	chain forward {
		type filter hook forward priority -5; policy accept;
		iifname %[2]q ct state established,related accept
%[3]s		iifname %[2]q ip daddr @private4 counter drop comment "VMs may not reach the home network"
		iifname %[2]q ip6 daddr { fc00::/7, fe80::/10 } counter drop
	}
}
`, table, nat, allow)
	if c.LANBridge != "" {
		bridge = fmt.Sprintf(`table bridge %[1]s
delete table bridge %[1]s
table bridge %[1]s {
	chain input {
		type filter hook input priority -5; policy accept;
		meta ibrname %[2]q iifname "vnet*" ether type arp accept
		meta ibrname %[2]q iifname "vnet*" meta l4proto ipv6-icmp accept
		meta ibrname %[2]q iifname "vnet*" ct state established,related accept
		meta ibrname %[2]q iifname "vnet*" counter drop comment "home-network VMs may not use the P8S server"
	}
}
`, table, c.LANBridge)
	}
	return inet, bridge, nil
}

// Apply installs the rules. The bridge part needs bridge conntrack; if that
// fails, the inet part still applies and the error says what is missing.
func Apply(ctx context.Context, run execx.Runner, c Config) error {
	inet, bridge, err := Rules(c)
	if err != nil {
		return err
	}
	if err := nftFile(ctx, run, inet); err != nil {
		return fmt.Errorf("guard (private network): %w", err)
	}
	if bridge != "" {
		_, _ = run.Run(ctx, "modprobe", "nf_conntrack_bridge")
		if err := nftFile(ctx, run, bridge); err != nil {
			return fmt.Errorf("guard (home network): %w", err)
		}
	} else {
		_, _ = run.Run(ctx, "nft", "delete", "table", "bridge", table) // LAN bridge removed
	}
	return nil
}

// Remove deletes the rules (guard turned off in the config).
func Remove(ctx context.Context, run execx.Runner) {
	_, _ = run.Run(ctx, "nft", "delete", "table", "inet", table)
	_, _ = run.Run(ctx, "nft", "delete", "table", "bridge", table)
}

func nftFile(ctx context.Context, run execx.Runner, script string) error {
	f, err := os.CreateTemp("", "p8s-guard-*.nft")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return err
	}
	f.Close()
	_, err = run.Run(ctx, "nft", "-f", f.Name())
	return err
}
