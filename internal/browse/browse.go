// Package browse opens a backup's disks read-only so single files can be
// looked at and downloaded (file-level restore, like Proxmox's "File
// Restore"). The disks are restored into a scratch place (a ZFS volume, or a
// sparse file for disk-image backups), their filesystems are mounted
// read-only, and everything is thrown away when the session is closed or an
// hour has passed.
package browse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/execx"
)

// Part is one filesystem found on the backup's disks.
type Part struct {
	ID    string `json:"id"`    // d0p1 = disk 0, partition 1
	FS    string `json:"fs"`    // ext4, xfs, ntfs, vfat, btrfs …
	Label string `json:"label"` // filesystem label
	Size  string `json:"size"`
	Error string `json:"error,omitempty"` // couldn't be mounted (LVM, encrypted, unknown)
	dev   string // block device
	mount string // where it is mounted (read-only)
}

// Session is one opened backup.
type Session struct {
	ID       string `json:"id"`
	VM       string `json:"vm"`
	Backup   string `json:"backup"`
	Owner    string `json:"owner,omitempty"`
	State    string `json:"state"` // preparing, ready, failed
	Error    string `json:"error,omitempty"`
	Parts    []Part `json:"parts"`
	Created  string `json:"created_at"`
	Expires  string `json:"expires_at"`
	datasets []string
	loops    []string
	dir      string
}

