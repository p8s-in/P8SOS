// Package ceph runs a Ceph storage cluster on the P8S servers themselves, like
// Proxmox does: every server can give disks (OSDs), three of them run
// monitors, and the pool p8s-vms holds VM disks (RBD images) that every server
// sees. VMs on it move between servers without copying disks and start on
// another server when theirs dies.
//
// Each server runs the steps for itself (Manager: install, monitor, manager,
// disk); the cluster master drives them (Cluster) and hands every server the
// same configuration and keys.
package ceph

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/execx"
	"p8sos/internal/installer"
)

// Pool is the RBD pool VM disks go to; Storage is its name in P8S.
const (
	Pool    = "p8s-vms"
	Storage = "ceph"
)

var (
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
	ErrBusy     = errors.New("busy")
	reName      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// Packages are what a Ceph server needs (Debian 13).
var Packages = []string{"ceph-mon", "ceph-mgr", "ceph-osd", "ceph-volume", "ceph-common", "lvm2"}

// Manager is this server's Ceph part.
type Manager struct {
	Run    execx.Runner
	Etc    string            // /etc/ceph
	Lib    string            // /var/lib/ceph
	Name   string            // this server's monitor/manager id (short host name)
	Exists func(string) bool // nil = os.Stat
	// Disks lists this server's disks (nil = lsblk).
	Disks func(ctx context.Context) ([]installer.Disk, error)
	// Addrs lists this server's IPv4 addresses with their networks (nil = the system's).
	Addrs func() []netip.Prefix
	Log   *slog.Logger
	// Settle is how long to wait for a new monitor / the cluster (0 = 2 minutes).
	Settle time.Duration
	// Tick is the pause between checks while waiting (0 = 2 s; tests shorten it).
	Tick time.Duration

	mu   sync.Mutex
	task Task
}

// Task is the last long step this server ran (install, monitor, disk ...).
type Task struct {
	Kind     string `json:"kind,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Running  bool   `json:"running"`
	Error    string `json:"error,omitempty"`
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`
}

// Bundle is what every Ceph server shares: the configuration and the keys.
type Bundle struct {
	Conf         string `json:"conf"`
	AdminKeyring string `json:"admin_keyring"`
	BootstrapOSD string `json:"bootstrap_osd"`
}

// Hash identifies a bundle (to see who has an old one).
func (b Bundle) Hash() string {
	h := sha256.Sum256([]byte(b.Conf + "\x00" + b.AdminKeyring + "\x00" + b.BootstrapOSD))
	return hex.EncodeToString(h[:8])
}

// Disk is a disk this server could give to Ceph.
type Disk struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Label string `json:"label"`
}

// NodeInfo is this server's Ceph state.
type NodeInfo struct {
	Name       string   `json:"name"`
	Installed  bool     `json:"installed"`
	Configured bool     `json:"configured"`
	Hash       string   `json:"hash,omitempty"`
	Mon        bool     `json:"mon"`
	Mgr        bool     `json:"mgr"`
	OSDs       []int    `json:"osds"`
	Disks      []Disk   `json:"disks"` // empty disks it could give
	Addrs      []string `json:"addrs"` // 192.168.1.10/24 ...
	Task       Task     `json:"task"`
}

func (m *Manager) etc(f string) string { return filepath.Join(m.Etc, f) }
func (m *Manager) lib(f string) string { return filepath.Join(m.Lib, f) }

// ConfFile, AdminKeyring, BootstrapKeyring are the shared files.
func (m *Manager) ConfFile() string         { return m.etc("ceph.conf") }
func (m *Manager) AdminKeyring() string     { return m.etc("ceph.client.admin.keyring") }
func (m *Manager) BootstrapKeyring() string { return m.lib("bootstrap-osd/ceph.keyring") }
func (m *Manager) monDir() string           { return m.lib("mon/ceph-" + m.Name) }
func (m *Manager) mgrDir() string           { return m.lib("mgr/ceph-" + m.Name) }

