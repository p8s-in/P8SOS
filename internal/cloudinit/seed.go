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
	if _, err := b.Run.Run(ctx, "cloud-localds", out, udPath, mdPath); err != nil {
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
