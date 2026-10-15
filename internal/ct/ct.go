// Package ct runs Linux containers (LXC), next to the KVM VMs — like Proxmox's
// "CT". Containers share the server's kernel, so they start in a second and
// use far less memory than VMs: the right thing for small, cheap servers.
//
// Every container is unprivileged (root inside is uid 100000 outside, so it
// is nobody on the host), lives on its own ZFS dataset cloned from a template
// (instant, takes no space until used), gets CPU and memory limits from
// cgroups and a disk limit from a ZFS quota. Templates are official
// linuxcontainers.org "cloud" images, so cloud-init sets the hostname, root
// password and SSH keys, like for VMs.
//
// Containers run in their own systemd units (p8s-ct-NAME), not inside p8sd,
// so restarting p8sd doesn't touch them.
package ct

import (
	"p8sos/internal/backup"
	"p8sos/internal/hostinfo"
	"p8sos/internal/ippool"
	"p8sos/internal/jobs"

	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"p8sos/internal/cloudinit"
	"p8sos/internal/execx"
	"p8sos/internal/vm"
)

// MapBase is where container uids/gids start on the host (root inside = 100000).
const MapBase = 100000

// Container is what the API shows.
type Container struct {
	Kind      string `json:"kind"` // always "ct"
	Name      string `json:"name"`
	State     string `json:"state"` // running, stopped, starting, stopping, unknown
	Template  string `json:"template"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memory_mib"`
	MemUsed   int    `json:"mem_used_mib,omitempty"` // in use now, file cache not counted
	DiskGB    int    `json:"disk_gb"`
	DiskUsed  int64  `json:"disk_used,omitempty"` // bytes
	IP        string `json:"ip,omitempty"`
	MAC       string `json:"mac"`
	Network   string `json:"network"` // nat, lan, public or vnet:NAME
	PublicIP  string `json:"public_ip,omitempty"`
	// Public is the whole lease: extra addresses, IPv6.
	Public    *ippool.Lease `json:"public,omitempty"`
	Owner     string        `json:"owner,omitempty"`
	Autostart bool          `json:"autostart"`
	Created   string        `json:"created"`
	User      string        `json:"user"` // login user (root)
	// Node / NodeID: the cluster server it runs on (set by the cluster).
	Node   string `json:"node,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

// Template is an installed container template.
type Template struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// CreateRequest makes a container.
type CreateRequest struct {
	Name          string   `json:"name"`
	Template      string   `json:"template"`
	VCPUs         int      `json:"vcpus"`
	MemoryMiB     int      `json:"memory_mib"`
	DiskGB        int      `json:"disk_gb"`
	Network       string   `json:"network"`
	IPPool        string   `json:"ip_pool"` // public address from this pool (network "public")
	Plan          string   `json:"plan"`    // P8S Cloud plan (sizes come from it; the API fills them in)
	Owner         string   `json:"owner"`
	RootPassword  string   `json:"root_password"`
	SSHKeys       []string `json:"ssh_keys"`
	Start         *bool    `json:"start"`
	Autostart     *bool    `json:"autostart"`
	NoDefaultKeys bool     `json:"no_default_keys,omitempty"` // a customer's container: not the host's keys
	Node          string   `json:"node,omitempty"`            // cluster server to make it on ("" = chosen)
}

// ResizeRequest changes limits (0 = unchanged). Disk can grow, or shrink down to what's used.
type ResizeRequest struct {
	VCPUs     int   `json:"vcpus"`
	MemoryMiB int   `json:"memory_mib"`
	DiskGB    int   `json:"disk_gb"`
	Autostart *bool `json:"autostart"`
}

// Snapshot of a container's disk.
type Snapshot struct {
	Name    string `json:"name"`
	Created string `json:"created"`
}

// Limits are the per-container ceilings.
type Limits struct{ MaxVCPUs, MaxMemoryMiB, MaxDiskGB int }

type meta struct {
	Template  string `json:"template"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memory_mib"`
	DiskGB    int    `json:"disk_gb"`
	MAC       string `json:"mac"`
	Network   string `json:"network"`
	Owner     string `json:"owner,omitempty"`
	Autostart bool   `json:"autostart"`
	Created   string `json:"created"`
	// NeedsSSH: the SSH server still has to be installed (first start).
	NeedsSSH  bool `json:"needs_ssh,omitempty"`
	RootLogin bool `json:"root_login,omitempty"` // a root password was set: allow root SSH logins
}