func (m *Manager) exists(p string) bool {
	if m.Exists != nil {
		return m.Exists(p)
	}
	_, err := os.Stat(p)
	return err == nil
}

// Installed reports whether the Ceph programs are here.
func (m *Manager) Installed() bool {
	for _, p := range []string{"/usr/bin/ceph-mon", "/usr/bin/ceph-mgr", "/usr/bin/ceph-osd", "/usr/sbin/ceph-volume", "/usr/bin/rbd"} {
		if !m.exists(p) {
			return false
		}
	}
	return true
}

// Configured reports whether this server has the cluster's configuration.
func (m *Manager) Configured() bool { return m.exists(m.ConfFile()) && m.exists(m.AdminKeyring()) }

// Info describes this server's Ceph state.
func (m *Manager) Info(ctx context.Context) NodeInfo {
	in := NodeInfo{Name: m.Name, Installed: m.Installed(), Configured: m.Configured(), OSDs: []int{}, Disks: []Disk{}, Addrs: []string{}}
	m.mu.Lock()
	in.Task = m.task
	m.mu.Unlock()
	if in.Configured {
		if b, err := m.Bundle(); err == nil {
			in.Hash = b.Hash()
		}
		in.Mon = m.exists(m.monDir())
		in.Mgr = m.exists(m.mgrDir())
		in.OSDs = m.localOSDs(ctx)
	}
	for _, p := range m.addrs() {
		in.Addrs = append(in.Addrs, p.String())
	}
	if d, err := m.emptyDisks(ctx); err == nil {
		in.Disks = d
	}
	return in
}

func (m *Manager) addrs() []netip.Prefix {
	if m.Addrs != nil {
		return m.Addrs()
	}
	return SystemAddrs()
}

// SystemAddrs are this server's IPv4 addresses (not loopback, not the cluster tunnel).
func SystemAddrs() []netip.Prefix {
	out, err := exec.Command("ip", "-j", "-4", "addr", "show").Output()
	if err != nil {
		return nil
	}
	return ParseAddrs(out)
}

