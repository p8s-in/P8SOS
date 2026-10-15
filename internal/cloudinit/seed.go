// Package cloudinit builds NoCloud seed ISOs that configure a VM on first boot.
package cloudinit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"p8sos/internal/execx"
)

type Spec struct {
	InstanceID string
	Hostname   string
	SSHKeys    []string
	UserData   string // optional raw #cloud-config; replaces the generated one
	// RootPasswordHash ($6$ crypt) sets root's password; empty = root stays locked.
	RootPasswordHash string
	// RootSSHPassword also allows SSH logins as root with that password.
	RootSSHPassword bool
	// Static sets a fixed address (public IP from a pool) instead of DHCP.
	Static *Static
	// DHCPByMAC: DHCP with the network card's MAC as the client id. Clones
	// share the original's machine id, which DHCP would otherwise use, so
	// two VMs would ask for (and get) the same address.
	DHCPByMAC string
}

// DHCPConfig renders network config v2: DHCP on the card with this MAC, identified by the MAC.
func DHCPConfig(mac string) string {
	return fmt.Sprintf("version: 2\nethernets:\n  nic0:\n    match:\n      macaddress: %q\n    dhcp4: true\n    dhcp-identifier: mac\n", strings.ToLower(mac))
}

// Static is a fixed IPv4 configuration for the VM's network card.
type Static struct {
	MAC     string   // the VM's NIC, e.g. 52:54:00:12:34:56
	IP      string   // e.g. 203.0.113.10
	Prefix  int      // e.g. 29
	Gateway string   // e.g. 203.0.113.9
	DNS     []string // name servers
	OnLink  bool     // gateway outside the network (routed /32): on-link route
	Extra   []string // more IPv4 addresses (same prefix)
	// IPv6 address, prefix length and gateway ("" = none).
	IP6      string
	Prefix6  int
	Gateway6 string
}

// NetworkConfig renders cloud-init network config v2 for a static address.
func NetworkConfig(st Static) string {
	var b strings.Builder
	b.WriteString("version: 2\nethernets:\n  eth0:\n")
	fmt.Fprintf(&b, "    match:\n      macaddress: %q\n    set-name: eth0\n", strings.ToLower(st.MAC))
	addrs := []string{fmt.Sprintf("%q", fmt.Sprintf("%s/%d", st.IP, st.Prefix))}
	for _, x := range st.Extra {
		addrs = append(addrs, fmt.Sprintf("%q", fmt.Sprintf("%s/%d", x, st.Prefix)))
	}
	if st.IP6 != "" {
		addrs = append(addrs, fmt.Sprintf("%q", fmt.Sprintf("%s/%d", st.IP6, st.Prefix6)))
	}
	fmt.Fprintf(&b, "    addresses: [%s]\n", strings.Join(addrs, ", "))
	fmt.Fprintf(&b, "    routes:\n      - to: 0.0.0.0/0\n        via: %s\n", st.Gateway)
	if st.OnLink {
		b.WriteString("        on-link: true\n")
	}
	if st.IP6 != "" && st.Gateway6 != "" {
		fmt.Fprintf(&b, "      - to: \"::/0\"\n        via: %s\n", st.Gateway6)
	}
	if len(st.DNS) > 0 {
		q := make([]string, len(st.DNS))
		for i, d := range st.DNS {
			q[i] = fmt.Sprintf("%q", d)
		}
		fmt.Fprintf(&b, "    nameservers:\n      addresses: [%s]\n", strings.Join(q, ", "))
	}
	return b.String()
}

type Builder struct {
	Run execx.Runner
	Dir string // where seed ISOs live, e.g. /var/lib/p8s/seeds
}

func (b *Builder) Path(vm string) string { return filepath.Join(b.Dir, vm+"-seed.iso") }

// Build writes user-data/meta-data and packs them with cloud-localds.
func (b *Builder) Build(ctx context.Context, vm string, s Spec) (string, error) {
	// QEMU runs as libvirt-qemu and must be able to traverse this directory
	// (Chmod also repairs dirs created 0750 by older bootstrap scripts).
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(b.Dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(b.Dir, vm+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	ud := s.UserData
	if ud == "" {
		ud = UserData(s)
	}
	md := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", s.InstanceID, s.Hostname)
	udPath, mdPath := filepath.Join(tmp, "user-data"), filepath.Join(tmp, "meta-data")
	if err := os.WriteFile(udPath, []byte(ud), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(mdPath, []byte(md), 0o600); err != nil {
		return "", err
	}
	out := b.Path(vm)
	args := []string{out, udPath, mdPath}
	nc := ""
	switch {
	case s.Static != nil:
		nc = NetworkConfig(*s.Static)
	case s.DHCPByMAC != "":
		nc = DHCPConfig(s.DHCPByMAC)
	}
	if nc != "" {
		ncPath := filepath.Join(tmp, "network-config")
		if err := os.WriteFile(ncPath, []byte(nc), 0o600); err != nil {
			return "", err
		}
		args = append([]string{"--network-config=" + ncPath}, args...)
	}
	if _, err := b.Run.Run(ctx, "cloud-localds", args...); err != nil {
		return "", fmt.Errorf("build seed: %w", err)
	}
	return out, nil
}

func (b *Builder) Remove(vm string) error {
	err := os.Remove(b.Path(vm))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// UserData renders the default #cloud-config: hostname, SSH keys, qemu guest
// agent, and root filesystem growth to fill the resized disk.
func UserData(s Spec) string {
	var sb strings.Builder
	sb.WriteString("#cloud-config\n")
	fmt.Fprintf(&sb, "hostname: %s\n", s.Hostname)
	sb.WriteString("growpart:\n  mode: auto\n  devices: ['/']\nresize_rootfs: true\n")
	if len(s.SSHKeys) > 0 {
		sb.WriteString("ssh_authorized_keys:\n")
		for _, k := range s.SSHKeys {
			fmt.Fprintf(&sb, "  - %q\n", strings.TrimSpace(k))
		}
	}
	if s.RootPasswordHash != "" {
		// Like a VPS: root gets the chosen password (and the SSH keys).
		sb.WriteString("disable_root: false\n")
		fmt.Fprintf(&sb, "chpasswd:\n  expire: false\n  users:\n    - {name: root, password: %q, type: hash}\n", s.RootPasswordHash)
		if s.RootSSHPassword {
			sb.WriteString("ssh_pwauth: true\n")
			sb.WriteString("write_files:\n  - path: /etc/ssh/sshd_config.d/10-p8s-root.conf\n    permissions: '0644'\n" +
				"    content: |\n      # P8S OS: root may log in over SSH with its password (chosen when the VM was created). Listed first so it wins.\n" +
				"      PermitRootLogin yes\n      PasswordAuthentication yes\n")
		}
	}
	sb.WriteString("package_update: true\npackages:\n  - qemu-guest-agent\n")
	sb.WriteString("runcmd:\n  - [systemctl, enable, --now, qemu-guest-agent]\n")
	if s.RootSSHPassword && s.RootPasswordHash != "" {
		// Debian/Ubuntu call the service "ssh", RHEL-likes "sshd".
		sb.WriteString("  - [sh, -c, 'systemctl reload-or-restart ssh 2>/dev/null || systemctl reload-or-restart sshd']\n")
	}
	return sb.String()
}
