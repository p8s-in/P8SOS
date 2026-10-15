package ct

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"p8sos/internal/vm"
)

// Copies of containers for high availability (like vm/replica.go for VMs).
// The container's disk gets a p8s-rep-<time> snapshot every few minutes and
// the change since the last one goes to the other server (zfs send -i), where
// it is kept as <pool>/p8s/ct-replica/NAME (not mounted) with the container's
// settings in data_dir/ct-replicas/NAME.json. If the container's server dies,
// the copy becomes the container there (PromoteReplica); when the dead server
// comes back, its old container becomes the copy (RetireCopy).

// RepSnapPrefix names the copy snapshots.
const RepSnapPrefix = "p8s-rep-"

// ReplicaHeader is the first line of a copy stream.
type ReplicaHeader struct {
	MoveHeader
	Base string `json:"base,omitempty"` // "" = full copy
	Snap string `json:"snap"`
	From string `json:"from,omitempty"` // server name it came from
}

func (s *Service) repDir() string { return filepath.Join(filepath.Dir(s.MetaFile), "ct-replicas") }
func (s *Service) repFile(name string) string {
	return filepath.Join(s.repDir(), name+".json")
}
func (s *Service) repRoot() string               { return s.Pool + "/p8s/ct-replica" }
func (s *Service) repDataset(name string) string { return s.repRoot() + "/" + name }

func notExist(err error) bool { return err != nil && strings.Contains(err.Error(), "does not exist") }

// repSnaps are the copy snapshots of a dataset, oldest first.
func (s *Service) repSnaps(ctx context.Context, ds string) ([]string, error) {
	out, err := s.Run.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-s", "creation", "-d", "1", ds)
	if err != nil {
		return nil, err
	}
	var l []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, snap, ok := strings.Cut(strings.TrimSpace(line), "@"); ok && strings.HasPrefix(snap, RepSnapPrefix) {
			l = append(l, snap)
		}
	}
	return l, nil
}

// dropRepSnapsAfter deletes the copy snapshots taken after snapshot snap.
func (s *Service) dropRepSnapsAfter(ctx context.Context, name, snap string) {
	out, err := s.Run.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-s", "creation", "-d", "1", s.dataset(name))
	if err != nil {
		return
	}
	after := false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		_, sn, ok := strings.Cut(strings.TrimSpace(line), "@")
		switch {
		case !ok:
		case sn == snap:
			after = true
		case after && strings.HasPrefix(sn, RepSnapPrefix):
			_, _ = s.zfs(ctx, "destroy", s.dataset(name)+"@"+sn)
		}
	}
}

// HACheck says whether a container can be protected with copies ("" = yes).
func (s *Service) HACheck(ctx context.Context, name string) (string, error) {
	m, err := s.get(name)
	if err != nil {
		return "", err
	}
	if m.Network == vm.NetPublic {
		return "it has a public IP from this server's pool (it can't follow the container to another server)", nil
	}
	return "", nil
}

// ReplicaSnaps are the copy snapshots of a container held here (oldest first).
func (s *Service) ReplicaSnaps(ctx context.Context, name string) ([]string, error) {
	if !reName.MatchString(name) {
		return nil, fmt.Errorf("%w: bad name", vm.ErrInvalid)
	}
	l, err := s.repSnaps(ctx, s.repDataset(name))
	if notExist(err) {
		return []string{}, nil
	}
	if l == nil {
		l = []string{}
	}
	return l, err
}