// ParseAddrs reads `ip -j -4 addr show`.
func ParseAddrs(out []byte) []netip.Prefix {
	var links []struct {
		Ifname   string `json:"ifname"`
		AddrInfo []struct {
			Family    string `json:"family"`
			Local     string `json:"local"`
			Prefixlen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if json.Unmarshal(out, &links) != nil {
		return nil
	}
	var l []netip.Prefix
	for _, k := range links {
		if k.Ifname == "lo" || strings.HasPrefix(k.Ifname, "p8s-wg") || strings.HasPrefix(k.Ifname, "virbr") {
			continue
		}
		for _, a := range k.AddrInfo {
			ip, err := netip.ParseAddr(a.Local)
			if err != nil || !ip.Is4() || ip.IsLoopback() {
				continue
			}
			l = append(l, netip.PrefixFrom(ip, a.Prefixlen))
		}
	}
	return l
}

// AddrIn is this server's address in network n.
func (m *Manager) AddrIn(n netip.Prefix) (netip.Addr, bool) {
	for _, p := range m.addrs() {
		if n.Contains(p.Addr()) {
			return p.Addr(), true
		}
	}
	return netip.Addr{}, false
}

func (m *Manager) emptyDisks(ctx context.Context) ([]Disk, error) {
	var disks []installer.Disk
	var err error
	if m.Disks != nil {
		disks, err = m.Disks(ctx)
	} else {
		var out []byte
		out, err = m.Run.Run(ctx, "lsblk", installer.LsblkArgs...)
		if err == nil {
			disks, err = installer.ParseLsblk(out, installer.ByIDLinks("/dev/disk/by-id"))
		}
	}
	if err != nil {
		return nil, err
	}
	l := []Disk{}
	for _, d := range disks {
		if d.Usable() && d.Empty() && !d.Removable {
			l = append(l, Disk{Name: d.Name, Path: d.Path, Size: d.Size, Label: d.Label()})
		}
	}
	return l, nil
}

// ceph runs the ceph tool with the admin key.
func (m *Manager) ceph(ctx context.Context, args ...string) ([]byte, error) {
	return m.Run.Run(ctx, "ceph", append([]string{"--connect-timeout", "20"}, args...)...)
}

func (m *Manager) localOSDs(ctx context.Context) []int {
	ids := []int{}
	ents, err := os.ReadDir(m.lib("osd"))
	if err != nil {
		return ids
	}
	for _, e := range ents {
		var id int
		if _, err := fmt.Sscanf(e.Name(), "ceph-%d", &id); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// Bundle reads this server's shared files.
func (m *Manager) Bundle() (Bundle, error) {
	var b Bundle
	for _, f := range []struct {
		p string
		v *string
	}{{m.ConfFile(), &b.Conf}, {m.AdminKeyring(), &b.AdminKeyring}, {m.BootstrapKeyring(), &b.BootstrapOSD}} {
		data, err := os.ReadFile(f.p)
		if err != nil {
			return Bundle{}, err
		}
		*f.v = string(data)
	}
	return b, nil
}

var reKeyring = regexp.MustCompile(`(?s)^\[[a-z0-9.\-]+\]\s*\n\s*key = [A-Za-z0-9+/=]+`)

// SetBundle writes the shared files (from the master).
func (m *Manager) SetBundle(b Bundle) error {
	if !strings.Contains(b.Conf, "fsid = ") || !strings.Contains(b.Conf, "mon_host = ") {
		return fmt.Errorf("%w: not a Ceph configuration", ErrInvalid)
	}
	if !reKeyring.MatchString(b.AdminKeyring) || !reKeyring.MatchString(b.BootstrapOSD) {
		return fmt.Errorf("%w: bad keyring", ErrInvalid)
	}
	for _, f := range []struct {
		p, data string
		mode    os.FileMode
	}{{m.ConfFile(), b.Conf, 0o644}, {m.AdminKeyring(), b.AdminKeyring, 0o600}, {m.BootstrapKeyring(), b.BootstrapOSD, 0o600}} {
		if err := writeAtomic(f.p, f.data, f.mode); err != nil {
			return err
		}
	}
	_, _ = m.Run.Run(context.Background(), "chown", "-R", "ceph:ceph", m.lib("bootstrap-osd"))
	return nil
}

func writeAtomic(p, data string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".p8s-tmp"
	if err := os.WriteFile(tmp, []byte(data), mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// --- long steps, run in the background (Task) ---

// Start runs fn as this server's Ceph task; one at a time.
func (m *Manager) Start(kind, detail string, fn func(ctx context.Context) error) error {
	m.mu.Lock()
	if m.task.Running {
		k := m.task.Kind
		m.mu.Unlock()
		return fmt.Errorf("%w: this server is still busy with Ceph (%s)", ErrBusy, k)
	}
	m.task = Task{Kind: kind, Detail: detail, Running: true, Started: time.Now().UTC().Format(time.RFC3339)}
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		err := fn(ctx)
		m.mu.Lock()
		m.task.Running = false
		m.task.Finished = time.Now().UTC().Format(time.RFC3339)
		if err != nil {
			m.task.Error = err.Error()
		}
		m.mu.Unlock()
		if m.Log != nil {
			m.Log.Info("ceph task done", "kind", kind, "detail", detail, "err", err)
		}
	}()
	return nil
}

// Wait waits for the running task to end (tests).
func (m *Manager) Wait() Task {
	for {
		m.mu.Lock()
		t := m.task
		m.mu.Unlock()
		if !t.Running {
			return t
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (m *Manager) tick() time.Duration {
	if m.Tick > 0 {
		return m.Tick
	}
	return 2 * time.Second
}

func (m *Manager) settle() time.Duration {
	if m.Settle > 0 {
		return m.Settle
	}
	return 2 * time.Minute
}

// Install puts the Ceph packages on this server.
func (m *Manager) Install(ctx context.Context) error {
	if m.Installed() {
		return nil
	}
	if _, err := m.Run.Run(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "update", "-qq"); err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}
	args := append([]string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-qq"}, Packages...)
	if _, err := m.Run.Run(ctx, "env", args...); err != nil {
		return fmt.Errorf("installing Ceph: %w", err)
	}
	return nil
}

func (m *Manager) needInstalled() error {
	if !m.Installed() {
		return fmt.Errorf("%w: Ceph isn't installed on this server yet", ErrConflict)
	}
	return nil
}

func (m *Manager) needConfigured() error {
	if err := m.needInstalled(); err != nil {
		return err
	}
	if !m.Configured() {
		return fmt.Errorf("%w: this server doesn't have the Ceph configuration yet (the master sends it)", ErrConflict)
	}
	return nil
}

// NewFSID makes a cluster id.
func NewFSID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// CheckInit says whether this server can start a new cluster on network.
func (m *Manager) CheckInit(network string) (netip.Prefix, netip.Addr, error) {
	if err := m.needInstalled(); err != nil {
		return netip.Prefix{}, netip.Addr{}, err
	}
	if m.Configured() {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("%w: Ceph is already set up on this server", ErrConflict)
	}
	n, err := netip.ParsePrefix(strings.TrimSpace(network))
	if err != nil || !n.Addr().Is4() || n.Bits() < 8 || n.Bits() > 30 {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("%w: network %q: write it like 192.168.1.0/24", ErrInvalid, network)
	}
	n = n.Masked()
	ip, ok := m.AddrIn(n)
	if !ok {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("%w: this server has no address in %s", ErrInvalid, n)
	}
	if !reName.MatchString(m.Name) {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("%w: host name %q can't be a Ceph monitor name (letters, digits, '-')", ErrInvalid, m.Name)
	}
	return n, ip, nil
}

// Init starts a new Ceph cluster with this server as its first monitor and manager.
func (m *Manager) Init(ctx context.Context, network string) error {
	n, ip, err := m.CheckInit(network)
	if err != nil {
		return err
	}
	fsid := NewFSID()
	tmp, err := os.MkdirTemp("", "p8s-ceph-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	monKey := filepath.Join(tmp, "ceph.mon.keyring")
	monmap := filepath.Join(tmp, "monmap")
	conf := Conf(fsid, n, []Mon{{Name: m.Name, IP: ip.String()}})
	if err := writeAtomic(m.ConfFile(), conf, 0o644); err != nil {
		return err
	}
	undo := func() { _ = os.Remove(m.ConfFile()); _ = os.Remove(m.AdminKeyring()) }
	steps := [][]string{
		{"ceph-authtool", "--create-keyring", monKey, "--gen-key", "-n", "mon.", "--cap", "mon", "allow *"},
		{"ceph-authtool", "--create-keyring", m.AdminKeyring(), "--gen-key", "-n", "client.admin",
			"--cap", "mon", "allow *", "--cap", "osd", "allow *", "--cap", "mds", "allow *", "--cap", "mgr", "allow *"},
		{"install", "-d", "-o", "ceph", "-g", "ceph", "-m", "0750", m.lib("bootstrap-osd")},
		{"ceph-authtool", "--create-keyring", m.BootstrapKeyring(), "--gen-key", "-n", "client.bootstrap-osd",
			"--cap", "mon", "profile bootstrap-osd", "--cap", "mgr", "allow r"},
		{"ceph-authtool", monKey, "--import-keyring", m.AdminKeyring()},
		{"ceph-authtool", monKey, "--import-keyring", m.BootstrapKeyring()},
		{"chown", "ceph:ceph", monKey, m.BootstrapKeyring()},
		{"chmod", "0600", m.AdminKeyring()},
		{"monmaptool", "--create", "--addv", m.Name, monAddrv(ip.String()), "--fsid", fsid, monmap},
		{"install", "-d", "-o", "ceph", "-g", "ceph", "-m", "0750", m.monDir()},
		{"ceph-mon", "--cluster", "ceph", "--mkfs", "-i", m.Name, "--monmap", monmap, "--keyring", monKey, "--setuser", "ceph", "--setgroup", "ceph"},
		{"systemctl", "enable", "--now", "ceph-mon@" + m.Name},
		{"systemctl", "enable", "ceph.target", "ceph-mon.target"},
	}
	for _, s := range steps {
		if _, err := m.Run.Run(ctx, s[0], s[1:]...); err != nil {
			undo()
			_, _ = m.Run.Run(context.WithoutCancel(ctx), "systemctl", "disable", "--now", "ceph-mon@"+m.Name)
			_ = os.RemoveAll(m.monDir())
			return fmt.Errorf("%s: %w", s[0], err)
		}
	}
	if err := m.waitUp(ctx); err != nil {
		return err
	}
	for _, s := range [][]string{
		{"mon", "enable-msgr2"},
		{"config", "set", "mon", "auth_allow_insecure_global_id_reclaim", "false"},
	} {
		if _, err := m.ceph(ctx, s...); err != nil {
			return fmt.Errorf("ceph %s: %w", s[0], err)
		}
	}
	return m.CreateMgr(ctx)
}

func monAddrv(ip string) string { return fmt.Sprintf("[v2:%s:3300,v1:%s:6789]", ip, ip) }

// waitUp waits until the monitors answer.
func (m *Manager) waitUp(ctx context.Context) error {
	deadline := time.Now().Add(m.settle())
	for {
		_, err := m.ceph(ctx, "status", "-f", "json")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the Ceph monitor didn't start: %w", err)
		}
		time.Sleep(m.tick())
	}
}

// CreateMon adds a monitor on this server (the cluster must be up).
func (m *Manager) CreateMon(ctx context.Context) error {
	if err := m.needConfigured(); err != nil {
		return err
	}
	if m.exists(m.monDir()) {
		return fmt.Errorf("%w: this server already runs a monitor", ErrConflict)
	}
	if !reName.MatchString(m.Name) {
		return fmt.Errorf("%w: host name %q can't be a monitor name", ErrInvalid, m.Name)
	}
	net, err := NetworkOf(m.confText())
	if err != nil {
		return err
	}
	if _, ok := m.AddrIn(net); !ok {
		return fmt.Errorf("%w: this server has no address in the Ceph network %s", ErrInvalid, net)
	}
	tmp, err := os.MkdirTemp("", "p8s-ceph-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	key, mp := filepath.Join(tmp, "key"), filepath.Join(tmp, "monmap")
	if _, err := m.ceph(ctx, "auth", "get", "mon.", "-o", key); err != nil {
		return fmt.Errorf("monitor key: %w", err)
	}
	if _, err := m.ceph(ctx, "mon", "getmap", "-o", mp); err != nil {
		return fmt.Errorf("monitor map: %w", err)
	}
	for _, s := range [][]string{
		{"chown", "ceph:ceph", key, mp},
		{"install", "-d", "-o", "ceph", "-g", "ceph", "-m", "0750", m.monDir()},
		{"ceph-mon", "--cluster", "ceph", "--mkfs", "-i", m.Name, "--monmap", mp, "--keyring", key, "--setuser", "ceph", "--setgroup", "ceph"},
		{"systemctl", "enable", "--now", "ceph-mon@" + m.Name},
		{"systemctl", "enable", "ceph.target", "ceph-mon.target"},
	} {
		if _, err := m.Run.Run(ctx, s[0], s[1:]...); err != nil {
			_, _ = m.Run.Run(context.WithoutCancel(ctx), "systemctl", "disable", "--now", "ceph-mon@"+m.Name)
			_ = os.RemoveAll(m.monDir())
			return fmt.Errorf("%s: %w", s[0], err)
		}
	}
	// it joins by itself; wait until it is in the map
	deadline := time.Now().Add(m.settle())
	for {
		out, err := m.ceph(ctx, "mon", "dump", "-f", "json")
		if err == nil {
			if mons, _, err := ParseMonDump(out); err == nil {
				for _, mo := range mons {
					if mo.Name == m.Name {
						return nil
					}
				}
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return errors.New("the new monitor didn't join the others (is the Ceph network reachable between the servers? ports 3300 and 6789)")
		}
		time.Sleep(m.tick())
	}
}

// CreateMgr adds a manager on this server.
func (m *Manager) CreateMgr(ctx context.Context) error {
	if err := m.needConfigured(); err != nil {
		return err
	}
	if m.exists(m.mgrDir()) {
		return fmt.Errorf("%w: this server already runs a manager", ErrConflict)
	}
	for _, s := range [][]string{
		{"install", "-d", "-o", "ceph", "-g", "ceph", "-m", "0750", m.mgrDir()},
		{"ceph", "--connect-timeout", "20", "auth", "get-or-create", "mgr." + m.Name, "mon", "allow profile mgr", "osd", "allow *", "mds", "allow *",
			"-o", filepath.Join(m.mgrDir(), "keyring")},
		{"chown", "-R", "ceph:ceph", m.mgrDir()},
		{"systemctl", "enable", "--now", "ceph-mgr@" + m.Name},
		{"systemctl", "enable", "ceph-mgr.target"},
	} {
		if _, err := m.Run.Run(ctx, s[0], s[1:]...); err != nil {
			_ = os.RemoveAll(m.mgrDir())
			return fmt.Errorf("%s: %w", s[0], err)
		}
	}
	return nil
}

// CheckOSD says whether dev can become an OSD (an empty disk of this server).
func (m *Manager) CheckOSD(ctx context.Context, dev string) error {
	if err := m.needConfigured(); err != nil {
		return err
	}
	disks, err := m.emptyDisks(ctx)
	if err != nil {
		return err
	}
	for _, d := range disks {
		if d.Path == dev || d.Name == dev {
			return nil
		}
	}
	return fmt.Errorf("%w: %s isn't an empty disk of this server (Ceph only takes disks with nothing on them; "+
		"see the list of disks it offers)", ErrInvalid, dev)
}

// CreateOSD gives an empty disk to Ceph.
func (m *Manager) CreateOSD(ctx context.Context, dev string) error {
	if err := m.CheckOSD(ctx, dev); err != nil {
		return err
	}
	if !strings.HasPrefix(dev, "/dev/") {
		dev = "/dev/" + dev
	}
	if _, err := m.Run.Run(ctx, "ceph-volume", "lvm", "create", "--data", dev); err != nil {
		return fmt.Errorf("ceph-volume: %w", err)
	}
	return nil
}

// RemoveOSD takes OSD id out of the cluster. While Ceph still needs its data
// it is only marked out (data moves to the other disks); called again once
// that is done, it removes it and wipes the disk.
func (m *Manager) RemoveOSD(ctx context.Context, id int) (done bool, err error) {
	if err := m.needConfigured(); err != nil {
		return false, err
	}
	sid := fmt.Sprint(id)
	if _, err := m.ceph(ctx, "osd", "safe-to-destroy", sid); err != nil {
		if _, err := m.ceph(ctx, "osd", "out", sid); err != nil {
			return false, fmt.Errorf("osd out: %w", err)
		}
		return false, nil
	}
	_, _ = m.ceph(ctx, "osd", "out", sid)
	_, _ = m.Run.Run(ctx, "systemctl", "disable", "--now", "ceph-osd@"+sid)
	if _, err := m.ceph(ctx, "osd", "purge", sid, "--yes-i-really-mean-it"); err != nil {
		return false, fmt.Errorf("osd purge: %w", err)
	}
	if _, err := m.Run.Run(ctx, "ceph-volume", "lvm", "zap", "--osd-id", sid, "--destroy"); err != nil {
		return true, fmt.Errorf("the disk was taken out of Ceph but not wiped: %w", err)
	}
	return true, nil
}

func (m *Manager) confText() string {
	b, _ := os.ReadFile(m.ConfFile())
	return string(b)
}

// --- the whole cluster (run where the admin key is) ---

// CreatePool makes the pool for VM disks with size copies of every block.
func (m *Manager) CreatePool(ctx context.Context, size int) error {
	if err := m.needConfigured(); err != nil {
		return err
	}
	if size < 1 || size > 3 {
		return fmt.Errorf("%w: copies must be 1-3", ErrInvalid)
	}
	if out, err := m.ceph(ctx, "osd", "pool", "ls"); err == nil && hasLine(string(out), Pool) {
		return fmt.Errorf("%w: the pool %s already exists", ErrConflict, Pool)
	}
	minSize := 1
	if size == 3 {
		minSize = 2
	}
	var steps [][]string
	if size == 1 {
		steps = append(steps, []string{"config", "set", "global", "mon_allow_pool_size_one", "true"})
	}
	steps = append(steps,
		[]string{"osd", "pool", "create", Pool, "32"},
		[]string{"osd", "pool", "set", Pool, "size", fmt.Sprint(size), "--yes-i-really-mean-it"},
		[]string{"osd", "pool", "set", Pool, "min_size", fmt.Sprint(minSize)},
		[]string{"osd", "pool", "application", "enable", Pool, "rbd"},
	)
	for _, s := range steps {
		if _, err := m.ceph(ctx, s...); err != nil {
			return fmt.Errorf("ceph %s: %w", strings.Join(s[:min(3, len(s))], " "), err)
		}
	}
	if _, err := m.Run.Run(ctx, "rbd", "pool", "init", Pool); err != nil {
		return fmt.Errorf("rbd pool init: %w", err)
	}
	return nil
}

func hasLine(out, want string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// PoolReady reports whether the VM pool exists (Ceph storage can be used).
func (m *Manager) PoolReady(ctx context.Context) bool {
	if !m.Configured() || !m.exists("/usr/bin/rbd") {
		return false
	}
	out, err := m.ceph(ctx, "osd", "pool", "ls")
	return err == nil && hasLine(string(out), Pool)
}

// Status is the cluster's state (`ceph status`, `ceph osd tree`, `ceph df`).
func (m *Manager) Status(ctx context.Context) (Status, error) {
	if err := m.needConfigured(); err != nil {
		return Status{}, err
	}
	out, err := m.ceph(ctx, "status", "-f", "json")
	if err != nil {
		return Status{}, fmt.Errorf("Ceph doesn't answer: %w", err)
	}
	st, err := ParseStatus(out)
	if err != nil {
		return st, err
	}
	if out, err := m.ceph(ctx, "osd", "tree", "-f", "json"); err == nil {
		st.OSDs = ParseOSDTree(out)
	}
	if out, err := m.ceph(ctx, "df", "-f", "json"); err == nil {
		st.Pools = ParseDF(out)
	}
	return st, nil
}

// Mons reads the monitors from the cluster.
func (m *Manager) Mons(ctx context.Context) ([]Mon, string, error) {
	out, err := m.ceph(ctx, "mon", "dump", "-f", "json")
	if err != nil {
		return nil, "", err
	}
	return ParseMonDump(out)
}

// RefreshConf rewrites ceph.conf from the monitors the cluster has now (so a
// server can reach every monitor). It reports whether the file changed.
func (m *Manager) RefreshConf(ctx context.Context) (bool, error) {
	if !m.Configured() {
		return false, nil
	}
	mons, fsid, err := m.Mons(ctx)
	if err != nil || len(mons) == 0 {
		return false, err
	}
	cur := m.confText()
	net, err := NetworkOf(cur)
	if err != nil {
		return false, err
	}
	if f := FSIDOf(cur); f != "" && f != fsid {
		return false, fmt.Errorf("ceph.conf is for cluster %s, the monitors run %s", f, fsid)
	}
	next := Conf(fsid, net, mons)
	if next == cur {
		return false, nil
	}
	return true, writeAtomic(m.ConfFile(), next, 0o644)
}
