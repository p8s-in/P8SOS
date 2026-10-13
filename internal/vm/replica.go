package vm

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

	"p8sos/internal/virt"
)

// Copies of VMs for high availability. The cluster's master asks the VM's
// server for a copy (SendReplica) and gives it to another server
// (RecvReplica), every few minutes. If the VM's server dies, the other one
// turns its copy into the VM (PromoteReplica). When the dead server comes
// back, its old VM becomes the copy (RetireVM), so copying goes the other way.

// replicaDisks is the storage that can keep copies (ZFS).
type replicaDisks interface {
	SendReplica(ctx context.Context, vm, base string, name func(snap string) error, w io.Writer) error
	RecvReplica(ctx context.Context, vm, base string, r io.Reader) error
	ReplicaSnaps(ctx context.Context, vm string) ([]string, error)
	DiskRepSnaps(ctx context.Context, vm string) ([]string, error)
	HasReplica(ctx context.Context, vm string) bool
	ReplicaBytes(ctx context.Context, vm string) int64
	DropReplica(ctx context.Context, vm string) error
	PromoteReplica(ctx context.Context, vm string) (string, error)
	RetireDisk(ctx context.Context, vm string) error
}

// ReplicaHeader is the first line of a copy stream.
type ReplicaHeader struct {
	MoveHeader
	Base string `json:"base,omitempty"` // "" = full copy
	Snap string `json:"snap"`
	From string `json:"from,omitempty"` // server name it came from
}