// SendReplica writes a copy of the container (header line + the disk's
// change since the newest snapshot in have that this server has too) to w.
// It works while the container runs.
func (s *Service) SendReplica(ctx context.Context, name string, have []string, from string, w io.Writer) error {
	if why, err := s.HACheck(ctx, name); err != nil {
		return err
	} else if why != "" {
		return fmt.Errorf("%w: %s can't be copied: %s", vm.ErrConflict, name, why)
	}
	m, _ := s.get(name)
	ds := s.dataset(name)
	mine, err := s.repSnaps(ctx, ds)
	if err != nil {
		return err
	}
	base := ""
	for _, x := range mine { // oldest first: the newest in common wins
		for _, o := range have {
			if x == o {
				base = x
			}
		}
	}
	snap := RepSnapPrefix + strconv.FormatInt(s.now().UnixNano(), 10)
	if _, err := s.zfs(ctx, "snapshot", ds+"@"+snap); err != nil {
		return fmt.Errorf("snapshot for the copy: %w", err)
	}
	bg := context.WithoutCancel(ctx)
	b, _ := json.Marshal(ReplicaHeader{MoveHeader: moveHeader(name, m), Base: base, Snap: snap, From: from})
	if _, err := w.Write(append(b, '\n')); err != nil {
		_, _ = s.zfs(bg, "destroy", ds+"@"+snap)
		return err
	}
	args := []string{"send", "-c"}
	if base != "" {
		args = append(args, "-i", ds+"@"+base)
	}
	c := s.zfsCmd(ctx, append(args, ds+"@"+snap)...)
	var stderr strings.Builder
	c.Stdout, c.Stderr = w, &stderr
	if err := c.Run(); err != nil {
		_, _ = s.zfs(bg, "destroy", ds+"@"+snap)
		return fmt.Errorf("zfs send: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	// keep the base (in case this copy didn't arrive) and the new one
	for _, x := range mine {
		if x != base {
			_, _ = s.zfs(bg, "destroy", ds+"@"+x)
		}
	}
	return nil
}

// RecvReplica stores a copy sent by SendReplica.
func (s *Service) RecvReplica(ctx context.Context, r io.Reader) (vm.Replica, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	line, err := br.ReadBytes('\n')
	if err != nil || len(line) > 1<<16 {
		return vm.Replica{}, fmt.Errorf("%w: no copy header in the stream", vm.ErrInvalid)
	}
	var h ReplicaHeader
	if err := json.Unmarshal(line, &h); err != nil || !reName.MatchString(h.Name) || !strings.HasPrefix(h.Snap, RepSnapPrefix) ||
		(h.Base != "" && !strings.HasPrefix(h.Base, RepSnapPrefix)) {
		return vm.Replica{}, fmt.Errorf("%w: bad copy header", vm.ErrInvalid)
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.Exists(h.Name) {
		return vm.Replica{}, fmt.Errorf("%w: container %s runs on this server; it can't also keep a copy of it", vm.ErrExists, h.Name)
	}
	ds := s.repDataset(h.Name)
	_, _ = s.zfs(ctx, "create", "-p", "-o", "canmount=off", s.repRoot())
	if h.Base == "" { // a full copy replaces what is here
		if _, err := s.zfs(ctx, "destroy", "-r", ds); err != nil && !notExist(err) {
			return vm.Replica{}, fmt.Errorf("remove the old copy: %w", err)
		}
	}
	args := []string{"receive", "-u", "-o", "canmount=off"}
	if h.Base != "" {
		args = append(args, "-F") // undo anything that touched the copy since
	}
	c := s.zfsCmd(ctx, append(args, ds)...)
	var stderr strings.Builder
	c.Stdin, c.Stderr = br, &stderr
	if err := c.Run(); err != nil {
		return vm.Replica{}, fmt.Errorf("zfs receive: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	if old, err := s.repSnaps(ctx, ds); err == nil { // only the newest is needed
		for _, x := range old {
			if x != h.Snap {
				_, _ = s.zfs(ctx, "destroy", ds+"@"+x)
			}
		}
	}
	if err := s.saveRep(h); err != nil {
		return vm.Replica{}, err
	}
	return s.repInfo(ctx, h), nil
}

func (s *Service) saveRep(h ReplicaHeader) error {
	if err := os.MkdirAll(s.repDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(h)
	tmp := s.repFile(h.Name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.repFile(h.Name))
}

func (s *Service) loadRep(name string) (ReplicaHeader, error) {
	var h ReplicaHeader
	if !reName.MatchString(name) {
		return h, fmt.Errorf("%w: bad name", vm.ErrInvalid)
	}
	b, err := os.ReadFile(s.repFile(name))
	if errors.Is(err, os.ErrNotExist) {
		return h, fmt.Errorf("%w: no copy of container %q on this server", vm.ErrNotFound, name)
	} else if err != nil {
		return h, err
	}
	return h, json.Unmarshal(b, &h)
}

func (s *Service) repInfo(ctx context.Context, h ReplicaHeader) vm.Replica {
	r := vm.Replica{Kind: "ct", Name: h.Name, From: h.From, VCPUs: h.VCPUs, MemoryMiB: h.MemoryMiB, DiskGB: h.DiskGB,
		OSType: h.Template, Owner: h.Owner}
	ds := s.repDataset(h.Name)
	if out, err := s.Run.Run(ctx, "zfs", "get", "-Hp", "-o", "value", "used", ds); err == nil {
		r.Bytes, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	}
	if snaps, _ := s.repSnaps(ctx, ds); len(snaps) > 0 {
		if t := vm.SnapTime(snaps[len(snaps)-1]); !t.IsZero() {
			r.At = t.Format(time.RFC3339)
		}
	}
	return r
}

func (s *Service) hasRep(ctx context.Context, name string) bool {
	_, err := s.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", s.repDataset(name))
	return err == nil
}

// Replicas lists the container copies held on this server.
func (s *Service) Replicas(ctx context.Context) []vm.Replica {
	out := []vm.Replica{}
	files, _ := filepath.Glob(filepath.Join(s.repDir(), "*.json"))
	sort.Strings(files)
	for _, f := range files {
		h, err := s.loadRep(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil || !s.hasRep(ctx, h.Name) {
			continue
		}
		out = append(out, s.repInfo(ctx, h))
	}
	return out
}

// PromoteReplica makes the container from its copy here (left stopped, no
// autostart). Only do this when it doesn't run anywhere else.
func (s *Service) PromoteReplica(ctx context.Context, name string) (Container, string, error) {
	h, err := s.loadRep(name)
	if err != nil {
		return Container{}, "", err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.Exists(name) {
		return Container{}, "", fmt.Errorf("%w: container %s is already on this server", vm.ErrExists, name)
	}
	if !s.hasRep(ctx, name) {
		return Container{}, "", fmt.Errorf("%w: the copy of container %q is missing", vm.ErrNotFound, name)
	}
	ds, rep := s.dataset(name), s.repDataset(name)
	if _, err := s.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", ds); err == nil {
		return Container{}, "", fmt.Errorf("%w: a disk %s is already here (left from an earlier container?); delete it first", vm.ErrConflict, ds)
	}
	_, _ = s.zfs(ctx, "create", "-p", "-o", "canmount=off", s.Pool+"/p8s/ct")
	if _, err := s.zfs(ctx, "rename", rep, ds); err != nil {
		return Container{}, "", fmt.Errorf("take the copy: %w", err)
	}
	bg := context.WithoutCancel(ctx)
	undo := func() {
		_, _ = s.zfs(bg, "unmount", ds)
		_, _ = s.zfs(bg, "set", "canmount=off", ds)
		_, _ = s.zfs(bg, "rename", ds, rep)
		_ = os.RemoveAll(s.confDir(name))
		_ = s.put(name, nil)
	}
	if _, err := s.zfs(ctx, "set", "mountpoint="+s.rootfs(name), "canmount=on", fmt.Sprintf("refquota=%dG", h.DiskGB), ds); err != nil {
		undo()
		return Container{}, "", err
	}
	if _, err := s.zfs(ctx, "mount", ds); err != nil && !strings.Contains(err.Error(), "already mounted") {
		undo()
		return Container{}, "", fmt.Errorf("mount the copy: %w", err)
	}
	note := ""
	network := h.Network
	if _, err := s.bridgeFor(name, network); err != nil {
		network, note = vm.NetNAT, fmt.Sprintf("; its network %s isn't available here, so it is on the private network", h.Network)
	}
	m := &meta{Template: h.Template, VCPUs: h.VCPUs, MemoryMiB: h.MemoryMiB, DiskGB: h.DiskGB, MAC: h.MAC, Network: network,
		Owner: h.Owner, Autostart: false, Created: h.Created, NeedsSSH: h.NeedsSSH, RootLogin: h.RootLogin}
	br, err := s.bridgeFor(name, network)
	if err != nil {
		undo()
		return Container{}, "", err
	}
	if err := s.writeConfig(name, m, br); err != nil {
		undo()
		return Container{}, "", err
	}
	if err := s.put(name, m); err != nil {
		undo()
		return Container{}, "", err
	}
	_ = os.Remove(s.repFile(name))
	if s.Refresh != nil {
		_ = s.Refresh(ctx)
	}
	if t := vm.SnapTime(h.Snap); !t.IsZero() {
		note = fmt.Sprintf("; from the copy of %s", t.Local().Format("2 Jan 15:04:05")) + note
	}
	return s.view(ctx, name, *m), note, nil
}

// DropReplica deletes the copy of a container held here.
func (s *Service) DropReplica(ctx context.Context, name string) error {
	if !reName.MatchString(name) {
		return fmt.Errorf("%w: bad name", vm.ErrInvalid)
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if _, err := s.zfs(ctx, "destroy", "-r", s.repDataset(name)); err != nil && !notExist(err) {
		return err
	}
	if err := os.Remove(s.repFile(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RetireCopy switches off a container that now runs on another server
// (after a failover) and keeps its disk as the copy. Missing: nothing to do.
func (s *Service) RetireCopy(ctx context.Context, name, from string) error {
	m, err := s.get(name)
	if errors.Is(err, vm.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if st := s.state(ctx, name); st != "stopped" && st != "unknown" {
		if _, err := s.Run.Run(ctx, "lxc-stop", "-n", name, "-k"); err != nil && s.state(ctx, name) == "running" {
			return fmt.Errorf("switch off the old container: %w", err)
		}
	}
	ds, rep := s.dataset(name), s.repDataset(name)
	if _, err := s.zfs(ctx, "destroy", "-r", rep); err != nil && !notExist(err) { // an older copy
		return err
	}
	_, _ = s.zfs(ctx, "create", "-p", "-o", "canmount=off", s.repRoot())
	if _, err := s.zfs(ctx, "unmount", ds); err != nil && !strings.Contains(err.Error(), "not currently mounted") {
		return fmt.Errorf("unmount the old container's disk: %w", err)
	}
	if _, err := s.zfs(ctx, "set", "canmount=off", ds); err != nil {
		return err
	}
	if _, err := s.zfs(ctx, "rename", ds, rep); err != nil {
		return fmt.Errorf("keep the old disk as the copy: %w", err)
	}
	_ = os.RemoveAll(s.confDir(name))
	if err := s.put(name, nil); err != nil {
		return err
	}
	return s.saveRep(ReplicaHeader{MoveHeader: moveHeader(name, m), From: from})
}

// Fence switches off (hard) those of the named containers that run here.
// A server cut off from the cluster does this, so a container never runs twice.
func (s *Service) Fence(ctx context.Context, names []string) []string {
	var out []string
	for _, n := range names {
		if !s.Exists(n) {
			continue
		}
		if st := s.state(ctx, n); st == "stopped" || st == "unknown" {
			continue
		}
		if _, err := s.Run.Run(ctx, "lxc-stop", "-n", n, "-k"); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// SetAutostart sets whether the container starts when the host boots.
func (s *Service) SetAutostart(ctx context.Context, name string, on bool) (Container, error) {
	return s.Resize(ctx, name, ResizeRequest{Autostart: &on})
}
