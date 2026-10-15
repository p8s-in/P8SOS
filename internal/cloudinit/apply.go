package cloudinit

import (
	"fmt"
	"strings"
)

// ApplyScript is a shell script that sets a guest's public addresses on the
// network card with st.MAC: now (ip commands) and, with persist, for the next
// boots too (netplan, systemd-networkd or ifupdown files; cloud-init's own
// network setup is switched off so it doesn't put the old address back).
// remove are addresses (with /prefix) to take off the card. It prints
// "persisted: <how>" at the end. Runs as root in the guest (QEMU guest agent,
// or lxc-attach for containers).
func ApplyScript(st Static, remove []string, persist bool) string {
	addrs := []string{fmt.Sprintf("%s/%d", st.IP, st.Prefix)}
	for _, x := range st.Extra {
		addrs = append(addrs, fmt.Sprintf("%s/%d", x, st.Prefix))
	}
	if st.IP6 != "" {
		addrs = append(addrs, fmt.Sprintf("%s/%d", st.IP6, st.Prefix6))
	}
	mac := strings.ToLower(st.MAC)
	var b strings.Builder
	b.WriteString("set -e\nexport PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
	fmt.Fprintf(&b, "IF=''\nfor d in /sys/class/net/*; do [ \"$(cat \"$d/address\" 2>/dev/null)\" = %q ] && IF=$(basename \"$d\") && break; done\n", mac)
	fmt.Fprintf(&b, "[ -n \"$IF\" ] || { echo 'no network card with MAC %s' >&2; exit 3; }\n", mac)
	for _, r := range remove {
		fmt.Fprintf(&b, "ip addr del %s dev \"$IF\" 2>/dev/null || true\n", r)
	}
	v4 := addrs
	if st.IP6 != "" {
		v4 = addrs[:len(addrs)-1]
	}
	for _, a := range v4 {
		fmt.Fprintf(&b, "ip addr replace %s dev \"$IF\"\n", a)
	}
	onlink := ""
	if st.OnLink {
		onlink = " onlink"
		fmt.Fprintf(&b, "ip route replace %s dev \"$IF\"\n", st.Gateway)
	}
	fmt.Fprintf(&b, "ip route replace default via %s dev \"$IF\"%s\n", st.Gateway, onlink)
	if st.IP6 != "" { // last, and not fatal: some systems have IPv6 switched off
		fmt.Fprintf(&b, "ip addr replace %s/%d dev \"$IF\" || echo 'IPv6 is switched off in this system' >&2\n", st.IP6, st.Prefix6)
		if st.Gateway6 != "" {
			fmt.Fprintf(&b, "ip -6 route replace default via %s dev \"$IF\" 2>/dev/null || true\n", st.Gateway6)
		}
	}
	if !persist {
		b.WriteString("echo 'persisted: no (applied now)'\n")
		return b.String()
	}
	b.WriteString("if [ -d /etc/cloud ]; then mkdir -p /etc/cloud/cloud.cfg.d; echo 'network: {config: disabled}' > /etc/cloud/cloud.cfg.d/99-p8s-network.cfg; fi\n")
	// netplan (Ubuntu, Debian cloud images)
	np := NetworkConfig(st)
	np = strings.Replace(np, "    set-name: eth0\n", "    set-name: IFNAME\n", 1)
	b.WriteString("if [ -d /etc/netplan ] && command -v netplan >/dev/null 2>&1; then\n")
	b.WriteString("  rm -f /etc/netplan/50-cloud-init.yaml\n")
	fmt.Fprintf(&b, "  cat > /etc/netplan/60-p8s.yaml <<'P8S_EOF'\nnetwork:\n%sP8S_EOF\n", indent(np, "  "))
	b.WriteString("  sed -i \"s/IFNAME/$IF/\" /etc/netplan/60-p8s.yaml; chmod 600 /etc/netplan/60-p8s.yaml\n")
	b.WriteString("  netplan generate >/dev/null 2>&1 || true\n  echo 'persisted: netplan'\n")
	// systemd-networkd
	nd := fmt.Sprintf("[Match]\nMACAddress=%s\n\n[Network]\n", mac)
	for _, a := range addrs {
		nd += "Address=" + a + "\n"
	}
	for _, d := range st.DNS {
		nd += "DNS=" + d + "\n"
	}
	nd += "\n[Route]\nGateway=" + st.Gateway + "\n"
	if st.OnLink {
		nd += "GatewayOnLink=yes\n"
	}
	if st.IP6 != "" && st.Gateway6 != "" {
		nd += "\n[Route]\nGateway=" + st.Gateway6 + "\n"
	}
	b.WriteString("elif [ -d /etc/systemd/network ] && systemctl is-enabled systemd-networkd >/dev/null 2>&1; then\n")
	b.WriteString("  rm -f /etc/systemd/network/*cloud-init*.network\n")
	fmt.Fprintf(&b, "  cat > /etc/systemd/network/10-p8s.network <<'P8S_EOF'\n%sP8S_EOF\n", nd)
	b.WriteString("  echo 'persisted: systemd-networkd'\n")
	// ifupdown (Debian without netplan, Alpine)
	ifu := "auto IFNAME\niface IFNAME inet static\n"
	ifu += fmt.Sprintf("\taddress %s/%d\n", st.IP, st.Prefix)
	if st.OnLink {
		ifu += fmt.Sprintf("\tpost-up ip route add %s dev IFNAME\n\tpost-up ip route add default via %s dev IFNAME\n", st.Gateway, st.Gateway)
	} else {
		ifu += "\tgateway " + st.Gateway + "\n"
	}
	for _, x := range st.Extra {
		ifu += fmt.Sprintf("\tup ip addr add %s/%d dev IFNAME\n", x, st.Prefix)
	}
	if len(st.DNS) > 0 {
		ifu += "\tdns-nameservers " + strings.Join(st.DNS, " ") + "\n"
	}
	if st.IP6 != "" {
		ifu += fmt.Sprintf("\niface IFNAME inet6 static\n\taddress %s/%d\n", st.IP6, st.Prefix6)
		if st.Gateway6 != "" {
			ifu += "\tgateway " + st.Gateway6 + "\n"
		}
	}
	b.WriteString("elif [ -f /etc/network/interfaces ]; then\n")
	b.WriteString("  mkdir -p /etc/network/interfaces.d; rm -f /etc/network/interfaces.d/50-cloud-init*\n")
	b.WriteString("  sed -i \"/^auto $IF\\$/d; /^iface $IF /,/^\\$/d\" /etc/network/interfaces\n")
	b.WriteString("  grep -q 'interfaces.d' /etc/network/interfaces || echo 'source /etc/network/interfaces.d/*' >> /etc/network/interfaces\n")
	fmt.Fprintf(&b, "  cat > /etc/network/interfaces.d/60-p8s <<'P8S_EOF'\n%sP8S_EOF\n", ifu)
	b.WriteString("  sed -i \"s/IFNAME/$IF/g\" /etc/network/interfaces.d/60-p8s\n  echo 'persisted: ifupdown'\n")
	b.WriteString("else\n  echo 'persisted: no (unknown network setup: set the address in the system settings)'\nfi\n")
	return b.String()
}

func indent(s, pre string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pre + l
	}
	return strings.Join(lines, "\n") + "\n"
}
