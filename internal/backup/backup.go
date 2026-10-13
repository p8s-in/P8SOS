// Package backup stores full copies of VM disks as compressed files
// (`zfs send` streams) and brings them back (`zfs receive`).
//
// Layout: <dir>/<vm>/<vm>-<id>.zfs.zst plus <vm>-<id>.json (what the VM looked
// like). Files, not ZFS snapshots, so a backup directory on another disk (or
// copied off the machine) survives the loss of the VM pool.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"p8sos/internal/execx"
)

// Meta describes one backup.
type Meta struct {
	ID          string `json:"id"` // YYYYMMDD-HHMMSS (UTC)
	VM          string `json:"vm"`
	CreatedAt   string `json:"created_at"`
	Template    string `json:"template"`
	User        string `json:"user"`
	Network     string `json:"network"`
	VLAN        int    `json:"vlan,omitempty"`
	OSType      string `json:"os_type,omitempty"`
	Owner       string `json:"owner,omitempty"`     // P8S user the VM belonged to
	Pool        string `json:"ip_pool,omitempty"`   // public IP pool, if it had an address
	PublicIP    string `json:"public_ip,omitempty"` // that address (given back when the VM is recreated)
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	DiskGB      int    `json:"disk_gb"`
	Bytes       int64  `json:"bytes"`          // size of the backup file
	Compression string `json:"compression"`    // zstd or gzip
	Consistent  bool   `json:"consistent"`     // guest filesystems were frozen (or the VM was off)
	Node        string `json:"node,omitempty"` // cluster server that keeps it (filled in by the master)
	// Hardware of VMs imported from other hypervisors (empty = what OSType implies).
	DiskBus  string `json:"disk_bus,omitempty"`
	NICModel string `json:"nic_model,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	// Extra are the VM's data disks, each in a file of its own next to the root disk's.
	Extra []ExtraDisk `json:"extra_disks,omitempty"`
}

// ExtraDisk is a data disk in a backup.
type ExtraDisk struct {
	Index  int   `json:"index"`
	SizeGB int   `json:"size_gb"`
	Bytes  int64 `json:"bytes"`
}

var (
	reID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)
	reVM = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}[a-z0-9]$`)
)

// ErrNotFound means there is no such backup.
var ErrNotFound = errors.New("backup not found")

type Store struct {
	Dir     string       // e.g. /var/lib/p8s/backups
	Dataset string       // parent of VM disks, e.g. tank/p8s/vms
	Run     execx.Runner // short zfs commands
	// Cmd builds streaming commands (zfs send/receive, zstd); nil = exec.CommandContext.
	Cmd func(ctx context.Context, name string, args ...string) *exec.Cmd
	// Compressor is "zstd" or "gzip"; empty = zstd when installed, else gzip.
	Compressor string
	Now        func() time.Time
	// Disk maps a guest to its dataset; nil = <Dataset>/<dom>-disk0 (VM zvols).
	Disk func(dom string) string
	// RecvOpts are the zfs receive -o options; nil = volmode=dev (VM zvols).
	RecvOpts []string
	// Parent says which dataset holds a VM's disks (network storage); nil = Dataset.
	Parent func(dom string) string
}

func (s *Store) parent(dom string) string {
	if s.Parent != nil {
		return s.Parent(dom)
	}
	return s.Dataset
}

// DiskDataset is the dataset of a VM's disk i (0 = root).
func (s *Store) DiskDataset(dom string, i int) string {
	if i == 0 {
		return s.disk(dom)
	}
	return s.extraDisk(dom, i)
}