// Stdin runs a command with input (for chpasswd); tests replace it.
type Stdin func(ctx context.Context, input string, name string, args ...string) error

// Service manages containers.
type Service struct {
	bg        sync.WaitGroup // first-boot setups running in the background
	Run       execx.Runner
	Stdin     Stdin
	Pool      string // e.g. "tank"
	MountDir  string // container root filesystems, e.g. /var/lib/p8s/ct
	LXCDir    string // LXC configs, e.g. /var/lib/lxc
	MetaFile  string // data_dir/containers.json
	Bridge    string // NAT bridge, e.g. virbr0
	LANBridge string // home-network bridge, e.g. br0
	LANUp     func() bool
	Limits    Limits
	SSHKeys   []string // host default keys (admins' containers)
	Chown     func(path string, uid, gid int) error
	Now       func() time.Time
	// Backups is the VM backup store (container backups go under its Dir/_ct);
	// Jobs runs them. Either nil = no container backups.
	Backups *backup.Store
	Jobs    *jobs.Manager
	// Pools are the public IP pools (shared with VMs); BridgeUp checks a pool's bridge.
	Pools    *ippool.Store
	BridgeUp func(bridge string) bool
	// Refresh re-applies the firewall (public address pinning) after a restore; nil = none.
	Refresh func(ctx context.Context) error
	// AfterBackup is told about every finished backup (off-site copies); nil = nothing.
	AfterBackup func(kind, guest, id string)
	// VNets are the cluster's virtual networks ("vnet:NAME"); nil = none.
	VNets vm.VNets
	// Cmd runs streaming commands (moves); nil = exec.CommandContext.
	Cmd func(ctx context.Context, name string, args ...string) *exec.Cmd

	mu    sync.Mutex
	metas map[string]*meta
	rmu   sync.Mutex // HA copies (replica.go)
}

