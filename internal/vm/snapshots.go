package vm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"p8sos/internal/storage"
	"p8sos/internal/virt"
)

// Snapshot is a point-in-time copy of a VM's disk.
type Snapshot = storage.Snapshot

// SnapshotDisks is the snapshot/resize part of the disk backend (ZFS).
type SnapshotDisks interface {
	Snapshot(ctx context.Context, vm, name string) error
	Snapshots(ctx context.Context, vm string) ([]Snapshot, error)
	Rollback(ctx context.Context, vm, name string, destroyNewer bool) error
	DestroySnapshot(ctx context.Context, vm, name string) error
	ResizeDisk(ctx context.Context, vm string, sizeGB int) error
	DiskSizeGB(ctx context.Context, vm string) (int, error)
}

var reSnap = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// MaxSnapshots per VM keeps old snapshots from silently eating the pool.
const MaxSnapshots = 30

func (s *Service) snapDisks() (SnapshotDisks, error) {
	sd, ok := s.Disks.(SnapshotDisks)
	if !ok {
		return nil, errors.New("this disk backend has no snapshots")
	}
	return sd, nil
}

// Snapshots lists a VM's disk snapshots, oldest first.
func (s *Service) Snapshots(ctx context.Context, name string) ([]Snapshot, error) {
	sd, err := s.snapDisks()
	if err != nil {
		return nil, err
	}
	dom := Prefix + name
	if _, err := s.state(ctx, dom); err != nil {
		return nil, err
	}
	return sd.Snapshots(ctx, dom)
}