func (s *Store) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if s.Cmd != nil {
		return s.Cmd(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

func (s *Store) compressor() string {
	if s.Compressor != "" {
		return s.Compressor
	}
	if _, err := exec.LookPath("zstd"); err == nil {
		return "zstd"
	}
	return "gzip"
}

func ext(comp string) string {
	if comp == "gzip" {
		return ".zfs.gz"
	}
	return ".zfs.zst"
}

// NewID returns the id for a backup taken now.
func (s *Store) NewID() string {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return now().UTC().Format("20060102-150405")
}

func (s *Store) disk(dom string) string {
	if s.Disk != nil {
		return s.Disk(dom)
	}
	return s.parent(dom) + "/" + dom + "-disk0"
}

func (s *Store) paths(vm, id, comp string) (data, meta string) {
	base := filepath.Join(s.Dir, vm, vm+"-"+id)
	return base + ext(comp), base + ".json"
}

// Files returns where a backup's data and metadata live.
func (s *Store) Files(m Meta) (data, meta string) { return s.paths(m.VM, m.ID, m.Compression) }

// ExtraFile is where data disk i of a backup lives.
func (s *Store) ExtraFile(m Meta, i int) string {
	return filepath.Join(s.Dir, m.VM, fmt.Sprintf("%s-%s-d%d%s", m.VM, m.ID, i, ext(m.Compression)))
}

// ExtraFiles are the files of all its data disks.
func (s *Store) ExtraFiles(m Meta) []string {
	var l []string
	for _, e := range m.Extra {
		l = append(l, s.ExtraFile(m, e.Index))
	}
	return l
}

func (s *Store) extraDisk(dom string, i int) string {
	return fmt.Sprintf("%s/%s-disk%d", s.Dataset, dom, i)
}

// send writes ds@snap compressed into file path (via .partial) and returns its size.
func (s *Store) send(ctx context.Context, ds, snap, comp, path string) (int64, error) {
	tmp := path + ".partial"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int64, error) { f.Close(); os.Remove(tmp); return 0, err }
	compArgs := []string{"-c"}
	if comp == "zstd" {
		compArgs = []string{"-q", "-c", "-T0", "-3"}
	}
	if err := pipe(s.cmd(ctx, "zfs", "send", ds+"@"+snap), s.cmd(ctx, comp, compArgs...), nil, f); err != nil {
		return fail(fmt.Errorf("backup stream: %w", err))
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return st.Size(), nil
}

// Save streams the existing snapshot dom-disk0@snap (and dom-diskN@snap of
// each data disk in m.Extra) into compressed files and writes the metadata
// last, so a backup is listed only once all its files are complete. m.VM and
// m.ID must be set.
func (s *Store) Save(ctx context.Context, dom, snap string, m Meta) (Meta, error) {
	if !reVM.MatchString(m.VM) || !reID.MatchString(m.ID) {
		return Meta{}, fmt.Errorf("bad backup name %q/%q", m.VM, m.ID)
	}
	m.Compression = s.compressor()
	data, metaPath := s.paths(m.VM, m.ID, m.Compression)
	if err := os.MkdirAll(filepath.Dir(data), 0o700); err != nil {
		return Meta{}, err
	}
	var done []string
	fail := func(err error) (Meta, error) {
		for _, f := range done {
			os.Remove(f)
		}
		return Meta{}, err
	}
	for i, e := range m.Extra {
		path := s.ExtraFile(m, e.Index)
		n, err := s.send(ctx, s.extraDisk(dom, e.Index), snap, m.Compression, path)
		if err != nil {
			return fail(fmt.Errorf("disk %d: %w", e.Index, err))
		}
		done = append(done, path)
		m.Extra[i].Bytes = n
	}
	n, err := s.send(ctx, s.disk(dom), snap, m.Compression, data)
	if err != nil {
		return fail(err)
	}
	done = append(done, data)
	m.Bytes = n
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(metaPath+".partial", b, 0o600); err != nil {
		return fail(err)
	}
	if err := os.Rename(metaPath+".partial", metaPath); err != nil {
		os.Remove(metaPath + ".partial")
		return fail(err)
	}
	return m, nil
}

// TotalBytes is the size of all of a backup's disk files.
func (m Meta) TotalBytes() int64 {
	n := m.Bytes
	for _, e := range m.Extra {
		n += e.Bytes
	}
	return n
}

// pipe runs a | b with a's stdin `in` and b's stdout `out`, and reports the
// first real failure of either side (with its stderr).
func pipe(a, b *exec.Cmd, in io.Reader, out io.Writer) error {
	var ea, eb strings.Builder
	a.Stdin, a.Stderr = in, &ea
	b.Stdout, b.Stderr = out, &eb
	r, err := a.StdoutPipe()
	if err != nil {
		return err
	}
	b.Stdin = r
	if err := a.Start(); err != nil {
		return err
	}
	if err := b.Start(); err != nil {
		_ = a.Process.Kill()
		_ = a.Wait()
		return err
	}
	errB := b.Wait()
	if errB != nil {
		_ = a.Process.Kill() // reader gone: don't leave the writer blocked
	}
	errA := a.Wait()
	switch {
	case errA != nil && errB == nil:
		return fmt.Errorf("%s: %v %s", a.Args[0], errA, strings.TrimSpace(ea.String()))
	case errB != nil:
		if errA != nil && ea.Len() > 0 {
			return fmt.Errorf("%s: %v %s", a.Args[0], errA, strings.TrimSpace(ea.String()))
		}
		return fmt.Errorf("%s: %v %s", b.Args[0], errB, strings.TrimSpace(eb.String()))
	}
	return nil
}

// List returns a VM's backups, newest first ("" = every VM's).
func (s *Store) List(vm string) ([]Meta, error) {
	var dirs []string
	if vm != "" {
		dirs = []string{filepath.Join(s.Dir, vm)}
	} else {
		ents, err := os.ReadDir(s.Dir)
		if errors.Is(err, os.ErrNotExist) {
			return []Meta{}, nil
		}
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() && reVM.MatchString(e.Name()) {
				dirs = append(dirs, filepath.Join(s.Dir, e.Name()))
			}
		}
	}
	out := []Meta{}
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.json"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var m Meta
			if json.Unmarshal(b, &m) != nil || !reID.MatchString(m.ID) || !reVM.MatchString(m.VM) {
				continue
			}
			if data, _ := s.paths(m.VM, m.ID, m.Compression); fileExists(data) {
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID > out[j].ID
		}
		return out[i].VM < out[j].VM
	})
	return out, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Get returns one backup's metadata.
