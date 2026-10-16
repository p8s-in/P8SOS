package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"p8sos/internal/storage"
	"p8sos/internal/virt"
)

// P8S Mirror: a running VM's disks are written to a second server at the
// same moment as to its own (QEMU's block mirror in write-blocking mode, to
// NBD exports on the other server over the cluster tunnel). A write counts as
// done only when both servers have it, so when the VM's server dies the other
// one starts it from an exact copy. The copy is kept like a high-availability
// copy (replica.go): failover, coming back and fencing are the same.
//
// The source side (this file, the VM's server) talks to the VM's QEMU; the
// target side (the server keeping the mirror) makes volumes and serves them.

// mirrorHostOK: mirrors go over the cluster tunnel only (tests use 127.0.0.1).
var mirrorHostOK = func(h string) bool { return strings.HasPrefix(h, "10.250.0.") }

// MirrorPortBase is the first TCP port of mirror exports (cluster tunnel only).
const MirrorPortBase = 10810

// MirrorPortCount is how many ports mirrors may use (10810-10899).
const MirrorPortCount = 90

// MirrorPlan is what the other server needs to keep a mirror: the VM's
// settings and its disks' exact sizes.
type MirrorPlan struct {
	Header MoveHeader           `json:"header"`
	Disks  []storage.MirrorDisk `json:"disks"`
}

// MirrorTarget is where a VM's disks are mirrored to.
type MirrorTarget struct {
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Disks []int  `json:"disks"` // export "disk<i>" for each
}

// MirrorDiskState is one disk's mirror right now.
type MirrorDiskState struct {
	Index  int   `json:"index"`
	Job    bool  `json:"job"`    // the mirror runs
	Synced bool  `json:"synced"` // every write goes to both servers
	Done   int64 `json:"done"`   // bytes copied by the first sync
	Len    int64 `json:"len"`
}

// MirrorState is a VM's mirror as its server sees it.
type MirrorState struct {
	Running bool              `json:"running"`          // the VM runs (else no mirror runs; the copy stays as it was)
	Moving  bool              `json:"moving,omitempty"` // a live move runs: leave it alone
	Disks   []MirrorDiskState `json:"disks"`
}

// InSync: every disk is mirrored write by write.
func (m MirrorState) InSync() bool {
	if !m.Running || len(m.Disks) == 0 {
		return false
	}
	for _, d := range m.Disks {
		if !d.Job || !d.Synced {
			return false
		}
	}
	return true
}

// Progress of the first sync, 0-100.
func (m MirrorState) Progress() int {
	var done, all int64
	for _, d := range m.Disks {
		done, all = done+d.Done, all+d.Len
	}
	if all == 0 {
		return 0
	}
	return int(done * 100 / all)
}

type monitor interface {
	Monitor(ctx context.Context, name string, cmd []byte) ([]byte, error)
}

type mirrorDisks interface {
	DiskBytes(ctx context.Context, vm string, i int) (int64, error)
	PrepareMirror(ctx context.Context, vm string, disks []storage.MirrorDisk) ([]string, error)
	MirrorSynced(ctx context.Context, vm string)
	MirrorRollback(ctx context.Context, vm string) (bool, error)
	StartMirrorExport(ctx context.Context, vm, host string, port int, disks []storage.MirrorDisk, paths []string) error
	StopMirrorExport(ctx context.Context, vm string)
	MirrorExportUp(ctx context.Context, vm string) bool
	HideMirror(ctx context.Context, vm string)
}

type diskPaths interface {
	DevicePath(vm string) string
	ExtraPath(vm string, i int) string
}

func (s *Service) mirrorDisks() (mirrorDisks, error) {
	md, ok := s.Disks.(mirrorDisks)
	if !ok || s.ReplicaDir == "" {
		return nil, fmt.Errorf("%w: this server's storage can't keep a mirror", ErrConflict)
	}
	return md, nil
}

func (s *Service) qmp(ctx context.Context, dom string, cmd string, args any) (json.RawMessage, error) {
	m, ok := s.HV.(monitor)
	if !ok {
		return nil, fmt.Errorf("%w: this server can't mirror running VMs", ErrConflict)
	}
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	b, _ := json.Marshal(req)
	return m.Monitor(ctx, dom, b)
}

// --- the VM's server ---

// MirrorCheck says whether a VM can be mirrored ("" = yes).
func (s *Service) MirrorCheck(ctx context.Context, name string) (string, error) {
	why, err := s.HACheck(ctx, name)
	if err != nil || why != "" {
		return why, err
	}
	if s.shared(Prefix + name) {
		return "its disks are on Ceph: every server has them already", nil
	}
	if _, err := s.mirrorDisks(); err != nil {
		return "this server's storage can't mirror disks", nil
	}
	if _, ok := s.HV.(monitor); !ok {
		return "this server can't mirror running VMs", nil
	}
	return "", nil
}