// CreateSnapshot snapshots the VM's disk. A running guest's filesystems are
// frozen for the instant of the snapshot when its guest agent answers, so the
// snapshot is clean; otherwise it is crash-consistent (like pulling the plug).
func (s *Service) CreateSnapshot(ctx context.Context, name, snap string) (Snapshot, error) {
	sd, err := s.snapDisks()
	if err != nil {
		return Snapshot{}, err
	}
	if snap == "" {
		snap = "snap-" + time.Now().UTC().Format("20060102-150405")
	}
	if !reSnap.MatchString(snap) {
		return Snapshot{}, fmt.Errorf("%w: snapshot name must be 1-40 chars: lowercase letters, digits, . _ -", ErrInvalid)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return Snapshot{}, err
	}
	snaps, err := sd.Snapshots(ctx, dom)
	if err != nil {
		return Snapshot{}, err
	}
	if findSnap(snaps, snap) >= 0 {
		return Snapshot{}, fmt.Errorf("%w: snapshot %q of vm %q", ErrExists, snap, name)
	}
	if len(snaps) >= MaxSnapshots {
		return Snapshot{}, fmt.Errorf("%w: vm %q already has %d snapshots; delete old ones first", ErrConflict, name, len(snaps))
	}
	if st == virt.Running {
		if err := s.HV.FSFreeze(ctx, dom); err == nil {
			defer func() { s.logErr("thaw "+dom, s.HV.FSThaw(context.WithoutCancel(ctx), dom)) }()
		} else if s.Log != nil {
			s.Log.Info("guest agent freeze unavailable; snapshot is crash-consistent", "vm", name, "err", err)
		}
	}
	if err := sd.Snapshot(ctx, dom, snap); err != nil {
		return Snapshot{}, err
	}
	snaps, err = sd.Snapshots(ctx, dom)
	if i := findSnap(snaps, snap); err == nil && i >= 0 {
		return snaps[i], nil
	}
	return Snapshot{Name: snap, CreatedAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

// Rollback returns a stopped VM's disk to a snapshot. ZFS can only roll back to
// the newest snapshot, so newer ones are deleted, but only with destroyNewer.
func (s *Service) Rollback(ctx context.Context, name, snap string, destroyNewer bool) (VM, error) {
	sd, err := s.snapDisks()
	if err != nil {
		return VM{}, err
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if st != virt.Stopped {
		return VM{}, fmt.Errorf("%w: stop vm %q before rolling back (it is %s)", ErrConflict, name, st)
	}
	snaps, err := sd.Snapshots(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	i := findSnap(snaps, snap)
	if i < 0 {
		return VM{}, fmt.Errorf("%w: snapshot %q of vm %q", ErrNotFound, snap, name)
	}
	if newer := snaps[i+1:]; len(newer) > 0 && !destroyNewer {
		names := make([]string, len(newer))
		for j, n := range newer {
			names[j] = n.Name
		}
		return VM{}, fmt.Errorf("%w: rolling back to %q deletes the newer snapshots %s; confirm with destroy_newer",
			ErrConflict, snap, strings.Join(names, ", "))
	}
	if err := sd.Rollback(ctx, dom, snap, destroyNewer); err != nil {
		return VM{}, err
	}
	// The disk size comes back with the snapshot; keep the VM's record in step.
	if gb, err := sd.DiskSizeGB(ctx, dom); err == nil {
		if err := s.redefine(ctx, dom, func(sp *virt.Spec) { sp.DiskGB = gb }); err != nil {
			return VM{}, err
		}
	}
	return s.get(ctx, dom)
}

// DeleteSnapshot removes one snapshot.
func (s *Service) DeleteSnapshot(ctx context.Context, name, snap string) error {
	sd, err := s.snapDisks()
	if err != nil {
		return err
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if _, err := s.state(ctx, dom); err != nil {
		return err
	}
	snaps, err := sd.Snapshots(ctx, dom)
	if err != nil {
		return err
	}
	if findSnap(snaps, snap) < 0 {
		return fmt.Errorf("%w: snapshot %q of vm %q", ErrNotFound, snap, name)
	}
	return sd.DestroySnapshot(ctx, dom, snap)
}

// ResizeRequest changes a stopped VM. Zero fields stay as they are.
type ResizeRequest struct {
	VCPUs     int `json:"vcpus,omitempty"`
	MemoryMiB int `json:"memory_mib,omitempty"`
	DiskGB    int `json:"disk_gb,omitempty"` // grow only
}

// Resize changes CPUs, memory and/or disk size of a stopped VM. The guest's
// root filesystem grows to the new disk size on its next boot (cloud-init).
func (s *Service) Resize(ctx context.Context, name string, r ResizeRequest) (VM, error) {
	sd, err := s.snapDisks()
	if err != nil {
		return VM{}, err
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	cur, err := s.get(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	var errs []string
	if r.VCPUs != 0 && (r.VCPUs < 1 || r.VCPUs > s.Limits.MaxVCPUs) {
		errs = append(errs, fmt.Sprintf("vcpus must be 1-%d", s.Limits.MaxVCPUs))
	}
	if r.MemoryMiB != 0 && (r.MemoryMiB < 256 || r.MemoryMiB > s.Limits.MaxMemoryMiB) {
		errs = append(errs, fmt.Sprintf("memory_mib must be 256-%d", s.Limits.MaxMemoryMiB))
	}
	if r.DiskGB != 0 && r.DiskGB < cur.DiskGB {
		errs = append(errs, fmt.Sprintf("disk_gb can only grow (now %d GB): shrinking would destroy data", cur.DiskGB))
	}
	if r.DiskGB > s.Limits.MaxDiskGB {
		errs = append(errs, fmt.Sprintf("disk_gb must be at most %d", s.Limits.MaxDiskGB))
	}
	if len(errs) > 0 {
		return VM{}, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
	}
	if st != virt.Stopped {
		return VM{}, fmt.Errorf("%w: stop vm %q before resizing (it is %s)", ErrConflict, name, st)
	}
	if r.DiskGB > cur.DiskGB {
		if err := sd.ResizeDisk(ctx, dom, r.DiskGB); err != nil {
			return VM{}, err
		}
	}
	err = s.redefine(ctx, dom, func(sp *virt.Spec) {
		if r.VCPUs != 0 {
			sp.VCPUs = r.VCPUs
		}
		if r.MemoryMiB != 0 {
			sp.MemoryMiB = r.MemoryMiB
		}
		if r.DiskGB > sp.DiskGB {
			sp.DiskGB = r.DiskGB
		}
	})
	if err != nil {
		return VM{}, err
	}
	return s.get(ctx, dom)
}

// redefine rewrites a domain's persistent definition from its own spec.
func (s *Service) redefine(ctx context.Context, dom string, change func(*virt.Spec)) error {
	b, err := s.HV.XML(ctx, dom)
	if err != nil {
		return err
	}
	spec, err := virt.ParseDomainXML(b)
	if err != nil {
		return err
	}
	change(&spec)
	x, err := virt.DomainXML(spec)
	if err != nil {
		return err
	}
	return s.HV.Define(ctx, x)
}

func findSnap(snaps []Snapshot, name string) int {
	for i, sn := range snaps {
		if sn.Name == name {
			return i
		}
	}
	return -1
}

// ConsoleAddr is where a running VM's screen (VNC) can be reached on the host.
func (s *Service) ConsoleAddr(ctx context.Context, name string) (string, error) {
	dom := Prefix + name
	st, err := s.state(ctx, dom)
	if err != nil {
		return "", err
	}
	if st != virt.Running {
		return "", fmt.Errorf("%w: vm %q is %s; start it to see its screen", ErrConflict, name, st)
	}
	return s.HV.VNCAddr(ctx, dom)
}

var reUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// SetPassword sets a guest account's password through the guest agent, so the
// user can log in on the console (cloud images ship with passwords locked).
func (s *Service) SetPassword(ctx context.Context, name, user, password string) error {
	if user == "" {
		user = "debian"
	}
	if !reUser.MatchString(user) {
		return fmt.Errorf("%w: user must be a Linux user name", ErrInvalid)
	}
	if len(password) < 8 || len(password) > 128 || strings.ContainsAny(password, "\n\r\x00") {
		return fmt.Errorf("%w: password must be 8-128 characters on one line", ErrInvalid)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return err
	}
	if st != virt.Running {
		return fmt.Errorf("%w: vm %q is %s; start it first", ErrConflict, name, st)
	}
	// Right after a boot or reboot the guest agent can take a few seconds to
	// connect, so "not connected / not responding" is retried for a while.
	pause, wait := s.AgentRetry, s.AgentWait
	if pause == 0 {
		pause = 3 * time.Second
	}
	if wait == 0 {
		wait = 45 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		err = s.HV.SetUserPassword(ctx, dom, user, password)
		if err == nil {
			return nil
		}
		msg := strings.ToLower(err.Error())
		retry := strings.Contains(msg, "not connected") || strings.Contains(msg, "not responding") ||
			strings.Contains(msg, "agent is not") || strings.Contains(msg, "timeout")
		if !retry || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(pause)
	}
	if s.Log != nil {
		s.Log.Error("set guest password", "vm", name, "user", user, "err", err)
	}
	return fmt.Errorf("%w: could not set the password for %q: %v", ErrConflict, user, err)
}
