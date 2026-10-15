package ct

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"p8sos/internal/cloudinit"
	"p8sos/internal/vm"
)

// prepare writes hostname, root password and SSH keys straight into the
// container's files before its first start. Unlike cloud-init this works for
// every distro.
func (s *Service) prepare(name string, keys []string, password string) error {
	root := s.rootfs(name)
	// hostname: the images ship a placeholder (LXC_NAME)
	if err := s.writeAs(name, "etc/hostname", []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	if b, err := s.readIn(root, "etc/hosts"); err == nil {
		h := strings.ReplaceAll(string(b), "LXC_NAME", name)
		if !strings.Contains(h, name) {
			h += "127.0.1.1\t" + name + "\n"
		}
		if err := s.rewrite(root, "etc/hosts", []byte(h)); err != nil {
			return err
		}
	}
	if password != "" {
		if err := s.setShadow(root, "root", cloudinit.HashPassword(password)); err != nil {
			return fmt.Errorf("root password: %w", err)
		}
	}
	if len(keys) > 0 {
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(strings.TrimSpace(k) + "\n")
		}
		if err := s.writeAs(name, "root/.ssh/authorized_keys", []byte(b.String()), 0o600); err != nil {
			return err
		}
		_ = os.Chmod(filepath.Join(root, "root/.ssh"), 0o700)
	}
	return nil
}

// readIn reads a file inside the container, refusing symlinks.
func (s *Service) readIn(root, rel string) ([]byte, error) {
	p := filepath.Join(root, rel)
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return os.ReadFile(p)
}

// rewrite replaces a file's content keeping its owner and mode.
func (s *Service) rewrite(root, rel string, data []byte) error {
	p := filepath.Join(root, rel)
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", rel)
	}
	tmp := p + ".p8s-tmp"
	if err := os.WriteFile(tmp, data, st.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Chmod(tmp, st.Mode().Perm())
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		if err := s.chown(tmp, int(sys.Uid), int(sys.Gid)); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, p)
}

// setShadow sets a user's password hash in /etc/shadow.
func (s *Service) setShadow(root, user, hash string) error {
	b, err := s.readIn(root, "etc/shadow")
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	found := false
	for i, l := range lines {
		f := strings.Split(l, ":")
		if len(f) >= 2 && f[0] == user {
			f[1] = hash
			if len(f) >= 3 {
				f[2] = fmt.Sprint(time.Now().Unix() / 86400) // last change: today (no forced change)
			}
			lines[i] = strings.Join(f, ":")
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no %s in /etc/shadow", user)
	}
	return s.rewrite(root, "etc/shadow", []byte(strings.Join(lines, "\n")))
}

// sshScript installs and starts an SSH server with whatever package manager
// the container has (Debian/Ubuntu, Rocky/Alma/Fedora, Alpine, Arch).
const sshScript = `
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
i=0; while [ $i -lt 60 ]; do ip route 2>/dev/null | grep -q default && break; i=$((i+1)); sleep 1; done
sleep 2
if command -v apt-get >/dev/null; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q && apt-get install -y -q openssh-server || exit 1; svc=ssh
elif command -v dnf >/dev/null; then dnf install -y -q openssh-server || exit 1; svc=sshd
elif command -v yum >/dev/null; then yum install -y -q openssh-server || exit 1; svc=sshd
elif command -v apk >/dev/null; then apk add --no-cache openssh || exit 1; rc-update add sshd default 2>/dev/null; svc=sshd
elif command -v pacman >/dev/null; then pacman -Sy --noconfirm openssh || exit 1; svc=sshd
else echo "no known package manager"; exit 1; fi
ssh-keygen -A >/dev/null 2>&1
if [ "$ROOT_LOGIN" = 1 ]; then
  if grep -qi '^Include /etc/ssh/sshd_config.d' /etc/ssh/sshd_config 2>/dev/null; then
    mkdir -p /etc/ssh/sshd_config.d
    printf 'PermitRootLogin yes\nPasswordAuthentication yes\n' > /etc/ssh/sshd_config.d/10-p8s-root.conf
  else
    sed -i '/^PermitRootLogin/d;/^PasswordAuthentication/d' /etc/ssh/sshd_config
    printf '\nPermitRootLogin yes\nPasswordAuthentication yes\n' >> /etc/ssh/sshd_config
  fi
fi
if command -v systemctl >/dev/null && [ -d /run/systemd/system ]; then
  systemctl enable $svc >/dev/null 2>&1; systemctl restart $svc
else
  rc-service sshd restart 2>/dev/null || /usr/sbin/sshd
fi
echo "SSH server ready"
`

// SetupSSH installs and starts the SSH server inside a running container
// (done by itself on the first start; the dashboard can run it again).
// It returns the output.
func (s *Service) SetupSSH(ctx context.Context, name string) (string, error) {
	m, err := s.get(name)
	if err != nil {
		return "", err
	}
	if s.state(ctx, name) != "running" {
		return "", fmt.Errorf("%w: start the container first", vm.ErrConflict)
	}
	rl := "0"
	if m.RootLogin {
		rl = "1"
	}
	out, err := s.Run.Run(ctx, "lxc-attach", "-n", name, "--clear-env", "--set-var", "ROOT_LOGIN="+rl,
		"--set-var", "HOME=/root", "--", "/bin/sh", "-c", sshScript+" 2>&1")
	_ = os.WriteFile(filepath.Join(s.confDir(name), "setup.log"), out, 0o640)
	if err != nil {
		return string(out), fmt.Errorf("installing the SSH server failed (does the container reach the internet?): %v", err)
	}
	if cur, err := s.get(name); err == nil && cur.NeedsSSH {
		cur.NeedsSSH = false
		_ = s.put(name, &cur)
	}
	return string(out), nil
}

// Wait waits for background work (first-boot setup) to finish.
func (s *Service) Wait() { s.bg.Wait() }

// firstBoot installs SSH in the background after the first start.
func (s *Service) firstBoot(name string) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		_, _ = s.SetupSSH(ctx, name)
	}()
}
