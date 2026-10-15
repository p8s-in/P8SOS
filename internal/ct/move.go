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

// Moving a container to another cluster server: a header line (its
// settings) and a copy of its disk (zfs send). The disk arrives as a dataset
// of its own, so the other server needs no template.
//
// A running container moves in two rounds: PhasePre copies the whole disk
// while it keeps running (the other server keeps it unmounted, no container
// yet); then it is shut down and PhaseFinal sends only what changed since,
// and the other server makes the container. It is off only for the shutdown
// and that last change.

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
	// Phase: "" = the whole move in one stream, PhasePre / PhaseFinal = the two rounds.
	Phase string `json:"phase,omitempty"`
}

// The rounds of moving a running container.
const (
	PhasePre   = "pre"
	PhaseFinal = "final"
)

const (
	moveSnap    = "p8s-move"
	movePreSnap = "p8s-move-pre"
)

func moveHeader(name string, m meta) MoveHeader {
	return MoveHeader{Name: name, Template: m.Template, VCPUs: m.VCPUs, MemoryMiB: m.MemoryMiB, DiskGB: m.DiskGB, MAC: m.MAC,
		Network: m.Network, Owner: m.Owner, Autostart: m.Autostart, Created: m.Created, NeedsSSH: m.NeedsSSH, RootLogin: m.RootLogin}
}

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

// LiveMoveCheck says whether a container can move while it runs ("" = yes).
func (s *Service) LiveMoveCheck(ctx context.Context, name string) (string, error) {
	m, err := s.get(name)
	if err != nil {
		return "", err
	}
	if m.Network == vm.NetPublic {
		return "it has a public IP from this server's pool; it can't take it along", nil
	}
	return "", nil
}

// Export streams a stopped container (header line + disk) to w.
func (s *Service) Export(ctx context.Context, name string, w io.Writer) error {
	return s.ExportPhase(ctx, name, "", w)
}

// ExportPhase streams one round of a move: "" all of a stopped container,
// PhasePre its whole disk while it may run, PhaseFinal (stopped) the change
// since PhasePre.
func (s *Service) ExportPhase(ctx context.Context, name, phase string, w io.Writer) error {
	check := s.MoveCheck
	switch phase {
	case "", PhaseFinal:
	case PhasePre:
		check = s.LiveMoveCheck
	default:
		return fmt.Errorf("%w: phase must be %q or %q", vm.ErrInvalid, PhasePre, PhaseFinal)
	}
	why, err := check(ctx, name)
	if err != nil {
		return err
	}
	if why != "" {
		return fmt.Errorf("%w: %s can't move: %s", vm.ErrConflict, name, why)
	}
	m, _ := s.get(name)
	h := moveHeader(name, m)
	h.Phase = phase
	b, _ := json.Marshal(h)
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	ds, bg := s.dataset(name), context.WithoutCancel(ctx)
	snap := ds + "@" + moveSnap
	if phase == PhasePre {
		snap = ds + "@" + movePreSnap
	}
	_, _ = s.zfs(ctx, "destroy", snap)
	if _, err := s.zfs(ctx, "snapshot", snap); err != nil {
		return fmt.Errorf("snapshot for the move: %w", err)
	}
	args := []string{"send", "-c"}
	switch phase {
	case PhasePre: // kept: the final round sends what changed since
	case PhaseFinal:
		args = append(args, "-i", ds+"@"+movePreSnap)
		defer s.zfs(bg, "destroy", snap)
		defer s.zfs(bg, "destroy", ds+"@"+movePreSnap)
	default:
		defer s.zfs(bg, "destroy", snap)
	}
	c := s.zfsCmd(ctx, append(args, snap)...)
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
	ds := s.dataset(h.Name)
	switch h.Phase {
	case PhasePre:
		return Container{}, "", s.importPre(ctx, h, br)
	case PhaseFinal:
		return s.importFinal(ctx, h, br)
	case "":
	default:
		return Container{}, "", fmt.Errorf("%w: unknown move phase %q", vm.ErrInvalid, h.Phase)
	}
	_, _ = s.zfs(ctx, "create", "-p", "-o", "canmount=off", s.Pool+"/p8s/ct")
	c := s.zfsCmd(ctx, "receive", "-o", "mountpoint="+s.rootfs(h.Name), "-o", fmt.Sprintf("refquota=%dG", h.DiskGB), ds)
	var stderr strings.Builder
	c.Stdin, c.Stderr = br, &stderr
	if err := c.Run(); err != nil {
		_, _ = s.zfs(context.WithoutCancel(ctx), "destroy", "-r", ds)
		return Container{}, "", fmt.Errorf("zfs receive: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return s.finishImport(ctx, h)
}

// importPre keeps the first round of a running container's move: its disk,
// unmounted, no container yet.
func (s *Service) importPre(ctx context.Context, h MoveHeader, r io.Reader) error {
	ds := s.dataset(h.Name)
	// a disk without a container here is left from an earlier move that stopped halfway
	if _, err := s.zfs(ctx, "destroy", "-r", ds); err != nil && !notExist(err) {
		return fmt.Errorf("remove a leftover disk %s: %w", ds, err)
	}
	_, _ = s.zfs(ctx, "create", "-p", "-o", "canmount=off", s.Pool+"/p8s/ct")
	c := s.zfsCmd(ctx, "receive", "-u", "-o", "mountpoint="+s.rootfs(h.Name), "-o", fmt.Sprintf("refquota=%dG", h.DiskGB), ds)
	var stderr strings.Builder
	c.Stdin, c.Stderr = r, &stderr
	if err := c.Run(); err != nil {
		_, _ = s.zfs(context.WithoutCancel(ctx), "destroy", "-r", ds)
		return fmt.Errorf("zfs receive: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// importFinal applies the last round (the change since importPre) and makes the container.
func (s *Service) importFinal(ctx context.Context, h MoveHeader, r io.Reader) (Container, string, error) {
	ds := s.dataset(h.Name)
	c := s.zfsCmd(ctx, "receive", "-u", "-F", ds)
	var stderr strings.Builder
	c.Stdin, c.Stderr = r, &stderr
	if err := c.Run(); err != nil {
		return Container{}, "", fmt.Errorf("zfs receive (last changes): %v %s", err, strings.TrimSpace(stderr.String()))
	}
	_, _ = s.zfs(ctx, "destroy", ds+"@"+movePreSnap)
	return s.finishImport(ctx, h)
}

// DropMove deletes the disk of a move that stopped halfway (no container here yet).
func (s *Service) DropMove(ctx context.Context, name string) error {
	if !reName.MatchString(name) {
		return fmt.Errorf("%w: bad name", vm.ErrInvalid)
	}
	if s.Exists(name) {
		return fmt.Errorf("%w: container %s is on this server", vm.ErrConflict, name)
	}
	if _, err := s.zfs(ctx, "destroy", "-r", s.dataset(name)); err != nil && !notExist(err) {
		return err
	}
	return nil
}

// finishImport makes the container from its received disk.
func (s *Service) finishImport(ctx context.Context, h MoveHeader) (Container, string, error) {
	ds := s.dataset(h.Name)
	note := ""
	network := h.Network
	if _, err := s.bridgeFor(h.Name, network); err != nil {
		network, note = vm.NetNAT, fmt.Sprintf("; its network %s isn't available here, so it is on the private network", h.Network)
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
