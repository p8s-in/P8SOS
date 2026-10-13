// Package storages connects network storage to the server:
//
//   - NFS shares (a NAS): mounted at <MountRoot>/<name>, kept in /etc/fstab.
//     They hold backup copies (<mount>/p8s/backups) and installer ISOs
//     (<mount>/p8s/iso).
//   - iSCSI disks (a SAN LUN, or a NAS that exports one): logged in with
//     open-iscsi, and a ZFS pool "p8s-<name>" is made on the disk, so VM disks
//     there have snapshots, backups and data disks like local ones. One server
//     uses a LUN at a time.
//
// p8s-storages.service (scripts/storages-up.sh) connects them at boot before
// libvirt starts the VMs.
package storages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"p8sos/internal/execx"
)

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("no such storage")
	ErrExists   = errors.New("already exists")
	ErrConfirm  = errors.New("needs confirmation")
)

const (
	NFS   = "nfs"
	ISCSI = "iscsi"
)

// Storage is one connected network storage.
type Storage struct {
	Name string `json:"name"`
	Type string `json:"type"` // nfs | iscsi

	Server  string `json:"server,omitempty"`  // nfs: NAS address
	Export  string `json:"export,omitempty"`  // nfs: e.g. /mnt/tank/p8s
	Options string `json:"options,omitempty"` // nfs: extra mount options, e.g. vers=3

	Portal       string `json:"portal,omitempty"` // iscsi: host:port
	Target       string `json:"target,omitempty"` // iscsi: IQN
	LUN          int    `json:"lun"`
	CHAPUser     string `json:"chap_user,omitempty"`
	CHAPPassword string `json:"chap_password,omitempty"` // never returned by the API

	CreatedAt string `json:"created_at"`
}

// Status is a storage as the dashboard sees it.
type Status struct {
	Storage
	Online    bool   `json:"online"`
	Path      string `json:"path"`   // nfs: mount point; iscsi: ZFS dataset for VM disks
	Device    string `json:"device"` // iscsi: the disk
	SizeBytes int64  `json:"size_bytes"`
	FreeBytes int64  `json:"free_bytes"`
	Uses      string `json:"uses"` // what it holds, in words
	Error     string `json:"error,omitempty"`
}

// Manager keeps the list (a JSON file) and connects storages.
type Manager struct {
	Run       execx.Runner
	File      string // /etc/p8s/storages.json
	MountRoot string // /mnt/p8s
	Fstab     string // /etc/fstab
	Mounts    string // /proc/mounts
	ByPath    string // /dev/disk/by-path
	// InitiatorFile has this server's iSCSI name; "" = /etc/iscsi/initiatorname.iscsi.
	InitiatorFile string
	// Wait is how long to wait for an iSCSI disk to appear; 0 = 20 s.
	Wait time.Duration

	mu sync.Mutex
}

var (
	reName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,18}[a-z0-9]$`)
	reHost   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	reExport = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]{0,250}$`)
	reOpts   = regexp.MustCompile(`^[a-z0-9_=.,-]{0,200}$`)
	reIQN    = regexp.MustCompile(`^(iqn\.[0-9]{4}-[0-9]{2}\.[a-z0-9.-]+(:[A-Za-z0-9._:-]+)?|eui\.[0-9A-Fa-f]{16}|naa\.[0-9A-Fa-f]{16,32})$`)
	reCHAP   = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
)

func (m *Manager) mountRoot() string {
	if m.MountRoot != "" {
		return m.MountRoot
	}
	return "/mnt/p8s"
}

// MountPoint is where an NFS storage is mounted.
func (m *Manager) MountPoint(name string) string { return filepath.Join(m.mountRoot(), name) }

// PoolName is the ZFS pool made on an iSCSI storage's disk.
func PoolName(name string) string { return "p8s-" + name }

// Dataset is where VM disks live on an iSCSI storage.
func Dataset(name string) string { return PoolName(name) + "/vms" }

// ISODir / BackupDir are the folders P8S uses on an NFS storage.
func (m *Manager) ISODir(name string) string { return filepath.Join(m.MountPoint(name), "p8s", "iso") }
func (m *Manager) BackupDir(name string) string {
	return filepath.Join(m.MountPoint(name), "p8s", "backups")
}

func (m *Manager) load() ([]Storage, error) {
	b, err := os.ReadFile(m.File)
	if errors.Is(err, os.ErrNotExist) {
		return []Storage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var l []Storage
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", m.File, err)
	}
	return l, nil
}

