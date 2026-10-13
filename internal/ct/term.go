package ct

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"p8sos/internal/vm"
)

// Terminal is a shell inside a container on a pseudo-terminal.
type Terminal struct {
	PTY *os.File
	Cmd *exec.Cmd
}

// Resize sets the terminal size.
func (t *Terminal) Resize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return nil
	}
	ws := struct{ Row, Col, X, Y uint16 }{uint16(rows), uint16(cols), 0, 0}
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, t.PTY.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	if e != 0 {
		return e
	}
	return nil
}

// Close ends the shell.
func (t *Terminal) Close() {
	_ = t.PTY.Close()
	if t.Cmd.Process != nil {
		_ = t.Cmd.Process.Kill()
	}
	_ = t.Cmd.Wait()
}

func openPTY() (*os.File, *os.File, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		m.Close()
		return nil, nil, e
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		m.Close()
		return nil, nil, e
	}
	sl, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, sl, nil
}

// Shell opens a root login shell in a running container (bash if it has one).
func (s *Service) Shell(ctx context.Context, name string) (*Terminal, error) {
	if _, err := s.get(name); err != nil {
		return nil, err
	}
	if s.state(ctx, name) != "running" {
		return nil, fmt.Errorf("%w: start the container first", vm.ErrConflict)
	}
	cmd := exec.Command("lxc-attach", "-n", name, "--clear-env",
		"--set-var", "TERM=xterm-256color", "--set-var", "HOME=/root", "--set-var", "LANG=C.UTF-8",
		"--set-var", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--", "/bin/sh", "-c", "cd /root 2>/dev/null; if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi")
	return StartPTY(cmd)
}

// StartPTY runs cmd on a new pseudo-terminal (it becomes the session leader).
func StartPTY(cmd *exec.Cmd) (*Terminal, error) {
	m, sl, err := openPTY()
	if err != nil {
		return nil, err
	}
	defer sl.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = sl, sl, sl
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		m.Close()
		return nil, err
	}
	return &Terminal{PTY: m, Cmd: cmd}, nil
}

// HostShell opens the server's root shell for admins (like Proxmox's node
// Shell). It is a root login on the server itself, the same as at its
// keyboard; p8sd's own service sandbox doesn't apply to it.
func HostShell() (*Terminal, error) {
	cmd := exec.Command("nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--",
		"/usr/bin/env", "-i", "TERM=xterm-256color", "HOME=/root", "LANG=C.UTF-8", "USER=root", "LOGNAME=root", "SHELL=/bin/bash",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"/bin/sh", "-c", "cd /root 2>/dev/null; if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi")
	return StartPTY(cmd)
}