// MirrorPlan describes the VM for the server that will keep its mirror.
func (s *Service) MirrorPlan(ctx context.Context, name string) (MirrorPlan, error) {
	if why, err := s.MirrorCheck(ctx, name); err != nil {
		return MirrorPlan{}, err
	} else if why != "" {
		return MirrorPlan{}, fmt.Errorf("%w: %s can't be mirrored: %s", ErrConflict, name, why)
	}
	md, _ := s.mirrorDisks()
	dom := Prefix + name
	unlock := s.lock(dom)
	h, err := s.moveHeader(ctx, name)
	unlock()
	if err != nil {
		return MirrorPlan{}, err
	}
	p := MirrorPlan{Header: h}
	for _, i := range append([]int{0}, extraIdx(h.Extra)...) {
		n, err := md.DiskBytes(ctx, dom, i)
		if err != nil {
			return MirrorPlan{}, fmt.Errorf("size of disk %d: %w", i, err)
		}
		p.Disks = append(p.Disks, storage.MirrorDisk{Index: i, Bytes: n})
	}
	return p, nil
}

type qmpBlock struct {
	Inserted *struct {
		NodeName string `json:"node-name"`
		File     string `json:"file"`
		Image    struct {
			Filename    string `json:"filename"`
			VirtualSize int64  `json:"virtual-size"`
		} `json:"image"`
	} `json:"inserted"`
}

type qmpJob struct {
	Device string `json:"device"`
	Ready  bool   `json:"ready"`
	Synced bool   `json:"actively-synced"`
	Offset int64  `json:"offset"`
	Len    int64  `json:"len"`
	Status string `json:"status"`
}

func mirrorJob(i int) string  { return fmt.Sprintf("p8s-mirror-%d", i) }
func mirrorNode(i int) string { return fmt.Sprintf("p8smir%d", i) }

// diskNodes maps the VM's disk indexes to QEMU's node names (by device path).
func (s *Service) diskNodes(ctx context.Context, dom string) (map[int]string, error) {
	dp, ok := s.Disks.(diskPaths)
	if !ok {
		return nil, fmt.Errorf("%w: this server's storage can't mirror disks", ErrConflict)
	}
	raw, err := s.qmp(ctx, dom, "query-block", nil)
	if err != nil {
		return nil, err
	}
	var blocks []qmpBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	byPath := map[string]string{}
	for _, b := range blocks {
		if b.Inserted != nil && b.Inserted.NodeName != "" {
			for _, f := range []string{b.Inserted.File, b.Inserted.Image.Filename} {
				if f != "" {
					byPath[f] = b.Inserted.NodeName
				}
			}
		}
	}
	out := map[int]string{}
	if n := byPath[dp.DevicePath(dom)]; n != "" {
		out[0] = n
	}
	for i := 1; i <= 8; i++ {
		if n := byPath[dp.ExtraPath(dom, i)]; n != "" {
			out[i] = n
		}
	}
	if out[0] == "" {
		return nil, fmt.Errorf("the VM's disk isn't found in QEMU (%s)", dp.DevicePath(dom))
	}
	return out, nil
}

func (s *Service) mirrorJobs(ctx context.Context, dom string) (map[string]qmpJob, error) {
	raw, err := s.qmp(ctx, dom, "query-block-jobs", nil)
	if err != nil {
		return nil, err
	}
	var l []qmpJob
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	m := map[string]qmpJob{}
	for _, j := range l {
		m[j.Device] = j
	}
	return m, nil
}

// MirrorStatus is the VM's mirror now.
func (s *Service) MirrorStatus(ctx context.Context, name string) (MirrorState, error) {
	dom := Prefix + name
	st, err := s.HV.State(ctx, dom)
	if err != nil {
		return MirrorState{}, err
	}
	out := MirrorState{Disks: []MirrorDiskState{}}
	if st != virt.Running {
		return out, nil
	}
	out.Running = true
	if raw, err := s.qmp(ctx, dom, "query-migrate", nil); err == nil {
		var m struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &m)
		switch m.Status {
		case "setup", "active", "pre-switchover", "device", "postcopy-active", "wait-unplug":
			out.Moving = true
		}
	}
	nodes, err := s.diskNodes(ctx, dom)
	if err != nil {
		return out, err
	}
	jobs, err := s.mirrorJobs(ctx, dom)
	if err != nil {
		return out, err
	}
	for i := 0; i <= 8; i++ {
		if _, ok := nodes[i]; !ok {
			continue
		}
		j, ok := jobs[mirrorJob(i)]
		out.Disks = append(out.Disks, MirrorDiskState{Index: i, Job: ok, Synced: ok && j.Ready && j.Synced, Done: j.Offset, Len: j.Len})
	}
	return out, nil
}