func (m *Manager) save(l []Storage) error {
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	b, _ := json.MarshalIndent(l, "", "  ")
	if err := os.MkdirAll(filepath.Dir(m.File), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.File+".tmp", b, 0o600); err != nil { // CHAP passwords inside
		return err
	}
	return os.Rename(m.File+".tmp", m.File)
}

// All returns the storages as saved.
func (m *Manager) All() []Storage {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, _ := m.load()
	return l
}

// Get returns one storage.
func (m *Manager) Get(name string) (Storage, error) {
	for _, s := range m.All() {
		if s.Name == name {
			return s, nil
		}
	}
	return Storage{}, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// VMPools maps iSCSI storage names to the dataset for their VM disks.
func (m *Manager) VMPools() map[string]string {
	out := map[string]string{}
	for _, s := range m.All() {
		if s.Type == ISCSI {
			out[s.Name] = Dataset(s.Name)
		}
	}
	return out
}

// ISOShelves maps NFS storage names to their ISO folders (mounted ones only).
func (m *Manager) ISOShelves() map[string]string {
	out := map[string]string{}
	for _, s := range m.All() {
		if s.Type == NFS && m.mounted(m.MountPoint(s.Name)) {
			out[s.Name] = m.ISODir(s.Name)
		}
	}
	return out
}

func (m *Manager) mounted(dir string) bool {
	f := m.Mounts
	if f == "" {
		f = "/proc/mounts"
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if x := strings.Fields(l); len(x) >= 3 && x[1] == dir && strings.HasPrefix(x[2], "nfs") {
			return true
		}
	}
	return false
}

func stderr(err error) string {
	var ee *execx.Error
	if errors.As(err, &ee) && strings.TrimSpace(ee.Stderr) != "" {
		return strings.TrimSpace(ee.Stderr)
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func (m *Manager) check(s *Storage) error {
	if !reName.MatchString(s.Name) || s.Name == "local" {
		return fmt.Errorf("%w: name must be 2-20 characters: lowercase letters, digits, hyphens (not \"local\")", ErrInvalid)
	}
	switch s.Type {
	case NFS:
		if !reHost.MatchString(s.Server) {
			return fmt.Errorf("%w: server must be the NAS's address (IP or name)", ErrInvalid)
		}
		if !reExport.MatchString(s.Export) || strings.Contains(s.Export, "..") {
			return fmt.Errorf("%w: export must be the shared folder's path, e.g. /mnt/tank/p8s", ErrInvalid)
		}
		if !reOpts.MatchString(s.Options) {
			return fmt.Errorf("%w: options: letters, digits and , = . _ - only (e.g. vers=3)", ErrInvalid)
		}
		s.Portal, s.Target, s.LUN, s.CHAPUser, s.CHAPPassword = "", "", 0, "", ""
	case ISCSI:
		host, port, err := net.SplitHostPort(s.Portal)
		if err != nil {
			host, port = s.Portal, "3260"
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 || !reHost.MatchString(host) {
			return fmt.Errorf("%w: portal must be the SAN's address, e.g. 192.168.0.20 or 192.168.0.20:3260", ErrInvalid)
		}
		s.Portal = net.JoinHostPort(host, port)
		if !reIQN.MatchString(s.Target) {
			return fmt.Errorf("%w: target must be an iSCSI name like iqn.2005-10.org.freenas.ctl:p8s", ErrInvalid)
		}
		if s.LUN < 0 || s.LUN > 255 {
			return fmt.Errorf("%w: lun must be 0-255", ErrInvalid)
		}
		if (s.CHAPUser == "") != (s.CHAPPassword == "") || s.CHAPUser != "" && (!reCHAP.MatchString(s.CHAPUser) || !reCHAP.MatchString(s.CHAPPassword)) {
			return fmt.Errorf("%w: CHAP needs both a user and a password (no spaces)", ErrInvalid)
		}
		s.Server, s.Export, s.Options = "", "", ""
	default:
		return fmt.Errorf("%w: type must be nfs or iscsi", ErrInvalid)
	}
	return nil
}

// Add connects a new storage and remembers it. For iSCSI, format must be
// true to make a new pool on the disk (it erases the disk); a disk that
// already has this storage's pool (e.g. after reinstalling the server) is
// taken over as it is, with its VM disks.
func (m *Manager) Add(ctx context.Context, s Storage, format bool) (Status, error) {
	if err := m.check(&s); err != nil {
		return Status{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.load()
	if err != nil {
		return Status{}, err
	}
	for _, x := range l {
		if x.Name == s.Name {
			return Status{}, fmt.Errorf("%w: storage %q", ErrExists, s.Name)
		}
		if s.Type == ISCSI && x.Type == ISCSI && x.Target == s.Target && x.LUN == s.LUN && x.Portal == s.Portal {
			return Status{}, fmt.Errorf("%w: that disk is already storage %q", ErrExists, x.Name)
		}
	}
	s.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	if s.Type == NFS {
		err = m.addNFS(ctx, s)
	} else {
		err = m.addISCSI(ctx, s, format)
	}
	if err != nil {
		return Status{}, err
	}
	if err := m.save(append(l, s)); err != nil {
		return Status{}, err
	}
	return m.status(ctx, s), nil
}

// --- NFS ---

func (s Storage) mountOpts() string {
	o := "soft,timeo=150,retrans=3,_netdev,nofail,x-systemd.mount-timeout=30"
	if s.Options != "" {
		o += "," + s.Options
	}
	return o
}

func (m *Manager) addNFS(ctx context.Context, s Storage) error {
	dir := m.MountPoint(s.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if !m.mounted(dir) {
		if _, err := m.Run.Run(ctx, "mount", "-t", "nfs", "-o", s.mountOpts(), s.Server+":"+s.Export, dir); err != nil {
			_ = os.Remove(dir)
			return fmt.Errorf("%w: couldn't mount %s:%s (%s). Check the address and that the NAS shares this folder with this server's IP",
				ErrInvalid, s.Server, s.Export, stderr(err))
		}
	}
	// P8S's folders; also proves we can write there (root squash, read-only shares)
	for _, d := range []string{m.ISODir(s.Name), m.BackupDir(s.Name)} {
		if _, err := m.Run.Run(ctx, "mkdir", "-p", d); err != nil {
			_, _ = m.Run.Run(context.WithoutCancel(ctx), "umount", dir)
			_ = os.Remove(dir)
			return fmt.Errorf("%w: the share is mounted but this server can't write to it (%s). On the NAS, allow root access (no root squash / maproot root) for this server",
				ErrInvalid, stderr(err))
		}
	}
	return m.fstab(s.Name, fmt.Sprintf("%s:%s %s nfs %s 0 0", s.Server, s.Export, dir, s.mountOpts()))
}

const fstabMark = "# p8s-storage "

// fstab sets (line != "") or removes the storage's line in /etc/fstab.
func (m *Manager) fstab(name, line string) error {
	f := m.Fstab
	if f == "" {
		f = "/etc/fstab"
	}
	b, err := os.ReadFile(f)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var out []string
	skip := false
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if skip {
			skip = false
			continue
		}
		if l == fstabMark+name {
			skip = true
			continue
		}
		out = append(out, l)
	}
	if line != "" {
		out = append(out, fstabMark+name, line)
	}
	text := strings.Join(out, "\n")
	text = strings.TrimLeft(text, "\n") + "\n"
	if err := os.WriteFile(f+".p8s-tmp", []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(f+".p8s-tmp", f)
}

// --- iSCSI ---

func (m *Manager) iscsi(ctx context.Context, args ...string) ([]byte, error) {
	return m.Run.Run(ctx, "iscsiadm", args...)
}

// Discover lists the iSCSI targets a portal offers.
func (m *Manager) Discover(ctx context.Context, portal string) ([]string, error) {
	s := Storage{Name: "x1", Type: ISCSI, Portal: portal, Target: "iqn.2000-01.x.y"}
	if err := m.check(&s); err != nil {
		return nil, err
	}
	out, err := m.iscsi(ctx, "-m", "discovery", "-t", "sendtargets", "-p", s.Portal)
	if err != nil {
		return nil, fmt.Errorf("%w: no answer from %s (%s). Is the iSCSI service on and port 3260 open?", ErrInvalid, s.Portal, stderr(err))
	}
	return parseTargets(string(out)), nil
}

// parseTargets reads `iscsiadm -m discovery` lines: "10.0.0.5:3260,1 iqn...".
func parseTargets(out string) []string {
	seen, l := map[string]bool{}, []string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && reIQN.MatchString(f[1]) && !seen[f[1]] {
			seen[f[1]] = true
			l = append(l, f[1])
		}
	}
	sort.Strings(l)
	return l
}

// Device is the disk of an iSCSI storage once logged in.
func (m *Manager) Device(s Storage) string {
	dir := m.ByPath
	if dir == "" {
		dir = "/dev/disk/by-path"
	}
	return filepath.Join(dir, fmt.Sprintf("ip-%s-iscsi-%s-lun-%d", s.Portal, s.Target, s.LUN))
}

func (m *Manager) login(ctx context.Context, s Storage) error {
	if _, err := m.iscsi(ctx, "-m", "discovery", "-t", "sendtargets", "-p", s.Portal); err != nil {
		return fmt.Errorf("%w: no answer from %s (%s)", ErrInvalid, s.Portal, stderr(err))
	}
	node := []string{"-m", "node", "-T", s.Target, "-p", s.Portal}
	set := func(k, v string) error {
		_, err := m.iscsi(ctx, append(node, "-o", "update", "-n", k, "-v", v)...)
		return err
	}
	if err := set("node.startup", "automatic"); err != nil {
		return fmt.Errorf("%w: target %s isn't offered by %s (%s)", ErrInvalid, s.Target, s.Portal, stderr(err))
	}
	if s.CHAPUser != "" {
		for _, kv := range [][2]string{{"node.session.auth.authmethod", "CHAP"}, {"node.session.auth.username", s.CHAPUser}, {"node.session.auth.password", s.CHAPPassword}} {
			if err := set(kv[0], kv[1]); err != nil {
				return err
			}
		}
	}
	if _, err := m.iscsi(ctx, append(node, "--login")...); err != nil && !strings.Contains(stderr(err), "already present") {
		return fmt.Errorf("%w: login to %s failed (%s). Check the target's allowed initiators / CHAP", ErrInvalid, s.Target, stderr(err))
	}
	return nil
}

func (m *Manager) logout(ctx context.Context, s Storage) {
	node := []string{"-m", "node", "-T", s.Target, "-p", s.Portal}
	_, _ = m.iscsi(ctx, append(node, "--logout")...)
	_, _ = m.iscsi(ctx, append(node, "-o", "delete")...)
}

func (m *Manager) waitDevice(ctx context.Context, dev string) error {
	limit := m.Wait
	if limit == 0 {
		limit = 20 * time.Second
	}
	for deadline := time.Now().Add(limit); ; {
		if _, err := os.Stat(dev); err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%w: logged in, but LUN %s didn't appear. Check the LUN number", ErrInvalid, filepath.Base(dev))
		}
		_, _ = m.Run.Run(ctx, "udevadm", "settle", "--timeout=5")
		time.Sleep(300 * time.Millisecond)
	}
}

func (m *Manager) addISCSI(ctx context.Context, s Storage, format bool) error {
	bg := context.WithoutCancel(ctx)
	if err := m.login(ctx, s); err != nil {
		m.logout(bg, s)
		return err
	}
	dev := m.Device(s)
	if err := m.waitDevice(ctx, dev); err != nil {
		m.logout(bg, s)
		return err
	}
	pool := PoolName(s.Name)
	// this storage's pool already on the disk (server reinstalled): take it over
	if _, err := m.Run.Run(ctx, "zpool", "import", "-d", dev, "-N", "-o", "cachefile=none", pool); err == nil {
		_, _ = m.Run.Run(ctx, "zfs", "create", "-p", "-o", "canmount=off", Dataset(s.Name))
		return nil
	}
	if !format {
		what := "it"
		if out, err := m.Run.Run(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", dev); err == nil && strings.TrimSpace(string(out)) != "" {
			what = "it (it has " + strings.TrimSpace(string(out)) + " data on it)"
		}
		m.logout(bg, s)
		return fmt.Errorf("%w: P8S makes a new ZFS pool on this disk, which erases %s; confirm with format", ErrConfirm, what)
	}
	if _, err := m.Run.Run(ctx, "zpool", "create", "-f", "-o", "cachefile=none", "-o", "ashift=12",
		"-O", "compression=lz4", "-O", "atime=off", "-m", "none", pool, dev); err != nil {
		m.logout(bg, s)
		return fmt.Errorf("make the pool: %s", stderr(err))
	}
	if _, err := m.Run.Run(ctx, "zfs", "create", "-o", "canmount=off", Dataset(s.Name)); err != nil {
		_, _ = m.Run.Run(bg, "zpool", "destroy", pool)
		m.logout(bg, s)
		return fmt.Errorf("make the VM folder: %s", stderr(err))
	}
	return nil
}

// --- status, reconnect, remove ---

// List returns every storage with its state.
func (m *Manager) List(ctx context.Context) []Status {
	out := []Status{}
	for _, s := range m.All() {
		out = append(out, m.status(ctx, s))
	}
	return out
}

func (m *Manager) status(ctx context.Context, s Storage) Status {
	st := Status{Storage: s}
	st.CHAPPassword = ""
	switch s.Type {
	case NFS:
		st.Path, st.Uses = m.MountPoint(s.Name), "backup copies, ISOs"
		st.Online = m.mounted(st.Path)
		if !st.Online {
			st.Error = "not mounted (NAS off or unreachable?)"
			break
		}
		// timeout: a hung NFS server must not hang the dashboard
		out, err := m.Run.Run(ctx, "timeout", "5", "stat", "-f", "-c", "%b %a %S", st.Path)
		if f := strings.Fields(string(out)); err == nil && len(f) == 3 {
			b, _ := strconv.ParseInt(f[0], 10, 64)
			a, _ := strconv.ParseInt(f[1], 10, 64)
			bs, _ := strconv.ParseInt(f[2], 10, 64)
			st.SizeBytes, st.FreeBytes = b*bs, a*bs
		} else {
			st.Online, st.Error = false, "not answering"
		}
	case ISCSI:
		st.Path, st.Device, st.Uses = Dataset(s.Name), m.Device(s), "VM disks"
		out, err := m.Run.Run(ctx, "zpool", "list", "-H", "-p", "-o", "size,free,health", PoolName(s.Name))
		f := strings.Fields(string(out))
		if err != nil || len(f) != 3 {
			st.Error = "pool not imported (SAN unreachable?): " + stderr(err)
			break
		}
		st.SizeBytes, _ = strconv.ParseInt(f[0], 10, 64)
		st.FreeBytes, _ = strconv.ParseInt(f[1], 10, 64)
		st.Online = f[2] == "ONLINE"
		if !st.Online {
			st.Error = "pool is " + f[2]
		}
	}
	return st
}

// Connect (re)connects a storage: mounts the share, or logs in and imports the pool.
func (m *Manager) Connect(ctx context.Context, name string) (Status, error) {
	s, err := m.Get(name)
	if err != nil {
		return Status{}, err
	}
	switch s.Type {
	case NFS:
		dir := m.MountPoint(name)
		_ = os.MkdirAll(dir, 0o755)
		if !m.mounted(dir) {
			if _, err := m.Run.Run(ctx, "mount", dir); err != nil {
				return m.status(ctx, s), fmt.Errorf("mount %s: %s", dir, stderr(err))
			}
		}
	case ISCSI:
		if err := m.login(ctx, s); err != nil {
			return m.status(ctx, s), err
		}
		if err := m.waitDevice(ctx, m.Device(s)); err != nil {
			return m.status(ctx, s), err
		}
		if _, err := m.Run.Run(ctx, "zpool", "list", "-H", PoolName(name)); err != nil {
			if _, err := m.Run.Run(ctx, "zpool", "import", "-d", m.Device(s), "-N", "-o", "cachefile=none", PoolName(name)); err != nil {
				return m.status(ctx, s), fmt.Errorf("import pool: %s", stderr(err))
			}
		}
	}
	return m.status(ctx, s), nil
}

// Remove disconnects a storage and forgets it. The caller makes sure nothing
// uses it any more (VMs on an iSCSI storage). Data on the NAS / SAN stays.
func (m *Manager) Remove(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.load()
	if err != nil {
		return err
	}
	i := -1
	for j, s := range l {
		if s.Name == name {
			i = j
		}
	}
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	s := l[i]
	switch s.Type {
	case NFS:
		dir := m.MountPoint(name)
		if m.mounted(dir) {
			if _, err := m.Run.Run(ctx, "umount", dir); err != nil {
				if _, err2 := m.Run.Run(ctx, "umount", "-l", dir); err2 != nil {
					return fmt.Errorf("unmount %s: %s", dir, stderr(err))
				}
			}
		}
		if err := m.fstab(name, ""); err != nil {
			return err
		}
		_ = os.Remove(dir)
	case ISCSI:
		if _, err := m.Run.Run(ctx, "zpool", "export", PoolName(name)); err != nil && !strings.Contains(stderr(err), "no such pool") {
			return fmt.Errorf("release the pool: %s (is a VM on it still running?)", stderr(err))
		}
		m.logout(ctx, s)
	}
	return m.save(append(l[:i:i], l[i+1:]...))
}

// Initiator is this server's iSCSI name; the SAN must allow it.
func (m *Manager) Initiator() string {
	f := m.InitiatorFile
	if f == "" {
		f = "/etc/iscsi/initiatorname.iscsi"
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "InitiatorName="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