var reName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}[a-z0-9]$`)

func (s *Service) dataset(name string) string { return s.Pool + "/p8s/ct/" + name }
func (s *Service) tplRoot() string            { return s.Pool + "/p8s/ct-templates" }
func (s *Service) rootfs(name string) string  { return filepath.Join(s.MountDir, name) }
func (s *Service) confDir(name string) string { return filepath.Join(s.LXCDir, name) }
func unit(name string) string                 { return "p8s-ct-" + name }

// zfs runs zfs in the host's mount namespace: p8sd runs sandboxed (its own
// mount namespace), and a container's disk must be mounted where lxc-start
// (a separate systemd unit) sees it. Host mounts do show up inside p8sd.
func (s *Service) zfs(ctx context.Context, args ...string) ([]byte, error) {
	return s.Run.Run(ctx, "nsenter", append([]string{"--mount=/proc/1/ns/mnt", "--", "zfs"}, args...)...)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) chown(p string, uid, gid int) error {
	if s.Chown != nil {
		return s.Chown(p, uid, gid)
	}
	return os.Lchown(p, uid, gid)
}

// --- metadata (containers.json) ---

func (s *Service) load() error {
	if s.metas != nil {
		return nil
	}
	s.metas = map[string]*meta{}
	b, err := os.ReadFile(s.MetaFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.metas)
}

func (s *Service) save() error {
	b, _ := json.MarshalIndent(s.metas, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.MetaFile), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(s.MetaFile+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(s.MetaFile+".tmp", s.MetaFile)
}

func (s *Service) get(name string) (meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return meta{}, err
	}
	m, ok := s.metas[name]
	if !ok {
		return meta{}, fmt.Errorf("%w: container %q", vm.ErrNotFound, name)
	}
	return *m, nil
}

func (s *Service) put(name string, m *meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	if m == nil {
		delete(s.metas, name)
	} else {
		s.metas[name] = m
	}
	return s.save()
}

// Exists reports whether a container has this name (VMs share the namespace).
func (s *Service) Exists(name string) bool {
	_, err := s.get(name)
	return err == nil
}

// --- templates ---

// Templates lists installed container templates (datasets under ct-templates
// with a @base snapshot; the label is the p8s:label property).
func (s *Service) Templates(ctx context.Context) []Template {
	out, err := s.Run.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name,p8s:label", "-r", "-d", "2", s.tplRoot())
	if err != nil {
		return []Template{}
	}
	l := []Template{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, "\t", 2)
		ds, snap, ok := strings.Cut(f[0], "@")
		if !ok || snap != "base" || !strings.HasPrefix(ds, s.tplRoot()+"/") {
			continue
		}
		t := Template{Name: strings.TrimPrefix(ds, s.tplRoot()+"/")}
		if len(f) > 1 && f[1] != "-" {
			t.Label = f[1]
		}
		if t.Label == "" {
			t.Label = t.Name
		}
		l = append(l, t)
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	return l
}

func (s *Service) hasTemplate(ctx context.Context, name string) bool {
	for _, t := range s.Templates(ctx) {
		if t.Name == name {
			return true
		}
	}
	return false
}

// --- create ---

func newMAC() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
}

func (s *Service) bridgeFor(name, network string) (string, error) {
	switch network {
	case vm.NetPublic:
		l, ok := s.lease(name)
		if !ok {
			return "", fmt.Errorf("%w: container %q has no public address any more", vm.ErrInvalid, name)
		}
		return l.Bridge, nil
	case "", vm.NetNAT:
		return s.Bridge, nil
	case vm.NetLAN:
		if s.LANBridge == "" || (s.LANUp != nil && !s.LANUp()) {
			return "", fmt.Errorf("%w: the home network isn't set up on this server (Server page → Home network)", vm.ErrInvalid)
		}
		return s.LANBridge, nil
	}
	if vm.IsVNet(network) {
		if s.VNets == nil {
			return "", fmt.Errorf("%w: virtual networks aren't available on this server", vm.ErrInvalid)
		}
		return s.VNets.BridgeOf(context.Background(), strings.TrimPrefix(network, vm.NetVNetPrefix))
	}
	return "", fmt.Errorf("%w: network must be nat, lan, public or vnet:NAME", vm.ErrInvalid)
}

func (s *Service) validate(r *CreateRequest) error {
	if !reName.MatchString(r.Name) {
		return fmt.Errorf("%w: name: 2-49 lowercase letters, digits and dashes, starting with a letter", vm.ErrInvalid)
	}
	if r.VCPUs == 0 {
		r.VCPUs = 1
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = 512
	}
	if r.DiskGB == 0 {
		r.DiskGB = 8
	}
	if r.VCPUs < 1 || (s.Limits.MaxVCPUs > 0 && r.VCPUs > s.Limits.MaxVCPUs) {
		return fmt.Errorf("%w: vcpus 1-%d", vm.ErrInvalid, max(s.Limits.MaxVCPUs, 1))
	}
	if r.MemoryMiB < 64 || (s.Limits.MaxMemoryMiB > 0 && r.MemoryMiB > s.Limits.MaxMemoryMiB) {
		return fmt.Errorf("%w: memory 64-%d MiB", vm.ErrInvalid, s.Limits.MaxMemoryMiB)
	}
	if r.DiskGB < 1 || (s.Limits.MaxDiskGB > 0 && r.DiskGB > s.Limits.MaxDiskGB) {
		return fmt.Errorf("%w: disk 1-%d GB", vm.ErrInvalid, s.Limits.MaxDiskGB)
	}
	if r.RootPassword != "" && (len(r.RootPassword) < 8 || len(r.RootPassword) > 128 || strings.ContainsAny(r.RootPassword, "\n\r:")) {
		return fmt.Errorf("%w: root password: 8-128 characters, no line breaks or colons", vm.ErrInvalid)
	}
	for _, k := range r.SSHKeys {
		if strings.ContainsAny(k, "\n\r") || !strings.HasPrefix(strings.TrimSpace(k), "ssh-") && !strings.HasPrefix(strings.TrimSpace(k), "ecdsa-") {
			return fmt.Errorf("%w: ssh key: one key per entry (ssh-ed25519 …)", vm.ErrInvalid)
		}
	}
	return nil
}

// Create makes (and by default starts) a container.
func (s *Service) Create(ctx context.Context, r CreateRequest) (Container, error) {
	if err := s.validate(&r); err != nil {
		return Container{}, err
	}
	if err := s.checkPool(&r); err != nil {
		return Container{}, err
	}
	if r.Network != vm.NetPublic {
		if _, err := s.bridgeFor(r.Name, r.Network); err != nil {
			return Container{}, err
		}
	}
	if r.Network == "" {
		r.Network = vm.NetNAT
	}
	if s.Exists(r.Name) {
		return Container{}, fmt.Errorf("%w: container %q", vm.ErrExists, r.Name)
	}
	if !s.hasTemplate(ctx, r.Template) {
		return Container{}, fmt.Errorf("%w: no container template %q (add one on the Server page)", vm.ErrInvalid, r.Template)
	}
	var lease *ippool.Lease
	if r.Network == vm.NetPublic {
		l, err := s.takeIP(r.IPPool, r.Name, "")
		if err != nil {
			return Container{}, err
		}
		lease = &l
	}
	br, err := s.bridgeFor(r.Name, r.Network)
	if err != nil {
		s.releaseIP(r.Name)
		return Container{}, err
	}
	keys := r.SSHKeys
	if len(keys) == 0 && !r.NoDefaultKeys {
		keys = s.SSHKeys
	}
	m := &meta{Template: r.Template, VCPUs: r.VCPUs, MemoryMiB: r.MemoryMiB, DiskGB: r.DiskGB, MAC: newMAC(),
		Network: r.Network, Owner: r.Owner, Autostart: r.Autostart == nil || *r.Autostart, Created: s.now().UTC().Format(time.RFC3339)}

	ds := s.dataset(r.Name)
	if _, err := s.zfs(ctx, "clone", "-p", "-o", "mountpoint="+s.rootfs(r.Name), "-o", fmt.Sprintf("refquota=%dG", r.DiskGB),
		s.tplRoot()+"/"+r.Template+"@base", ds); err != nil {
		s.releaseIP(r.Name)
		return Container{}, fmt.Errorf("create the container's disk: %w", err)
	}
	undo := func() {
		_, _ = s.zfs(context.Background(), "destroy", "-r", ds)
		_ = os.RemoveAll(s.confDir(r.Name))
		s.releaseIP(r.Name)
	}
	if err := s.seed(r.Name, keys, r.RootPassword); err != nil {
		undo()
		return Container{}, fmt.Errorf("cloud-init seed: %w", err)
	}
	// Set up what we can by writing files (works for every distro, also
	// without cloud-init): hostname, root password, SSH keys. The SSH server
	// itself is installed on the first start.
	if err := s.prepare(r.Name, keys, r.RootPassword); err != nil {
		undo()
		return Container{}, fmt.Errorf("prepare the container: %w", err)
	}
	if lease != nil {
		if err := s.writeNet(r.Name, m.MAC, lease); err != nil {
			undo()
			return Container{}, fmt.Errorf("network setup: %w", err)
		}
	}
	m.NeedsSSH, m.RootLogin = true, r.RootPassword != ""
	if err := s.writeConfig(r.Name, m, br); err != nil {
		undo()
		return Container{}, err
	}
	if err := s.put(r.Name, m); err != nil {
		undo()
		return Container{}, err
	}
	if r.Start == nil || *r.Start {
		if err := s.start(ctx, r.Name); err != nil {
			return s.view(ctx, r.Name, *m), fmt.Errorf("the container was made but didn't start: %w", err)
		}
		s.firstBoot(r.Name)
	}
	return s.view(ctx, r.Name, *m), nil
}

// writeAs writes a file inside a container's root filesystem, owned by the
// container's root (mapped uid), creating folders the same way.
func (s *Service) writeAs(name, rel string, data []byte, mode os.FileMode) error {
	root := s.rootfs(name)
	p := filepath.Join(root, rel)
	if !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return fmt.Errorf("bad path %q", rel)
	}
	// create parents one by one, refusing symlinks planted in the template
	dir := root
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		dir = filepath.Join(dir, part)
		if st, err := os.Lstat(dir); err == nil {
			if !st.IsDir() {
				return fmt.Errorf("%s is not a folder", dir)
			}
			continue
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		if err := s.chown(dir, MapBase, MapBase); err != nil {
			return err
		}
	}
	if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", p)
	}
	if err := os.WriteFile(p, data, mode); err != nil {
		return err
	}
	return s.chown(p, MapBase, MapBase)
}

// seed writes a NoCloud seed for cloud-init inside the container.
func (s *Service) seed(name string, keys []string, password string) error {
	var ud strings.Builder
	ud.WriteString("#cloud-config\n")
	fmt.Fprintf(&ud, "hostname: %s\nmanage_etc_hosts: true\n", name)
	if len(keys) > 0 {
		ud.WriteString("disable_root: false\nssh_authorized_keys:\n")
		for _, k := range keys {
			fmt.Fprintf(&ud, "  - %q\n", strings.TrimSpace(k))
		}
	}
	if password != "" {
		fmt.Fprintf(&ud, "disable_root: false\nssh_pwauth: true\nchpasswd:\n  expire: false\n  users:\n    - {name: root, password: %q, type: hash}\n",
			cloudinit.HashPassword(password))
		ud.WriteString("write_files:\n  - path: /etc/ssh/sshd_config.d/10-p8s-root.conf\n    permissions: '0644'\n" +
			"    content: |\n      # P8S OS: root may log in over SSH with the password chosen when the container was made.\n" +
			"      PermitRootLogin yes\n      PasswordAuthentication yes\n")
	}
	// SSH server, so the container is reachable like a VPS
	ud.WriteString("packages:\n  - openssh-server\nruncmd:\n  - [sh, -c, 'systemctl enable --now ssh 2>/dev/null || systemctl enable --now sshd 2>/dev/null || (rc-update add sshd && rc-service sshd start) || true']\n")
	md := fmt.Sprintf("instance-id: p8s-ct-%s-%d\nlocal-hostname: %s\n", name, s.now().Unix(), name)
	if err := s.writeAs(name, "var/lib/cloud/seed/nocloud/user-data", []byte(ud.String()), 0o600); err != nil {
		return err
	}
	return s.writeAs(name, "var/lib/cloud/seed/nocloud/meta-data", []byte(md), 0o600)
}

func (s *Service) config(name string, m *meta, bridge string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# P8S OS container %s (written by p8sd; changes here are overwritten)\n", name)
	b.WriteString("lxc.include = /usr/share/lxc/config/common.conf\n")
	b.WriteString("lxc.include = /usr/share/lxc/config/userns.conf\n")
	b.WriteString("lxc.arch = linux64\n")
	fmt.Fprintf(&b, "lxc.uts.name = %s\n", name)
	fmt.Fprintf(&b, "lxc.idmap = u 0 %d 65536\nlxc.idmap = g 0 %d 65536\n", MapBase, MapBase)
	fmt.Fprintf(&b, "lxc.rootfs.path = dir:%s\n", s.rootfs(name))
	b.WriteString("lxc.apparmor.profile = generated\nlxc.apparmor.allow_nesting = 0\n")
	fmt.Fprintf(&b, "lxc.net.0.type = veth\nlxc.net.0.link = %s\nlxc.net.0.flags = up\nlxc.net.0.name = eth0\nlxc.net.0.hwaddr = %s\n", bridge, m.MAC)
	fmt.Fprintf(&b, "lxc.net.0.veth.pair = %s\n", HostPort(name)) // fixed name: the firewall pins public addresses to it
	if strings.HasPrefix(bridge, "vxb") {                         // a virtual network: frames travel inside the cluster tunnel
		b.WriteString("lxc.net.0.mtu = 1230\n")
	}
	if l, ok := s.lease(name); ok && m.Network == vm.NetPublic {
		fmt.Fprintf(&b, "lxc.net.0.ipv4.address = %s/%d\n", l.IP, l.Prefix)
		if !l.OnLink {
			fmt.Fprintf(&b, "lxc.net.0.ipv4.gateway = %s\n", l.Gateway)
		}
		for _, x := range l.Extra {
			fmt.Fprintf(&b, "lxc.net.0.ipv4.address = %s/%d\n", x, l.Prefix)
		}
		if l.IP6 != "" {
			fmt.Fprintf(&b, "lxc.net.0.ipv6.address = %s/%d\n", l.IP6, l.Prefix6)
			fmt.Fprintf(&b, "lxc.net.0.ipv6.gateway = %s\n", l.Gateway6)
		}
	}
	fmt.Fprintf(&b, "lxc.cgroup2.memory.max = %dM\n", m.MemoryMiB)
	fmt.Fprintf(&b, "lxc.cgroup2.memory.swap.max = %dM\n", min(m.MemoryMiB, 512))
	fmt.Fprintf(&b, "lxc.cgroup2.cpu.max = %d 100000\n", m.VCPUs*100000)
	b.WriteString("lxc.cgroup2.pids.max = 8192\n")
	b.WriteString("lxc.start.auto = 0\n") // p8sd starts them (autostart) after boot
	fmt.Fprintf(&b, "lxc.log.file = %s\nlxc.log.level = WARN\n", filepath.Join(s.confDir(name), "lxc.log"))
	return b.String()
}

func (s *Service) writeConfig(name string, m *meta, bridge string) error {
	if err := os.MkdirAll(s.confDir(name), 0o750); err != nil {
		return err
	}
	p := filepath.Join(s.confDir(name), "config")
	if err := os.WriteFile(p+".tmp", []byte(s.config(name, m, bridge)), 0o640); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// --- state ---

func (s *Service) state(ctx context.Context, name string) string {
	out, err := s.Run.Run(ctx, "lxc-info", "-n", name, "-sH")
	if err != nil {
		return "unknown"
	}
	switch st := strings.ToLower(strings.TrimSpace(string(out))); st {
	case "running", "stopped", "starting", "stopping", "aborting", "freezing", "frozen", "thawed":
		return st
	}
	return "unknown"
}

func (s *Service) ip(ctx context.Context, name string) string {
	out, err := s.Run.Run(ctx, "lxc-info", "-n", name, "-iH")
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.Count(f, ".") == 3 {
			return f
		}
	}
	return ""
}

func (s *Service) used(ctx context.Context, name string) int64 {
	out, err := s.Run.Run(ctx, "zfs", "get", "-Hp", "-o", "value", "used", s.dataset(name))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

func (s *Service) view(ctx context.Context, name string, m meta) Container {
	c := Container{Kind: "ct", Name: name, State: s.state(ctx, name), Template: m.Template, VCPUs: m.VCPUs, MemoryMiB: m.MemoryMiB,
		DiskGB: m.DiskGB, MAC: m.MAC, Network: m.Network, Owner: m.Owner, Autostart: m.Autostart, Created: m.Created, User: "root"}
	if c.State == "running" {
		c.IP = s.ip(ctx, name)
		c.MemUsed = hostinfo.CgroupMemMiB("sys/fs/cgroup/system.slice/" + unit(name) + ".service")
	}
	if l, ok := s.lease(name); ok && m.Network == vm.NetPublic {
		c.PublicIP = l.IP
		c.Public = &l
	}
	c.DiskUsed = s.used(ctx, name)
	return c
}

// Get returns one container.
func (s *Service) Get(ctx context.Context, name string) (Container, error) {
	m, err := s.get(name)
	if err != nil {
		return Container{}, err
	}
	return s.view(ctx, name, m), nil
}

// List returns all containers.
func (s *Service) List(ctx context.Context) ([]Container, error) {
	s.mu.Lock()
	if err := s.load(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	names := make([]string, 0, len(s.metas))
	ms := map[string]meta{}
	for n, m := range s.metas {
		names = append(names, n)
		ms[n] = *m
	}
	s.mu.Unlock()
	sort.Strings(names)
	out := []Container{}
	for _, n := range names {
		out = append(out, s.view(ctx, n, ms[n]))
	}
	return out, nil
}

// --- power ---

func (s *Service) start(ctx context.Context, name string) error {
	_, _ = s.Run.Run(ctx, "systemctl", "reset-failed", unit(name)) // a unit left from a crash
	if _, err := s.Run.Run(ctx, "systemd-run", "--unit="+unit(name), "--collect", "--quiet",
		"--property=Delegate=yes", "--property=KillMode=mixed", "--property=TimeoutStopSec=90",
		"--description=P8S OS container "+name, "lxc-start", "-F", "-n", name); err != nil {
		return err
	}
	for i := 0; i < 120; i++ { // up to 30 s
		switch s.state(ctx, name) {
		case "running":
			return nil
		case "stopped", "unknown":
			// lxc-start may still be setting up; give up only once its unit has ended
			out, _ := s.Run.Run(ctx, "systemctl", "is-active", unit(name))
			if st := strings.TrimSpace(string(out)); i > 8 && st != "active" && st != "activating" {
				log, _ := os.ReadFile(filepath.Join(s.confDir(name), "lxc.log"))
				lines := strings.Split(strings.TrimSpace(string(log)), "\n")
				if len(lines) > 5 {
					lines = lines[len(lines)-5:]
				}
				return fmt.Errorf("%w: container %s did not start: %s", vm.ErrConflict, name, strings.Join(lines, " | "))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("%w: container %s is still starting; check again in a minute", vm.ErrConflict, name)
}

// Start powers a container on.
func (s *Service) Start(ctx context.Context, name string) (Container, error) {
	m, err := s.get(name)
	if err != nil {
		return Container{}, err
	}
	if s.state(ctx, name) == "running" {
		return s.view(ctx, name, m), nil
	}
	// re-write the config: limits or the bridge may have changed
	br, err := s.bridgeFor(name, m.Network)
	if err != nil {
		return Container{}, err
	}
	if err := s.writeConfig(name, &m, br); err != nil {
		return Container{}, err
	}
	if err := s.start(ctx, name); err != nil {
		return Container{}, err
	}
	if m.NeedsSSH {
		s.firstBoot(name)
	}
	return s.view(ctx, name, m), nil
}

// Stop shuts a container down (force = immediately).
func (s *Service) Stop(ctx context.Context, name string, force bool) (Container, error) {
	m, err := s.get(name)
	if err != nil {
		return Container{}, err
	}
	if s.state(ctx, name) != "stopped" {
		args := []string{"-n", name, "-t", "60"}
		if force {
			args = []string{"-n", name, "-k"}
		}
		if _, err := s.Run.Run(ctx, "lxc-stop", args...); err != nil && s.state(ctx, name) != "stopped" {
			return Container{}, err
		}
	}
	return s.view(ctx, name, m), nil
}

// Reboot restarts a running container.
func (s *Service) Reboot(ctx context.Context, name string) (Container, error) {
	if _, err := s.Stop(ctx, name, false); err != nil {
		return Container{}, err
	}
	return s.Start(ctx, name)
}

// Delete removes a container and its disk (and snapshots).
func (s *Service) Delete(ctx context.Context, name string) error {
	if _, err := s.get(name); err != nil {
		return err
	}
	if s.state(ctx, name) != "stopped" {
		_, _ = s.Run.Run(ctx, "lxc-stop", "-n", name, "-k")
	}
	if _, err := s.zfs(ctx, "destroy", "-r", s.dataset(name)); err != nil && !strings.Contains(err.Error(), "does not exist") {
		return err
	}
	_ = os.RemoveAll(s.confDir(name))
	s.releaseIP(name)
	return s.put(name, nil)
}

// Resize changes CPU, memory and disk limits (live when running).
func (s *Service) Resize(ctx context.Context, name string, r ResizeRequest) (Container, error) {
	m, err := s.get(name)
	if err != nil {
		return Container{}, err
	}
	n := m
	if r.VCPUs != 0 {
		n.VCPUs = r.VCPUs
	}
	if r.MemoryMiB != 0 {
		n.MemoryMiB = r.MemoryMiB
	}
	if r.DiskGB != 0 {
		n.DiskGB = r.DiskGB
	}
	if r.Autostart != nil {
		n.Autostart = *r.Autostart
	}
	chk := CreateRequest{Name: name, VCPUs: n.VCPUs, MemoryMiB: n.MemoryMiB, DiskGB: n.DiskGB}
	if err := s.validate(&chk); err != nil {
		return Container{}, err
	}
	if n.DiskGB != m.DiskGB {
		if _, err := s.zfs(ctx, "set", fmt.Sprintf("refquota=%dG", n.DiskGB), s.dataset(name)); err != nil {
			return Container{}, fmt.Errorf("%w: disk: %v (it can't be smaller than what is used)", vm.ErrInvalid, err)
		}
	}
	running := s.state(ctx, name) == "running"
	if running {
		_, _ = s.Run.Run(ctx, "lxc-cgroup", "-n", name, "memory.max", fmt.Sprintf("%d", int64(n.MemoryMiB)<<20))
		_, _ = s.Run.Run(ctx, "lxc-cgroup", "-n", name, "cpu.max", fmt.Sprintf("%d 100000", n.VCPUs*100000))
	}
	br, err := s.bridgeFor(name, n.Network)
	if err != nil {
		br = s.Bridge
	}
	if err := s.writeConfig(name, &n, br); err != nil {
		return Container{}, err
	}
	if err := s.put(name, &n); err != nil {
		return Container{}, err
	}
	return s.view(ctx, name, n), nil
}

// SetOwner hands a container to a user ("" = admins only).
func (s *Service) SetOwner(ctx context.Context, name, owner string) (Container, error) {
	m, err := s.get(name)
	if err != nil {
		return Container{}, err
	}
	m.Owner = owner
	if err := s.put(name, &m); err != nil {
		return Container{}, err
	}
	return s.view(ctx, name, m), nil
}

// SetPassword sets a user's password inside a running container.
func (s *Service) SetPassword(ctx context.Context, name, user, password string) error {
	if _, err := s.get(name); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(user) {
		return fmt.Errorf("%w: user name", vm.ErrInvalid)
	}
	if len(password) < 8 || len(password) > 128 || strings.ContainsAny(password, "\n\r:") {
		return fmt.Errorf("%w: password: 8-128 characters, no line breaks or colons", vm.ErrInvalid)
	}
	if s.state(ctx, name) != "running" {
		return fmt.Errorf("%w: start the container first", vm.ErrConflict)
	}
	in := s.Stdin
	if in == nil {
		in = runStdin
	}
	return in(ctx, user+":"+password+"\n", "lxc-attach", "-n", name, "--clear-env", "--", "chpasswd")
}

func runStdin(ctx context.Context, input, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- snapshots ---

var reSnap = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$`)