// MirrorStart starts mirroring the running VM's disks to t (disks already
// mirrored are left alone). The first sync copies everything in the background.
func (s *Service) MirrorStart(ctx context.Context, name string, t MirrorTarget) error {
	if t.Port < MirrorPortBase || t.Port >= MirrorPortBase+MirrorPortCount || !mirrorHostOK(t.Host) {
		return fmt.Errorf("%w: mirror target %s:%d (must be on the cluster tunnel)", ErrInvalid, t.Host, t.Port)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if st, err := s.HV.State(ctx, dom); err != nil {
		return err
	} else if st != virt.Running {
		return fmt.Errorf("%w: %s isn't running", ErrConflict, name)
	}
	nodes, err := s.diskNodes(ctx, dom)
	if err != nil {
		return err
	}
	jobs, err := s.mirrorJobs(ctx, dom)
	if err != nil {
		return err
	}
	for _, i := range t.Disks {
		node, ok := nodes[i]
		if !ok {
			return fmt.Errorf("disk %d isn't attached to the VM", i)
		}
		if _, running := jobs[mirrorJob(i)]; running {
			continue
		}
		_, _ = s.qmp(ctx, dom, "blockdev-del", map[string]any{"node-name": mirrorNode(i)}) // left from a mirror that broke
		if _, err := s.qmp(ctx, dom, "blockdev-add", MirrorTargetNode(i, t.Host, t.Port)); err != nil {
			return fmt.Errorf("connect disk %d to the mirror on %s: %w", i, t.Host, err)
		}
		if _, err := s.qmp(ctx, dom, "blockdev-mirror", MirrorJobArgs(i, node)); err != nil {
			_, _ = s.qmp(context.WithoutCancel(ctx), dom, "blockdev-del", map[string]any{"node-name": mirrorNode(i)})
			return fmt.Errorf("start mirroring disk %d: %w", i, err)
		}
	}
	return nil
}

// MirrorTargetNode is the QMP blockdev-add arguments for disk i's mirror.
// A short network hiccup (3 s) holds the writes instead of breaking the mirror.
func MirrorTargetNode(i int, host string, port int) map[string]any {
	return map[string]any{"driver": "nbd", "node-name": mirrorNode(i),
		"server": map[string]any{"type": "inet", "host": host, "port": fmt.Sprint(port)},
		"export": fmt.Sprintf("disk%d", i), "reconnect-delay": 3}
}

// MirrorJobArgs is the QMP blockdev-mirror arguments: copy everything, and
// from then on every write goes to both before the VM is told it's done.
func MirrorJobArgs(i int, node string) map[string]any {
	return map[string]any{"job-id": mirrorJob(i), "device": node, "target": mirrorNode(i),
		"sync": "full", "copy-mode": "write-blocking"}
}

// MirrorStop ends the VM's mirror (the copy keeps what it has; a mirror that
// was in sync is an exact copy of this moment).
func (s *Service) MirrorStop(ctx context.Context, name string) error {
	dom := Prefix + name
	defer s.lock(dom)()
	if st, err := s.HV.State(ctx, dom); err != nil || st != virt.Running {
		return nil // no QEMU, no mirror
	}
	jobs, err := s.mirrorJobs(ctx, dom)
	if err != nil {
		return err
	}
	for i := 0; i <= 8; i++ {
		if _, ok := jobs[mirrorJob(i)]; ok {
			if _, err := s.qmp(ctx, dom, "block-job-cancel", map[string]any{"device": mirrorJob(i)}); err != nil {
				return fmt.Errorf("stop mirroring disk %d: %w", i, err)
			}
		}
	}
	for try := 0; try < 50; try++ { // the jobs end within a moment
		jobs, err = s.mirrorJobs(ctx, dom)
		left := false
		for i := 0; i <= 8; i++ {
			_, ok := jobs[mirrorJob(i)]
			left = left || ok
		}
		if err != nil || !left {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for i := 0; i <= 8; i++ {
		_, _ = s.qmp(ctx, dom, "blockdev-del", map[string]any{"node-name": mirrorNode(i)})
	}
	return nil
}

// --- the server keeping the mirror ---

func (s *Service) mirrorPortFile(name string) string {
	return filepath.Join(s.ReplicaDir, name+".mirror-port")
}

// mirrorPort is the VM's export port here (kept across restarts; free ones are taken in order).
func (s *Service) mirrorPort(name string) (int, error) {
	if b, err := os.ReadFile(s.mirrorPortFile(name)); err == nil {
		var p int
		if _, err := fmt.Sscan(string(b), &p); err == nil && p >= MirrorPortBase && p < MirrorPortBase+MirrorPortCount {
			return p, nil
		}
	}
	used := map[int]bool{}
	files, _ := filepath.Glob(filepath.Join(s.ReplicaDir, "*.mirror-port"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var p int
		if _, err := fmt.Sscan(string(b), &p); err == nil {
			used[p] = true
		}
	}
	for p := MirrorPortBase; p < MirrorPortBase+MirrorPortCount; p++ {
		if !used[p] {
			if err := os.MkdirAll(s.ReplicaDir, 0o700); err != nil {
				return 0, err
			}
			return p, os.WriteFile(s.mirrorPortFile(name), []byte(fmt.Sprint(p)), 0o600)
		}
	}
	return 0, errors.New("no free port for another mirror on this server (90 at most)")
}

// MirrorPrepare gets this server ready to keep the mirror of a VM: its
// volumes (a snapshot of their last good state first), the VM's settings,
// and the NBD exports on host (this server's tunnel address).
func (s *Service) MirrorPrepare(ctx context.Context, p MirrorPlan, from, host string) (MirrorTarget, error) {
	md, err := s.mirrorDisks()
	if err != nil {
		return MirrorTarget{}, err
	}
	name := p.Header.Name
	if !reName.MatchString(name) || len(p.Disks) == 0 || p.Disks[0].Index != 0 {
		return MirrorTarget{}, fmt.Errorf("%w: bad mirror plan", ErrInvalid)
	}
	if !strings.HasPrefix(host, "10.250.0.") {
		return MirrorTarget{}, fmt.Errorf("%w: this server has no cluster tunnel address", ErrConflict)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return MirrorTarget{}, fmt.Errorf("%w: %s runs on this server; it can't also keep its mirror", ErrExists, name)
	}
	port, err := s.mirrorPort(name)
	if err != nil {
		return MirrorTarget{}, err
	}
	md.StopMirrorExport(ctx, dom)
	paths, err := md.PrepareMirror(ctx, dom, p.Disks)
	if err != nil {
		return MirrorTarget{}, err
	}
	h := ReplicaHeader{MoveHeader: p.Header, Snap: repSnapNow(), From: from, Mirror: true}
	if err := s.saveReplica(h); err != nil {
		return MirrorTarget{}, err
	}
	if err := md.StartMirrorExport(ctx, dom, host, port, p.Disks, paths); err != nil {
		return MirrorTarget{}, err
	}
	t := MirrorTarget{Host: host, Port: port}
	for _, d := range p.Disks {
		t.Disks = append(t.Disks, d.Index)
	}
	// it listens within a moment
	for i := 0; i < 50 && !md.MirrorExportUp(ctx, dom); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return t, nil
}

// MirrorSynced records that the mirror caught up: the old state isn't needed.
func (s *Service) MirrorSynced(ctx context.Context, name string) error {
	md, err := s.mirrorDisks()
	if err != nil {
		return err
	}
	dom := Prefix + name
	defer s.lock(dom)()
	h, err := s.loadReplica(name)
	if err != nil {
		return err
	}
	if !h.Mirror || h.Synced {
		return nil
	}
	md.MirrorSynced(ctx, dom)
	h.Synced, h.Snap = true, repSnapNow()
	return s.saveReplica(h)
}

// mirrorRelease stops serving a mirror held here before it is used (promote)
// or deleted; a mirror cut short in its first sync goes back to its last good
// state. note says what happened.
func (s *Service) mirrorRelease(ctx context.Context, name string, use bool) (note string, err error) {
	md, ok := s.Disks.(mirrorDisks)
	if !ok {
		return "", nil
	}
	dom := Prefix + name
	md.StopMirrorExport(ctx, dom)
	_ = os.Remove(s.mirrorPortFile(name))
	if !use {
		return "", nil
	}
	h, err := s.loadReplica(name)
	if err != nil || !h.Mirror {
		return "", nil
	}
	if !h.Synced {
		rolled, err := md.MirrorRollback(ctx, dom)
		if err != nil {
			return "", err
		}
		if rolled {
			note = "; the mirror was still catching up, so it went back to its last complete state"
		}
	}
	md.HideMirror(ctx, dom)
	return note, nil
}