func (s *Store) Get(vm, id string) (Meta, error) {
	if !reVM.MatchString(vm) || !reID.MatchString(id) {
		return Meta{}, ErrNotFound
	}
	all, err := s.List(vm)
	if err != nil {
		return Meta{}, err
	}
	for _, m := range all {
		if m.ID == id {
			return m, nil
		}
	}
	return Meta{}, ErrNotFound
}

// Delete removes a backup.
func (s *Store) Delete(vm, id string) error {
	m, err := s.Get(vm, id)
	if err != nil {
		return err
	}
	data, meta := s.paths(m.VM, m.ID, m.Compression)
	if err := os.Remove(data); err != nil {
		return err
	}
	for _, f := range s.ExtraFiles(m) {
		_ = os.Remove(f)
	}
	_ = os.Remove(meta)
	return nil
}

// Prune keeps the newest `keep` backups of a VM and deletes the rest.
func (s *Store) Prune(vm string, keep int) (removed int, err error) {
	if keep < 1 {
		return 0, nil
	}
	all, err := s.List(vm)
	if err != nil {
		return 0, err
	}
	for _, m := range all[min(keep, len(all)):] {
		if err := s.Delete(m.VM, m.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// Restore writes a backup to the disks of domain dom. With replace the
// existing disks (and their snapshots) are swapped out; otherwise dom must have
// no disks yet. Data disks the VM has now but the backup hasn't are left as
// they are. All disks are received next to the old ones first, so a failed
// restore leaves the existing disks untouched.
func (s *Store) Restore(ctx context.Context, m Meta, dom string, replace bool) error {
	type part struct{ file, target string }
	data, _ := s.paths(m.VM, m.ID, m.Compression)
	parts := []part{{data, s.disk(dom)}}
	for _, e := range m.Extra {
		parts = append(parts, part{s.ExtraFile(m, e.Index), s.extraDisk(dom, e.Index)})
	}
	bg := context.WithoutCancel(ctx)
	var received []string
	cleanup := func() {
		for _, t := range received {
			_, _ = s.Run.Run(bg, "zfs", "destroy", "-r", t)
		}
	}
	for _, p := range parts {
		tmp := p.target + "-restore"
		_, _ = s.Run.Run(ctx, "zfs", "destroy", "-r", tmp) // leftover from an interrupted restore
		if err := s.receive(ctx, m, p.file, tmp); err != nil {
			cleanup()
			return err
		}
		received = append(received, tmp)
	}
	for _, p := range parts {
		tmp := p.target + "-restore"
		if replace {
			if _, err := s.Run.Run(ctx, "zfs", "destroy", "-r", p.target); err != nil {
				var ee *execx.Error
				if !errors.As(err, &ee) || !strings.Contains(ee.Stderr, "does not exist") {
					cleanup()
					return fmt.Errorf("remove current disk: %w", err)
				}
			}
		}
		if _, err := s.Run.Run(ctx, "zfs", "rename", tmp, p.target); err != nil {
			return fmt.Errorf("put restored disk in place (it is kept as %s): %w", tmp, err)
		}
		received = received[1:]
	}
	return nil
}

func (s *Store) receive(ctx context.Context, m Meta, file, tmp string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := s.cmd(ctx, m.Compression, "-d", "-c")
	if m.Compression == "zstd" {
		dec = s.cmd(ctx, "zstd", "-q", "-d", "-c")
	}
	// volmode=dev like the template, so the host doesn't scan the guest's partitions.
	opts := s.RecvOpts
	if opts == nil {
		opts = []string{"volmode=dev"}
	}
	args := []string{"receive", "-u"}
	for _, o := range opts {
		args = append(args, "-o", o)
	}
	if err := pipe(dec, s.cmd(ctx, "zfs", append(args, tmp)...), f, io.Discard); err != nil {
		_, _ = s.Run.Run(context.WithoutCancel(ctx), "zfs", "destroy", "-r", tmp)
		return fmt.Errorf("restore stream: %w", err)
	}
	// The stream carries the backup snapshot; the restored disk doesn't need it.
	_, _ = s.Run.Run(ctx, "zfs", "destroy", "-r", tmp+"@p8s-backup-"+m.ID)
	return nil
}