// Snapshots lists a container's snapshots, oldest first.
func (s *Service) Snapshots(ctx context.Context, name string) ([]Snapshot, error) {
	if _, err := s.get(name); err != nil {
		return nil, err
	}
	out, err := s.Run.Run(ctx, "zfs", "list", "-H", "-p", "-t", "snapshot", "-o", "name,creation", "-s", "creation", "-d", "1", s.dataset(name))
	if err != nil {
		return nil, err
	}
	l := []Snapshot{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		_, snap, ok := strings.Cut(f[0], "@")
		if !ok || strings.HasPrefix(snap, RepSnapPrefix) { // HA copy points are internal
			continue
		}
		sec, _ := strconv.ParseInt(f[1], 10, 64)
		l = append(l, Snapshot{Name: snap, Created: time.Unix(sec, 0).UTC().Format(time.RFC3339)})
	}
	return l, nil
}

// CreateSnapshot takes a snapshot (instant; also while running).
func (s *Service) CreateSnapshot(ctx context.Context, name, snap string) (Snapshot, error) {
	if _, err := s.get(name); err != nil {
		return Snapshot{}, err
	}
	if snap == "" {
		snap = s.now().UTC().Format("2006-01-02_15-04-05")
	}
	if !reSnap.MatchString(snap) {
		return Snapshot{}, fmt.Errorf("%w: snapshot name: letters, digits, . _ -", vm.ErrInvalid)
	}
	if _, err := s.zfs(ctx, "snapshot", s.dataset(name)+"@"+snap); err != nil {
		if strings.Contains(err.Error(), "exists") {
			return Snapshot{}, fmt.Errorf("%w: snapshot %q", vm.ErrExists, snap)
		}
		return Snapshot{}, err
	}
	return Snapshot{Name: snap, Created: s.now().UTC().Format(time.RFC3339)}, nil
}