// Entry is a file or folder.
type Entry struct {
	Name    string `json:"name"`
	Dir     bool   `json:"dir"`
	Link    bool   `json:"link,omitempty"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

var (
	ErrNotFound = errors.New("not found")
	ErrBusy     = errors.New("busy")
)

// Manager keeps the open sessions.
type Manager struct {
	Run     execx.Runner
	Backups *backup.Store
	Dir     string // data_dir/browse: mount points, sparse files
	Dataset string // scratch ZFS parent, e.g. tank/p8s/browse
	TTL     time.Duration
	// Wait finds a device that appears after a receive; nil = poll the path.
	Wait func(ctx context.Context, dev string) error

	mu sync.Mutex
	s  map[string]*Session
}

func (m *Manager) ttl() time.Duration {
	if m.TTL > 0 {
		return m.TTL
	}
	return time.Hour
}

// MaxSessions open at once (each holds a restored copy of a backup).
const MaxSessions = 4

// Open starts preparing a session for backup b (in the background).
func (m *Manager) Open(b backup.Meta, owner string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.s == nil {
		m.s = map[string]*Session{}
	}
	for _, s := range m.s {
		if s.VM == b.VM && s.Backup == b.ID && s.State != "failed" {
			return m.view(s), nil // already open
		}
	}
	if len(m.s) >= MaxSessions {
		return Session{}, fmt.Errorf("%w: %d backups are open for browsing; close one first", ErrBusy, MaxSessions)
	}
	id := make([]byte, 6)
	_, _ = rand.Read(id)
	now := time.Now().UTC()
	s := &Session{ID: hex.EncodeToString(id), VM: b.VM, Backup: b.ID, Owner: owner, State: "preparing", Parts: []Part{},
		Created: now.Format(time.RFC3339), Expires: now.Add(m.ttl()).Format(time.RFC3339)}
	s.dir = filepath.Join(m.Dir, s.ID)
	m.s[s.ID] = s
	go m.prepare(s, b)
	return m.view(s), nil
}

func (m *Manager) view(s *Session) Session {
	v := *s
	v.Parts = append([]Part{}, s.Parts...)
	return v
}

// Get returns a session.
func (m *Manager) Get(id string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.s[id]
	if !ok {
		return Session{}, false
	}
	return m.view(s), true
}

// List returns the open sessions.
func (m *Manager) List() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Session{}
	for _, s := range m.s {
		out = append(out, m.view(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

func (m *Manager) prepare(s *Session, b backup.Meta) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	err := m.restore(ctx, s, b)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		s.State, s.Error = "failed", err.Error()
		go m.Close(s.ID)
		return
	}
	s.State = "ready"
}

func (m *Manager) restore(ctx context.Context, s *Session, b backup.Meta) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	idx := []int{0}
	for _, e := range b.Extra {
		idx = append(idx, e.Index)
	}
	for _, i := range idx {
		var dev string
		if b.Format == backup.FormatRaw {
			f := filepath.Join(s.dir, fmt.Sprintf("disk%d.img", i))
			if err := m.Backups.RawToFile(ctx, b, i, f); err != nil {
				return err
			}
			out, err := m.Run.Run(ctx, "losetup", "-f", "--show", "-P", "-r", f)
			if err != nil {
				return fmt.Errorf("opening disk %d: %w", i, err)
			}
			dev = strings.TrimSpace(string(out))
			m.mu.Lock()
			s.loops = append(s.loops, dev)
			m.mu.Unlock()
		} else {
			// the scratch parent (not mounted; volumes only)
			_, _ = m.Run.Run(ctx, "zfs", "create", "-p", "-o", "mountpoint=none", "-o", "canmount=off", m.Dataset)
			ds := fmt.Sprintf("%s/%s-%d", m.Dataset, s.ID, i)
			m.mu.Lock()
			s.datasets = append(s.datasets, ds)
			m.mu.Unlock()
			if err := m.Backups.ReceiveDisk(ctx, b, i, ds); err != nil {
				return err
			}
			dev = "/dev/zvol/" + ds
			if err := m.wait(ctx, dev); err != nil {
				return err
			}
		}
		_, _ = m.Run.Run(ctx, "udevadm", "settle", "--timeout=10")
		parts, err := m.findParts(ctx, dev, i)
		if err != nil {
			return err
		}
		for _, p := range parts {
			if p.Error == "" {
				p.mount = filepath.Join(s.dir, p.ID)
				if p.Error = m.mount(ctx, p); p.Error != "" {
					p.mount = ""
				}
			}
			m.mu.Lock()
			s.Parts = append(s.Parts, p)
			m.mu.Unlock()
		}
	}
	return nil
}

func (m *Manager) wait(ctx context.Context, dev string) error {
	if m.Wait != nil {
		return m.Wait(ctx, dev)
	}
	for k := 0; k < 60; k++ {
		if _, err := os.Stat(dev); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s didn't appear", dev)
}

type lsblkDev struct {
	Path     string     `json:"path"`
	FSType   *string    `json:"fstype"`
	Label    *string    `json:"label"`
	Size     string     `json:"size"`
	Type     string     `json:"type"`
	Children []lsblkDev `json:"children"`
}

// findParts lists the filesystems on dev (its partitions, or dev itself).
func (m *Manager) findParts(ctx context.Context, dev string, disk int) ([]Part, error) {
	out, err := m.Run.Run(ctx, "lsblk", "-J", "-o", "PATH,FSTYPE,LABEL,SIZE,TYPE", dev)
	if err != nil {
		return nil, fmt.Errorf("reading disk %d: %w", disk, err)
	}
	return ParseLsblk(out, disk)
}

// ParseLsblk turns lsblk -J output into parts (exported for tests).
func ParseLsblk(out []byte, disk int) ([]Part, error) {
	var r struct {
		Devices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	var parts []Part
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	add := func(d lsblkDev, n int) {
		f := str(d.FSType)
		parts = append(parts, Part{ID: fmt.Sprintf("d%dp%d", disk, n), FS: f, Label: str(d.Label), Size: d.Size, Error: fsProblem(f), dev: d.Path})
	}
	for _, d := range r.Devices {
		if len(d.Children) == 0 { // a filesystem on the whole disk
			if str(d.FSType) != "" {
				add(d, 0)
			}
			continue
		}
		for k, c := range d.Children {
			if str(c.FSType) == "" || str(c.FSType) == "swap" {
				continue
			}
			add(c, k+1)
		}
	}
	return parts, nil
}

var mountable = map[string]string{
	"ext4": "ro,noload", "ext3": "ro,noload", "ext2": "ro", "xfs": "ro,norecovery,nouuid", "btrfs": "ro,nologreplay",
	"vfat": "ro", "exfat": "ro", "ntfs": "ro", "ntfs3": "ro",
}

func fsProblem(fs string) string {
	switch fs {
	case "LVM2_member":
		return "LVM volume: not opened here (restore the backup as a new VM to get at it)"
	case "crypto_LUKS":
		return "encrypted (LUKS): restore the backup as a new VM and unlock it there"
	case "zfs_member":
		return "ZFS pool inside the VM: restore the backup as a new VM"
	}
	if _, ok := mountable[fs]; !ok {
		return "filesystem " + fs + " can't be opened here"
	}
	return ""
}

// mount mounts p.dev read-only on p.mount; it returns why not ("" = mounted).
func (m *Manager) mount(ctx context.Context, p Part) string {
	if err := os.MkdirAll(p.mount, 0o700); err != nil {
		return err.Error()
	}
	t := p.FS
	if t == "ntfs" {
		t = "ntfs3" // the kernel's NTFS driver
	}
	if _, err := m.Run.Run(ctx, "mount", "-t", t, "-o", mountable[p.FS]+",nodev,nosuid,noexec", p.dev, p.mount); err != nil {
		var ee *execx.Error
		if errors.As(err, &ee) && ee.Stderr != "" {
			return "couldn't open it: " + ee.Stderr
		}
		return "couldn't open it: " + err.Error()
	}
	return ""
}

// Close unmounts and deletes everything of a session.
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	s, ok := m.s[id]
	if ok {
		delete(m.s, id)
	}
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, p := range s.Parts {
		if p.mount != "" {
			_, _ = m.Run.Run(ctx, "umount", "-l", p.mount)
		}
	}
	for _, l := range s.loops {
		_, _ = m.Run.Run(ctx, "losetup", "-d", l)
	}
	for _, ds := range s.datasets {
		_, _ = m.Run.Run(ctx, "zfs", "destroy", "-r", ds)
	}
	return os.RemoveAll(s.dir)
}

// Expire closes sessions past their time; run it every few minutes.
func (m *Manager) Expire() {
	now := time.Now()
	for _, s := range m.List() {
		if t, err := time.Parse(time.RFC3339, s.Expires); err == nil && now.After(t) {
			_ = m.Close(s.ID)
		}
	}
}

// Cleanup removes what an earlier p8sd left behind (at start).
func (m *Manager) Cleanup(ctx context.Context) {
	if m.Dataset != "" {
		if out, err := m.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-d", "1", m.Dataset); err == nil {
			for _, n := range strings.Fields(string(out)) {
				if n != m.Dataset {
					_, _ = m.Run.Run(ctx, "zfs", "destroy", "-r", n)
				}
			}
		}
	}
	if out, err := m.Run.Run(ctx, "losetup", "-n", "-O", "NAME,BACK-FILE"); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(l)
			if len(f) == 2 && m.Dir != "" && strings.HasPrefix(f[1], m.Dir+"/") {
				_, _ = m.Run.Run(ctx, "losetup", "-d", f[0])
			}
		}
	}
	if m.Dir != "" {
		ents, _ := os.ReadDir(m.Dir)
		for _, e := range ents {
			p := filepath.Join(m.Dir, e.Name())
			if subs, err := os.ReadDir(p); err == nil {
				for _, sub := range subs {
					_, _ = m.Run.Run(ctx, "umount", "-l", filepath.Join(p, sub.Name()))
				}
			}
			_ = os.RemoveAll(p)
		}
	}
}

func (m *Manager) part(id, part string) (*Session, Part, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.s[id]
	if !ok {
		return nil, Part{}, ErrNotFound
	}
	if s.State != "ready" {
		return nil, Part{}, fmt.Errorf("%w: the backup is still being opened", ErrBusy)
	}
	for _, p := range s.Parts {
		if p.ID == part {
			if p.Error != "" {
				return nil, Part{}, fmt.Errorf("%w: %s", ErrNotFound, p.Error)
			}
			return s, p, nil
		}
	}
	return nil, Part{}, ErrNotFound
}

// clean makes a path relative to the filesystem's root ("" = root).
func clean(p string) string {
	p = path.Clean("/" + p)
	return strings.TrimPrefix(p, "/")
}

// List lists a folder of a mounted filesystem. Symbolic links are shown but
// never followed out of it (os.Root).
func (m *Manager) Ls(id, part, dir string) ([]Entry, error) {
	_, p, err := m.part(id, part)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(p.mount)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel := clean(dir)
	if rel == "" {
		rel = "."
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, dir)
	}
	defer f.Close()
	ents, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("%w: %s isn't a folder", ErrNotFound, dir)
	}
	out := make([]Entry, 0, len(ents))
	for _, e := range ents {
		en := Entry{Name: e.Name(), Dir: e.IsDir(), Link: e.Type()&fs.ModeSymlink != 0}
		if info, err := e.Info(); err == nil {
			en.Size, en.ModTime = info.Size(), info.ModTime().UTC().Format(time.RFC3339)
		}
		out = append(out, en)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Open opens a regular file for download.
func (m *Manager) OpenFile(id, part, file string) (io.ReadCloser, fs.FileInfo, error) {
	_, p, err := m.part(id, part)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(p.mount)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.Open(clean(file))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, file)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%w: %s isn't a file", ErrNotFound, file)
	}
	return f, st, nil
}