// Replica is a copy of a VM held on this server.
type Replica struct {
	Name      string `json:"name"`
	From      string `json:"from,omitempty"`
	At        string `json:"at,omitempty"` // when the copy was taken (RFC 3339)
	Bytes     int64  `json:"bytes"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memory_mib"`
	DiskGB    int    `json:"disk_gb"`
	OSType    string `json:"os_type,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

func (s *Service) replicas() (replicaDisks, error) {
	r, ok := s.Disks.(replicaDisks)
	if !ok || s.ReplicaDir == "" {
		return nil, fmt.Errorf("%w: this server's storage can't keep VM copies", ErrConflict)
	}
	return r, nil
}

func (s *Service) replicaFile(name string) string { return filepath.Join(s.ReplicaDir, name+".json") }

// SnapTime reads the time from a copy snapshot's name.
func SnapTime(snap string) time.Time {
	n, err := strconv.ParseInt(strings.TrimPrefix(snap, "p8s-rep-"), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// HACheck says whether a VM can be protected with copies ("" = yes).
func (s *Service) HACheck(ctx context.Context, name string) (string, error) {
	v, err := s.Get(ctx, name)
	if err != nil {
		return "", err
	}
	if _, err := s.replicas(); err != nil {
		return "this server's storage can't make copies", nil
	}
	if v.Public != nil {
		return "it has a public IP from this server's pool (it can't follow the VM to another server)", nil
	}
	if len(v.Disks) > 0 {
		return "it has extra disks; high availability copies only VMs with one disk for now", nil
	}
	return "", nil
}

// ReplicaSnaps are the copy snapshots of a VM held here (oldest first).
func (s *Service) ReplicaSnaps(ctx context.Context, name string) ([]string, error) {
	rd, err := s.replicas()
	if err != nil {
		return nil, err
	}
	if !reName.MatchString(name) {
		return nil, fmt.Errorf("%w: bad name", ErrInvalid)
	}
	l, err := rd.ReplicaSnaps(ctx, Prefix+name)
	if l == nil {
		l = []string{}
	}
	return l, err
}

// SendReplica writes a copy of the VM (header line + disk change since the
// newest snapshot the other server has, of those in have) to w.
func (s *Service) SendReplica(ctx context.Context, name string, have []string, from string, w io.Writer) error {
	rd, err := s.replicas()
	if err != nil {
		return err
	}
	if why, err := s.HACheck(ctx, name); err != nil {
		return err
	} else if why != "" {
		return fmt.Errorf("%w: %s can't be copied: %s", ErrConflict, name, why)
	}
	dom := Prefix + name
	unlock := s.lock(dom)
	h, err := s.moveHeader(ctx, name)
	unlock()
	if err != nil {
		return err
	}
	mine, err := rd.DiskRepSnaps(ctx, dom)
	if err != nil {
		return err
	}
	base := ""
	for _, m := range mine {
		for _, o := range have {
			if m == o {
				base = m // newest in common wins (mine is oldest first)
			}
		}
	}
	rh := ReplicaHeader{MoveHeader: h, Base: base, From: from}
	return rd.SendReplica(ctx, dom, base, func(snap string) error {
		rh.Snap = snap
		b, _ := json.Marshal(rh)
		_, err := w.Write(append(b, '\n'))
		return err
	}, w)
}

// RecvReplica stores a copy sent by SendReplica.
func (s *Service) RecvReplica(ctx context.Context, r io.Reader) (Replica, error) {
	rd, err := s.replicas()
	if err != nil {
		return Replica{}, err
	}
	br := bufio.NewReaderSize(r, 1<<20)
	_, line, err := readHeader(br)
	if err != nil {
		return Replica{}, err
	}
	var h ReplicaHeader
	if err := json.Unmarshal(line, &h); err != nil || !strings.HasPrefix(h.Snap, "p8s-rep-") {
		return Replica{}, fmt.Errorf("%w: bad copy header", ErrInvalid)
	}
	dom := Prefix + h.Name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return Replica{}, fmt.Errorf("%w: %s runs on this server; it can't also keep a copy of it", ErrExists, h.Name)
	}
	if err := rd.RecvReplica(ctx, dom, h.Base, br); err != nil {
		return Replica{}, err
	}
	if err := s.saveReplica(h); err != nil {
		return Replica{}, err
	}
	return s.replicaInfo(ctx, rd, h), nil
}

func (s *Service) saveReplica(h ReplicaHeader) error {
	if err := os.MkdirAll(s.ReplicaDir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(h)
	tmp := s.replicaFile(h.Name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.replicaFile(h.Name))
}

func (s *Service) loadReplica(name string) (ReplicaHeader, error) {
	var h ReplicaHeader
	if !reName.MatchString(name) {
		return h, fmt.Errorf("%w: bad name", ErrInvalid)
	}
	b, err := os.ReadFile(s.replicaFile(name))
	if errors.Is(err, os.ErrNotExist) {
		return h, fmt.Errorf("%w: no copy of %q on this server", ErrNotFound, name)
	} else if err != nil {
		return h, err
	}
	return h, json.Unmarshal(b, &h)
}

func (s *Service) replicaInfo(ctx context.Context, rd replicaDisks, h ReplicaHeader) Replica {
	r := Replica{Name: h.Name, From: h.From, Bytes: rd.ReplicaBytes(ctx, Prefix+h.Name), VCPUs: h.VCPUs,
		MemoryMiB: h.MemoryMiB, DiskGB: h.DiskGB, OSType: h.OSType, Owner: h.Owner}
	if snaps, _ := rd.ReplicaSnaps(ctx, Prefix+h.Name); len(snaps) > 0 {
		if t := SnapTime(snaps[len(snaps)-1]); !t.IsZero() {
			r.At = t.Format(time.RFC3339)
		}
	}
	return r
}

// Replicas lists the VM copies held on this server.
func (s *Service) Replicas(ctx context.Context) []Replica {
	out := []Replica{}
	rd, err := s.replicas()
	if err != nil {
		return out
	}
	files, _ := filepath.Glob(filepath.Join(s.ReplicaDir, "*.json"))
	sort.Strings(files)
	for _, f := range files {
		h, err := s.loadReplica(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil || !rd.HasReplica(ctx, Prefix+h.Name) {
			continue
		}
		out = append(out, s.replicaInfo(ctx, rd, h))
	}
	return out
}

// PromoteReplica makes the VM from its copy here (left stopped). Only do this
// when the VM isn't running anywhere else.
func (s *Service) PromoteReplica(ctx context.Context, name string) (VM, string, error) {
	rd, err := s.replicas()
	if err != nil {
		return VM{}, "", err
	}
	h, err := s.loadReplica(name)
	if err != nil {
		return VM{}, "", err
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return VM{}, "", fmt.Errorf("%w: %s is already on this server", ErrExists, name)
	}
	if !rd.HasReplica(ctx, dom) {
		return VM{}, "", fmt.Errorf("%w: the copy of %q is missing", ErrNotFound, name)
	}
	_ = s.Disks.DestroyDisk(ctx, dom) // leftover of an interrupted move or failover
	disk, err := rd.PromoteReplica(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	bg := context.WithoutCancel(ctx)
	undo := []func(){func() { s.logErr("undo failover disk", rd.RetireDisk(bg, dom)) }}
	h.Autostart = false // high availability starts it, not the host's boot
	v, note, err := s.defineMoved(ctx, h.MoveHeader, disk, &undo)
	if err != nil {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return VM{}, "", err
	}
	_ = os.Remove(s.replicaFile(name))
	if t := SnapTime(h.Snap); !t.IsZero() {
		note = fmt.Sprintf("; from the copy of %s", t.Local().Format("2 Jan 15:04:05")) + note
	}
	return v, note, nil
}

// DropReplica deletes the copy of a VM held here.
func (s *Service) DropReplica(ctx context.Context, name string) error {
	rd, err := s.replicas()
	if err != nil {
		return err
	}
	if !reName.MatchString(name) {
		return fmt.Errorf("%w: bad name", ErrInvalid)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if err := rd.DropReplica(ctx, dom); err != nil {
		return err
	}
	if err := os.Remove(s.replicaFile(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RetireVM switches off a VM that now runs on another server (after a
// failover) and keeps its disk as the copy. Missing VM: nothing to do.
func (s *Service) RetireVM(ctx context.Context, name, from string) error {
	rd, err := s.replicas()
	if err != nil {
		return err
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); errors.Is(err, virt.ErrNoDomain) {
		return nil
	} else if err != nil {
		return err
	}
	h, err := s.moveHeader(ctx, name)
	if err != nil {
		return err
	}
	if err := s.remove(ctx, name, true); err != nil {
		return err
	}
	if err := rd.RetireDisk(context.WithoutCancel(ctx), dom); err != nil {
		return err
	}
	return s.saveReplica(ReplicaHeader{MoveHeader: h, From: from})
}

// Fence switches off (hard) those of the named VMs that are running here.
// A server that lost the cluster does this, so a VM never runs twice.
func (s *Service) Fence(ctx context.Context, names []string) []string {
	var out []string
	for _, n := range names {
		dom := Prefix + n
		if st, err := s.HV.State(ctx, dom); err == nil && st != virt.Stopped {
			if err := s.HV.Destroy(ctx, dom); err == nil {
				out = append(out, n)
			} else {
				s.logErr("fence "+n, err)
			}
		}
	}
	return out
}

// SetAutostart sets whether the VM starts when the host boots.
func (s *Service) SetAutostart(ctx context.Context, name string, on bool) (VM, error) {
	dom := Prefix + name
	defer s.lock(dom)()
	if _, err := s.state(ctx, dom); err != nil {
		return VM{}, err
	}
	if err := s.HV.SetAutostart(ctx, dom, on); err != nil {
		return VM{}, err
	}
	return s.get(ctx, dom)
}
