package ct

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"p8sos/internal/vm"
)

// Moving a stopped container to another cluster server: a header line (its
// settings) and a full copy of its disk (zfs send). The disk arrives as a
// dataset of its own, so the other server needs no template.

// MoveHeader describes a container in a move stream.
type MoveHeader struct {
	Name      string `json:"name"`
	Template  string `json:"template"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memory_mib"`
	DiskGB    int    `json:"disk_gb"`
	MAC       string `json:"mac"`
	Network   string `json:"network"`
	Owner     string `json:"owner,omitempty"`
	Autostart bool   `json:"autostart"`
	Created   string `json:"created"`
	NeedsSSH  bool   `json:"needs_ssh,omitempty"`
	RootLogin bool   `json:"root_login,omitempty"`
}

const moveSnap = "p8s-move"

func (s *Service) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if s.Cmd != nil {
		return s.Cmd(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// zfsCmd is a streaming zfs command in the host's mount namespace.
func (s *Service) zfsCmd(ctx context.Context, args ...string) *exec.Cmd {
	return s.cmd(ctx, "nsenter", append([]string{"--mount=/proc/1/ns/mnt", "--", "zfs"}, args...)...)
}

// MoveCheck says whether a container can move now ("" = yes).
func (s *Service) MoveCheck(ctx context.Context, name string) (string, error) {
	m, err := s.get(name)
	if err != nil {
		return "", err
	}
	switch {
	case s.state(ctx, name) != "stopped":
		return "shut it down first", nil
	case m.Network == vm.NetPublic:
		return "it has a public IP from this server's pool; it can't take it along", nil
	}
	return "", nil
}

// Export streams a stopped container (header line + disk) to w.
func (s *Service) Export(ctx context.Context, name string, w io.Writer) error {
	why, err := s.MoveCheck(ctx, name)
	if err != nil {
		return err
	}
	if why != "" {
		return fmt.Errorf("%w: %s can't move: %s", vm.ErrConflict, name, why)
	}
	m, _ := s.get(name)
	h := MoveHeader{Name: name, Template: m.Template, VCPUs: m.VCPUs, MemoryMiB: m.MemoryMiB, DiskGB: m.DiskGB, MAC: m.MAC,
		Network: m.Network, Owner: m.Owner, Autostart: m.Autostart, Created: m.Created, NeedsSSH: m.NeedsSSH, RootLogin: m.RootLogin}
	b, _ := json.Marshal(h)
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	snap := s.dataset(name) + "@" + moveSnap
	_, _ = s.zfs(ctx, "destroy", snap)
	if _, err := s.zfs(ctx, "snapshot", snap); err != nil {
		return fmt.Errorf("snapshot for the move: %w", err)
	}
	defer s.zfs(context.WithoutCancel(ctx), "destroy", snap)
	c := s.zfsCmd(ctx, "send", "-c", snap)
	var stderr strings.Builder
	c.Stdout, c.Stderr = w, &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("zfs send: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Import makes a container from an Export stream (left stopped); the note
// says what changed (a network this server doesn't have).
func (s *Service) Import(ctx context.Context, r io.Reader) (Container, string, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	line, err := br.ReadBytes('\n')
	if err != nil || len(line) > 1<<16 {
		return Container{}, "", fmt.Errorf("%w: no container header in the stream", vm.ErrInvalid)
	}
	var h MoveHeader
	if err := json.Unmarshal(line, &h); err != nil || !reName.MatchString(h.Name) || h.VCPUs < 1 || h.MemoryMiB < 64 || h.DiskGB < 1 {
		return Container{}, "", fmt.Errorf("%w: bad container header", vm.ErrInvalid)
	}
	if s.Exists(h.Name) {
		return Container{}, "", fmt.Errorf("%w: a container called %q is already on this server", vm.ErrExists, h.Name)
	}
	note := ""
	network := h.Network
	if _, err := s.bridgeFor(h.Name, network); err != nil {
		network, note = vm.NetNAT, fmt.Sprintf("; its network %s isn't available here, so it is on the private network", h.Network)
	}
	ds := s.dataset(h.Name)
	_, _ = s.zfs(ctx, "create", "-p", "-o", "canmount=off", s.Pool+"/p8s/ct")
	c := s.zfsCmd(ctx, "receive", "-o", "mountpoint="+s.rootfs(h.Name), "-o", fmt.Sprintf("refquota=%dG", h.DiskGB), ds)
	var stderr strings.Builder
	c.Stdin, c.Stderr = br, &stderr
	if err := c.Run(); err != nil {
		_, _ = s.zfs(context.WithoutCancel(ctx), "destroy", "-r", ds)
		return Container{}, "", fmt.Errorf("zfs receive: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	undo := func() {
		bg := context.WithoutCancel(ctx)
		_, _ = s.zfs(bg, "destroy", "-r", ds)
		_ = os.RemoveAll(s.confDir(h.Name))
	}
	_, _ = s.zfs(ctx, "destroy", ds+"@"+moveSnap)
	_, _ = s.zfs(ctx, "mount", ds)
	m := &meta{Template: h.Template, VCPUs: h.VCPUs, MemoryMiB: h.MemoryMiB, DiskGB: h.DiskGB, MAC: h.MAC, Network: network,
		Owner: h.Owner, Autostart: h.Autostart, Created: h.Created, NeedsSSH: h.NeedsSSH, RootLogin: h.RootLogin}
	bridge, err := s.bridgeFor(h.Name, network)
	if err != nil {
		undo()
		return Container{}, "", err
	}
	if err := s.writeConfig(h.Name, m, bridge); err != nil {
		undo()
		return Container{}, "", err
	}
	if err := s.put(h.Name, m); err != nil {
		undo()
		return Container{}, "", err
	}
	if s.Refresh != nil {
		_ = s.Refresh(ctx)
	}
	return s.view(ctx, h.Name, *m), note, nil
}