// Rollback returns the container to a snapshot (it must be stopped).
func (s *Service) Rollback(ctx context.Context, name, snap string, destroyNewer bool) (Container, error) {
	m, err := s.get(name)
	if err != nil {
		return Container{}, err
	}
	if !reSnap.MatchString(snap) {
		return Container{}, fmt.Errorf("%w: snapshot name", vm.ErrInvalid)
	}
	if s.state(ctx, name) != "stopped" {
		return Container{}, fmt.Errorf("%w: stop the container first", vm.ErrConflict)
	}
	// HA copy points newer than the snapshot would block the rollback: drop
	// them (the next copy is then a full one)
	s.dropRepSnapsAfter(ctx, name, snap)
	args := []string{"rollback"}
	if destroyNewer {
		args = append(args, "-r")
	}
	if _, err := s.zfs(ctx, append(args, s.dataset(name)+"@"+snap)...); err != nil {
		if strings.Contains(err.Error(), "more recent snapshots") {
			return Container{}, fmt.Errorf("%w: newer snapshots exist; roll back with destroy_newer to remove them", vm.ErrConflict)
		}
		return Container{}, err
	}
	return s.view(ctx, name, m), nil
}

// DeleteSnapshot removes a snapshot.
func (s *Service) DeleteSnapshot(ctx context.Context, name, snap string) error {
	if _, err := s.get(name); err != nil {
		return err
	}
	if !reSnap.MatchString(snap) {
		return fmt.Errorf("%w: snapshot name", vm.ErrInvalid)
	}
	_, err := s.zfs(ctx, "destroy", s.dataset(name)+"@"+snap)
	return err
}

// Autostart starts the containers marked autostart (after the server boots).
func (s *Service) Autostart(ctx context.Context, onErr func(string, error)) {
	l, err := s.List(ctx)
	if err != nil {
		return
	}
	for _, c := range l {
		if c.Autostart && c.State == "stopped" {
			if _, err := s.Start(ctx, c.Name); err != nil && onErr != nil {
				onErr(c.Name, err)
			}
		}
	}
}
