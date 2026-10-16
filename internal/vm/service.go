// Package vm orchestrates storage, cloud-init and the hypervisor into VM
// lifecycle operations. It is the only layer the API talks to.
package vm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/cloudinit"
	"p8sos/internal/config"
	"p8sos/internal/firewall"
	"p8sos/internal/ippool"
	"p8sos/internal/isos"
	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrInvalid  = errors.New("invalid request")
	ErrConflict = errors.New("conflict")
)

// Domains managed by p8sd carry this prefix so we never touch VMs created by hand.
const Prefix = "p8s-"

var reName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}[a-z0-9]$`)

type Disks interface {
	CloneDisk(ctx context.Context, snapshot, vm string, sizeGB int) (string, error)
	DestroyDisk(ctx context.Context, vm string) error
	SnapshotExists(ctx context.Context, snapshot string) bool
}

type Seeds interface {
	Build(ctx context.Context, vm string, s cloudinit.Spec) (string, error)
	Remove(vm string) error
}

type Service struct {
	HV        virt.Hypervisor
	Disks     Disks
	Seeds     Seeds
	Templates []config.Template
	// TemplatesFile keeps the templates made from VMs (JSON); "" = none.
	TemplatesFile string
	Bridge        string // NAT bridge (network "nat", the default)
	LANBridge     string // bridge onto the home network (network "lan"); empty = not set up
	// VLANs makes the bridges of tagged VLANs on the home network; nil = no VLANs.
	VLANs VLANs
	// VNets are the cluster's virtual networks (nil: none).
	VNets VNets
	// BridgeUp reports whether a host bridge exists; nil means "assume yes".
	BridgeUp func(name string) bool
	// TPMReady reports whether the software TPM (swtpm) is installed; nil = yes.
	TPMReady func() bool
	Limits   Limits
	SSHKeys  []string // default keys for creates that pass none
	// AgentRetry is the pause between guest-agent retries; 0 = 3s (tests shorten it).
	AgentRetry time.Duration
	// AgentWait is how long to keep retrying; 0 = 45s.
	AgentWait time.Duration
	Log       *slog.Logger

	Backups *backup.Store // nil = backups off
	ISOs    *isos.Library // installer images; nil = ISO installs off
	Pools   *ippool.Store // public IP pools; nil = none
	// PoolsShared: this server is in a cluster, whose servers share the
	// master's pools — a VM's public address can move with it.
	PoolsShared  func() bool
	Jobs         *jobs.Manager // runs backups and restores in the background
	ScheduleFile string        // daily-backup settings per VM (JSON); empty = in memory only
	FirewallFile string        // per-VM firewall settings (JSON); empty = in memory only
	// Firewall loads the per-VM firewall rules (nftables); nil = not enforced (tests).
	Firewall func(ctx context.Context, vms []firewall.VM) error
	// OtherGuests lists non-VM guests (containers) that share the firewall
	// table; their settings are kept under their Name (e.g. "ct:web2").
	OtherGuests func(ctx context.Context) ([]firewall.VM, error)
	// OtherBackup runs the daily backup of a non-VM guest whose schedule is
	// kept under "ct:NAME" (containers); nil = such entries are skipped.
	OtherBackup func(ctx context.Context, key string, keep int) error
	// AfterBackup is told about every finished backup (off-site copies); nil = nothing.
	AfterBackup func(kind, guest, id string)
	// ImportDir holds files to import (Proxmox backups, disk images); empty = imports off.
	ImportDir string
	// NVRAMDir is where libvirt keeps UEFI variables (<vm>_VARS.fd); empty =
	// /var/lib/libvirt/qemu/nvram.
	NVRAMDir string
	// TPMDir is where libvirt keeps software TPM state; empty = /var/lib/libvirt/swtpm.
	TPMDir string
	// ReplicaDir keeps the settings of VM copies held here for high availability.
	ReplicaDir string
	// HostDevs lists the server's PCI/USB devices for passthrough; nil = off.
	HostDevs HostDevices
	// StateDir keeps the saved memory of snapshots "with RAM" (one folder
	// per VM); empty = memory snapshots off.
	StateDir string

	locks    sync.Map // name -> *sync.Mutex; serializes ops per VM
	memSched sync.Map // backup schedules when ScheduleFile is empty (tests)
	memFW    sync.Map // firewall settings when FirewallFile is empty (tests)
}

type Limits struct{ MaxVCPUs, MaxMemoryMiB, MaxDiskGB int }

type VM struct {
	Name string `json:"name"`
	// LinkedTo: this VM is a linked clone of that VM (shares its disk blocks);
	// LinkedClones: VMs that are linked clones of this one. Only in Get.
	LinkedTo     string     `json:"linked_to,omitempty"`
	LinkedClones []string   `json:"linked_clones,omitempty"`
	Node         string     `json:"node,omitempty"`    // server it runs on (clusters)
	NodeID       string     `json:"node_id,omitempty"` // n1 = master
	State        virt.State `json:"state"`
	// MemUsedMiB is the RAM in use inside the guest (filled by the API from
	// the balloon statistics; 0 = not known yet).
	MemUsedMiB int    `json:"mem_used_mib,omitempty"`
	Template   string `json:"template"`
	User       string `json:"user"`                  // login user with the SSH keys (debian, ubuntu, rocky, ...)
	ISO        string `json:"iso,omitempty"`         // installer CD still in the drive
	OSType     string `json:"os_type,omitempty"`     // "windows" (BIOS) / "windows11" (UEFI, Secure Boot, TPM)
	Drivers    string `json:"drivers_iso,omitempty"` // drivers CD (virtio-win) still in the drive
	VCPUs      int    `json:"vcpus"`
	MemoryMiB  int    `json:"memory_mib"`
	DiskGB     int    `json:"disk_gb"`
	MAC        string `json:"mac"`
	Network    string `json:"network"`        // "nat" (private, behind the host) or "lan" (home network)
	VLAN       int    `json:"vlan,omitempty"` // tagged VLAN on the home network (0 = none)
	IP         string `json:"ip"`             // first IPv4; empty while booting or stopped
	Autostart  bool   `json:"autostart"`      // starts when the host boots
	UUID       string `json:"uuid"`
	CreatedAt  string `json:"created_at"`
	Owner      string `json:"owner,omitempty"` // P8S user it belongs to; empty = admins only
	// Access is set by the API for a VM shared with the caller: "viewer" or "user".
	Access string `json:"access,omitempty"`
	// Public is the VM's address from an IP pool (network "public").
	Public *ippool.Lease `json:"public,omitempty"`
	// Disks are the data disks next to the root disk (DiskGB).
	Disks []Disk `json:"disks,omitempty"`
	// NICs are the network cards after the first one (MAC/Network/VLAN).
	NICs []NIC `json:"nics,omitempty"`
	// PCI and USB are the server's devices given to the VM (passthrough).
	PCI []string `json:"pci,omitempty"`
	USB []string `json:"usb,omitempty"`
	// Hot-plug ceilings: vCPUs / RAM can be raised up to these while it runs (0 = off).
	HotCPUs   int `json:"hot_cpus,omitempty"`
	HotMemMiB int `json:"hot_mem_mib,omitempty"`
	// IOLimits caps its disk and network speed (each disk, each card).
	IOLimits *virt.IOLimits `json:"io_limits,omitempty"`
	// Storage holds the VM's disks: empty = this server's own pool, else a
	// network storage (iSCSI).
	Storage string `json:"storage,omitempty"`
	// CPU the VM runs with: "host" (this server's CPU as it is) or a model name
	// (e.g. the cluster's common CPU, for live moves between different CPUs).
	CPU string `json:"cpu"`
}

type CreateRequest struct {
	Name      string   `json:"name"`
	Node      string   `json:"node,omitempty"` // cluster server to put it on (id or name); "" = the best one
	Template  string   `json:"template"`
	VCPUs     int      `json:"vcpus"`
	MemoryMiB int      `json:"memory_mib"`
	DiskGB    int      `json:"disk_gb"`
	SSHKeys   []string `json:"ssh_keys"`
	UserData  string   `json:"user_data,omitempty"`
	Network   string   `json:"network,omitempty"` // "nat" (default), "lan", or "public" (with ip_pool)
	VLAN      int      `json:"vlan,omitempty"`    // with "lan": put it on this tagged VLAN (1-4094)
	// Storage puts the disks on a network storage (iSCSI); "" / "local" = this server's pool.
	Storage string `json:"storage,omitempty"`
	// IPPool gives the VM a public address from this pool (static, via cloud-init).
	IPPool string `json:"ip_pool,omitempty"`
	// Plan is a priced size (P8S Cloud); the API fills in CPUs/RAM/disk from it.
	Plan string `json:"plan,omitempty"`
	// ISO installs by hand from an image in the ISO library instead of a
	// template: empty disk of DiskGB, boots the CD, install via the console.
	ISO string `json:"iso,omitempty"`
	// OSType with ISO: "linux" (default, virtio), "windows" (BIOS; Windows 10 and
	// older) or "windows11" (UEFI + Secure Boot + TPM 2.0; Windows 11, Server 2022/2025).
	OSType string `json:"os_type,omitempty"`
	// DriversISO is a second CD from the library, e.g. virtio-win (Windows drivers).
	DriversISO string `json:"drivers_iso,omitempty"`
	// RootPassword sets root's password (8-128 chars). It is stored only as a
	// SHA-512 crypt hash inside the VM's seed and never returned by the API.
	RootPassword string `json:"root_password,omitempty"`
	// SSHPasswordAuth allows SSH logins as root with RootPassword (default true when one is set).
	SSHPasswordAuth *bool `json:"ssh_password_auth,omitempty"`
	// NoDefaultKeys leaves out the host's default SSH keys (customer VMs: the
	// provider's key doesn't belong in them). Set by the API, not by clients.
	NoDefaultKeys bool `json:"-"`
	// Owner is the P8S user the VM is made for (set by the API from the login).
	Owner     string `json:"owner,omitempty"`
	Start     *bool  `json:"start,omitempty"`     // default true
	Autostart *bool  `json:"autostart,omitempty"` // default true: start when the host boots
}

// Network names a VM can be attached to.
const (
	NetNAT = "nat" // private network behind the host (libvirt virbr0)
	NetLAN = "lan" // bridged onto the home network; the router gives the IP
)

// LANAvailable reports whether VMs can be put on the home network.
func (s *Service) LANAvailable() bool {
	return s.LANBridge != "" && (s.BridgeUp == nil || s.BridgeUp(s.LANBridge))
}

// Networks lists the networks new VMs can use on this host.
func (s *Service) Networks() []string {
	if s.LANAvailable() {
		return []string{NetNAT, NetLAN}
	}
	return []string{NetNAT}
}

// VNets resolves cluster-wide virtual networks (sdn.Manager): network
// "vnet:NAME" is the bridge of vnet NAME on every server.
type VNets interface {
	BridgeOf(ctx context.Context, name string) (string, error)
	NameOf(bridge string) string
	Names() []string
}

// NetVNetPrefix starts a virtual network's name as a VM's network ("vnet:office").
const NetVNetPrefix = "vnet:"

// IsVNet says whether a network is a virtual network ("vnet:NAME").
func IsVNet(network string) bool { return strings.HasPrefix(network, NetVNetPrefix) }

// checkVNet says what is wrong with a "vnet:NAME" network ("" = fine).
func (s *Service) checkVNet(network string) string {
	if s.VNets == nil {
		return "virtual networks aren't available on this server"
	}
	name := strings.TrimPrefix(network, NetVNetPrefix)
	for _, n := range s.VNets.Names() {
		if n == name {
			return ""
		}
	}
	return fmt.Sprintf("there is no virtual network %q (Settings → Virtual networks)", name)
}

// VLANs makes the bridge of a tagged VLAN (vlan.Manager).
type VLANs interface {
	Bridge(ctx context.Context, vid int) (string, error)
	VLANOf(bridge string) int
}

func (s *Service) vlanOf(bridge string) int {
	if s.VLANs == nil {
		return 0
	}
	return s.VLANs.VLANOf(bridge)
}

// bridgeVLAN is the bridge for a network and VLAN (made if needed).
func (s *Service) bridgeVLAN(ctx context.Context, network string, vid int) (string, error) {
	if IsVNet(network) {
		if s.VNets == nil {
			return "", fmt.Errorf("%w: virtual networks aren't available on this server", ErrInvalid)
		}
		return s.VNets.BridgeOf(ctx, strings.TrimPrefix(network, NetVNetPrefix))
	}
	if vid > 0 && network == NetLAN {
		if s.VLANs == nil {
			return "", fmt.Errorf("%w: VLANs aren't available on this host", ErrInvalid)
		}
		return s.VLANs.Bridge(ctx, vid)
	}
	return s.bridgeFor(network), nil
}

func (s *Service) checkVLAN(network string, vid int) string {
	switch {
	case vid == 0:
		return ""
	case vid < 1 || vid > 4094:
		return "vlan must be 1-4094"
	case network != NetLAN:
		return `vlan needs network "lan" (the home / datacenter network)`
	case s.VLANs == nil:
		return "VLANs aren't available on this host"
	}
	return ""
}

func (s *Service) bridgeFor(network string) string {
	if IsVNet(network) {
		if s.VNets == nil {
			return ""
		}
		br, _ := s.VNets.BridgeOf(context.Background(), strings.TrimPrefix(network, NetVNetPrefix))
		return br
	}
	if network == NetLAN {
		return s.LANBridge
	}
	return s.Bridge
}

func (s *Service) networkOf(bridge string) string {
	if s.VNets != nil {
		if n := s.VNets.NameOf(bridge); n != "" {
			return NetVNetPrefix + n
		}
	}
	if s.LANBridge != "" && bridge == s.LANBridge || s.vlanOf(bridge) > 0 {
		return NetLAN
	}
	return NetNAT
}

func (s *Service) lock(name string) func() {
	m, _ := s.locks.LoadOrStore(name, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Service) template(name string) (config.Template, bool) {
	for _, t := range s.AllTemplates() {
		if t.Name == name {
			return t, true
		}
	}
	return config.Template{}, false
}

func (s *Service) validate(r *CreateRequest) (config.Template, error) {
	var errs []string
	if !reName.MatchString(r.Name) {
		errs = append(errs, "name must be 2-49 chars: lowercase letters, digits, hyphens; start with a letter")
	}
	t, ok := s.template(r.Template)
	if !ok {
		errs = append(errs, fmt.Sprintf("unknown template %q", r.Template))
	}
	if r.VCPUs == 0 {
		r.VCPUs = 1
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = 1024
	}
	if r.DiskGB == 0 {
		r.DiskGB = t.MinDiskGB
	}
	if r.VCPUs < 1 || r.VCPUs > s.Limits.MaxVCPUs {
		errs = append(errs, fmt.Sprintf("vcpus must be 1-%d", s.Limits.MaxVCPUs))
	}
	if r.MemoryMiB < 256 || r.MemoryMiB > s.Limits.MaxMemoryMiB {
		errs = append(errs, fmt.Sprintf("memory_mib must be 256-%d", s.Limits.MaxMemoryMiB))
	}
	if ok && (r.DiskGB < t.MinDiskGB || r.DiskGB > s.Limits.MaxDiskGB) {
		errs = append(errs, fmt.Sprintf("disk_gb must be %d-%d", t.MinDiskGB, s.Limits.MaxDiskGB))
	}
	for _, k := range r.SSHKeys {
		if !strings.HasPrefix(k, "ssh-") && !strings.HasPrefix(k, "ecdsa-") && !strings.HasPrefix(k, "sk-") {
			errs = append(errs, "ssh_keys must be OpenSSH public keys")
			break
		}
	}
	if msg := s.checkPool(r); msg != "" {
		errs = append(errs, msg)
	}
	switch r.Network {
	case "", NetNAT:
		r.Network = NetNAT
	case NetLAN:
		if !s.LANAvailable() {
			errs = append(errs, "network \"lan\" is not set up on this host (run: sudo p8 lan-setup)")
		}
	case NetPublic:
	default:
		if IsVNet(r.Network) {
			if msg := s.checkVNet(r.Network); msg != "" {
				errs = append(errs, msg)
			}
			break
		}
		errs = append(errs, `network must be "nat", "lan", "public" or "vnet:NAME"`)
	}
	if msg := s.checkVLAN(r.Network, r.VLAN); msg != "" {
		errs = append(errs, msg)
	}
	if msg := s.checkStorage(r.Storage); msg != "" {
		errs = append(errs, msg)
	}
	if r.RootPassword != "" {
		if len(r.RootPassword) < 8 || len(r.RootPassword) > 128 || strings.ContainsAny(r.RootPassword, "\n\r\x00") {
			errs = append(errs, "root_password must be 8-128 characters on one line")
		}
		if r.UserData != "" {
			errs = append(errs, "root_password can't be combined with user_data (set the password in your cloud-config)")
		}
	} else if r.SSHPasswordAuth != nil && *r.SSHPasswordAuth {
		errs = append(errs, "ssh_password_auth needs a root_password")
	}
	if r.UserData != "" && !strings.HasPrefix(r.UserData, "#cloud-config") {
		errs = append(errs, "user_data must start with #cloud-config")
	}
	if len(errs) > 0 {
		return t, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
	}
	return t, nil
}

// Create clones the template disk, builds the cloud-init seed, defines the
// domain and (by default) starts it. Any failure rolls back what was created.
func (s *Service) Create(ctx context.Context, r CreateRequest) (VM, error) {
	if r.ISO != "" {
		return s.createFromISO(ctx, r)
	}
	t, err := s.validate(&r)
	if err != nil {
		return VM{}, err
	}
	dom := Prefix + r.Name
	defer s.lock(dom)()

	if _, err := s.HV.State(ctx, dom); err == nil {
		return VM{}, fmt.Errorf("%w: vm %q", ErrExists, r.Name)
	} else if !errors.Is(err, virt.ErrNoDomain) {
		return VM{}, err
	}
	// No domain exists, so any disk or seed with this name is a leftover from a
	// create that was interrupted (crash, power loss). Clear it so we can retry.
	if err := s.dropLeftover(ctx, dom); err != nil {
		return VM{}, fmt.Errorf("remove leftover disk: %w", err)
	}
	s.logErr("remove leftover seed", s.Seeds.Remove(dom))
	if !s.Disks.SnapshotExists(ctx, t.Snapshot) {
		return VM{}, fmt.Errorf("template %q snapshot %s is missing on this host", t.Name, t.Snapshot)
	}

	var undo []func()
	rollback := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	// Rollback must still run if the client disconnects mid-create.
	bg := context.WithoutCancel(ctx)

	if err := s.place(dom, r.Storage); err != nil {
		return VM{}, err
	}
	disk, err := s.Disks.CloneDisk(ctx, t.Snapshot, dom, r.DiskGB)
	if err != nil {
		return VM{}, err
	}
	undo = append(undo, func() { s.logErr("rollback disk", s.Disks.DestroyDisk(bg, dom)) })
	var extra []virt.ExtraDisk
	if len(t.Extra) > 0 { // a template made from a VM with data disks
		tdd, ok := s.Disks.(templateDataDisks)
		if !ok {
			rollback()
			return VM{}, fmt.Errorf("%w: this server's storage can't copy the template's data disks", ErrConflict)
		}
		for _, e := range t.Extra {
			p, err := tdd.CloneExtra(ctx, e.Snapshot, dom, e.Index, e.SizeGB)
			if err != nil {
				rollback()
				return VM{}, err
			}
			extra = append(extra, virt.ExtraDisk{Index: e.Index, Path: p, SizeGB: e.SizeGB})
		}
	}

	uuid := newUUID()
	keys := r.SSHKeys
	if len(keys) == 0 && !r.NoDefaultKeys {
		keys = s.SSHKeys
	}
	ci := cloudinit.Spec{InstanceID: uuid, Hostname: r.Name, SSHKeys: keys, UserData: r.UserData}
	if r.RootPassword != "" {
		ci.RootPasswordHash = cloudinit.HashPassword(r.RootPassword)
		ci.RootSSHPassword = r.SSHPasswordAuth == nil || *r.SSHPasswordAuth
	}
	mac := virt.RandomMAC()
	if s.isCustomTemplate(t.Name) && ci.Static == nil {
		ci.DHCPByMAC = mac // VMs from one image may share a machine id: ask DHCP by MAC
	}
	bridge, err := s.bridgeVLAN(ctx, r.Network, r.VLAN)
	if err != nil {
		rollback()
		return VM{}, err
	}
	if r.IPPool != "" {
		l, release, err := s.lease(r.IPPool, r.Name, "")
		if err != nil {
			rollback()
			return VM{}, err
		}
		undo = append(undo, release)
		ci.Static, bridge = static(l, mac), l.Bridge
	}
	seed, err := s.Seeds.Build(ctx, dom, ci)
	if err != nil {
		rollback()
		return VM{}, err
	}
	undo = append(undo, func() { s.logErr("rollback seed", s.Seeds.Remove(dom)) })

	spec := virt.Spec{
		Name: dom, UUID: uuid, VCPUs: r.VCPUs, MemoryMiB: r.MemoryMiB, DiskGB: r.DiskGB,
		DiskPath: disk, SeedISO: seed, Bridge: bridge, MAC: mac,
		Template: t.Name, CreatedAt: time.Now().UTC().Format(time.RFC3339), Owner: r.Owner, Extra: extra,
	}
	xml, err := virt.DomainXML(spec)
	if err != nil {
		rollback()
		return VM{}, err
	}
	if err := s.HV.Define(ctx, xml); err != nil {
		rollback()
		return VM{}, err
	}
	undo = append(undo, func() { s.logErr("rollback define", s.HV.Undefine(bg, dom)) })

	if err := s.HV.SetAutostart(ctx, dom, r.Autostart == nil || *r.Autostart); err != nil {
		rollback()
		return VM{}, err
	}

	if r.Start == nil || *r.Start {
		if err := s.HV.Start(ctx, dom); err != nil {
			rollback()
			return VM{}, err
		}
		s.afterStart(ctx, r.Name)
	}
	return s.get(ctx, dom)
}

func (s *Service) Get(ctx context.Context, name string) (VM, error) {
	v, err := s.get(ctx, Prefix+name)
	if err == nil {
		if l, ok := s.Disks.(linker); ok {
			v.LinkedTo = strings.TrimPrefix(l.LinkedTo(ctx, Prefix+name), Prefix)
			for _, c := range l.LinkedClones(ctx, Prefix+name) {
				v.LinkedClones = append(v.LinkedClones, strings.TrimPrefix(c, Prefix))
			}
		}
	}
	return v, err
}

func (s *Service) get(ctx context.Context, dom string) (VM, error) {
	b, err := s.HV.XML(ctx, dom)
	if errors.Is(err, virt.ErrNoDomain) {
		return VM{}, fmt.Errorf("%w: vm %q", ErrNotFound, strings.TrimPrefix(dom, Prefix))
	}
	if err != nil {
		return VM{}, err
	}
	spec, err := virt.ParseDomainXML(b)
	if err != nil {
		return VM{}, err
	}
	st, err := s.HV.State(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	v := VM{
		Name: strings.TrimPrefix(spec.Name, Prefix), State: st, Template: spec.Template,
		VCPUs: spec.VCPUs, MemoryMiB: spec.MemoryMiB, DiskGB: spec.DiskGB,
		MAC: spec.MAC, Network: s.networkOf(spec.Bridge), VLAN: s.vlanOf(spec.Bridge), UUID: spec.UUID, CreatedAt: spec.CreatedAt,
		Owner: spec.Owner,
		User:  "debian",
	}
	if s.Pools != nil {
		if l, ok := s.Pools.Lookup(v.Name); ok {
			v.Public, v.Network = &l, NetPublic
		}
	}
	if t, ok := s.template(spec.Template); ok {
		v.User = t.User()
	} else if strings.HasPrefix(spec.Template, "iso:") {
		v.User = "" // installed by hand: whatever the user set up
	}
	if spec.InstallISO != "" {
		v.ISO = filepath.Base(spec.InstallISO)
	}
	if spec.DriversISO != "" {
		v.Drivers = filepath.Base(spec.DriversISO)
	}
	v.OSType = spec.OSType
	v.NICs = s.nicsOf(spec)
	v.PCI = spec.PCI
	v.HotCPUs, v.HotMemMiB = spec.HotCPUs, spec.HotMemMiB
	if !spec.Limits.Zero() {
		l := spec.Limits
		v.IOLimits = &l
	}
	for _, u := range spec.USB {
		v.USB = append(v.USB, u.ID())
	}
	v.Storage = s.storageOf(dom)
	v.CPU = "host"
	if spec.CPUModel != "" {
		v.CPU = spec.CPUModel
	}
	for _, e := range spec.Extra {
		v.Disks = append(v.Disks, Disk{Index: e.Index, SizeGB: e.SizeGB, Device: virt.ExtraTarget(spec, e.Index)})
	}
	// Best effort: these must never make GET/list fail.
	v.Autostart, _ = s.HV.Autostart(ctx, dom)
	if st == virt.Running {
		if v.Public != nil {
			v.IP = v.Public.IP // fixed address; no lookup needed
		} else if ips, _ := s.HV.Addresses(ctx, dom); len(ips) > 0 {
			v.IP = ips[0]
		}
	}
	return v, nil
}

func (s *Service) List(ctx context.Context) ([]VM, error) {
	names, err := s.HV.List(ctx)
	if err != nil {
		return nil, err
	}
	// Each lookup is a few libvirt round trips; do them side by side so the
	// dashboard's list stays quick with many VMs.
	var doms []string
	for _, n := range names {
		if strings.HasPrefix(n, Prefix) {
			doms = append(doms, n)
		}
	}
	res := make([]VM, len(doms))
	errs := make([]error, len(doms))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, n := range doms {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			res[i], errs[i] = s.get(ctx, n)
		}()
	}
	wg.Wait()
	vms := []VM{}
	for i := range doms {
		if errors.Is(errs[i], ErrNotFound) {
			continue // deleted between list and get
		}
		if errs[i] != nil {
			return nil, errs[i]
		}
		vms = append(vms, res[i])
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	return vms, nil
}

func (s *Service) Start(ctx context.Context, name string) (VM, error) {
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if st != virt.Running {
		if err := s.HV.Start(ctx, dom); err != nil {
			return VM{}, err
		}
		s.afterStart(ctx, name)
	}
	return s.get(ctx, dom)
}

// afterStart re-applies firewall rules that depend on the VM's host port
// (public-address pinning); the port name changes on every start.
func (s *Service) afterStart(ctx context.Context, name string) {
	s.pressBootKey(context.WithoutCancel(ctx), Prefix+name)
	if s.Pools == nil {
		return
	}
	if _, ok := s.Pools.Lookup(name); ok {
		s.logErr("firewall after start of "+name, s.ApplyFirewalls(context.WithoutCancel(ctx)))
	}
}

// Reboot asks a running guest to restart via ACPI.
func (s *Service) Reboot(ctx context.Context, name string) (VM, error) {
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if st != virt.Running {
		return VM{}, fmt.Errorf("%w: vm %q is %s, not running", ErrConflict, name, st)
	}
	if err := s.HV.Reboot(ctx, dom); err != nil {
		return VM{}, err
	}
	return s.get(ctx, dom)
}

// Stop asks the guest to shut down via ACPI, or pulls the power if force is set.
func (s *Service) Stop(ctx context.Context, name string, force bool) (VM, error) {
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if st != virt.Stopped {
		if force {
			err = s.HV.Destroy(ctx, dom)
		} else {
			err = s.HV.Shutdown(ctx, dom)
		}
		if err != nil {
			return VM{}, err
		}
	}
	return s.get(ctx, dom)
}

// Delete powers off the VM, removes the domain, its disk and its seed ISO.
func (s *Service) Delete(ctx context.Context, name string) error {
	dom := Prefix + name
	defer s.lock(dom)()
	return s.remove(ctx, name, false)
}

// remove deletes the VM (caller holds the lock); keepDisk leaves its disk
// for the caller (failover: it becomes the copy).
func (s *Service) remove(ctx context.Context, name string, keepDisk bool) error {
	dom := Prefix + name
	st, err := s.state(ctx, dom)
	if err != nil {
		return err
	}
	if !keepDisk {
		if l := s.linkedClones(ctx, dom); len(l) > 0 {
			return fmt.Errorf("%w: %s's disks are used by its linked clones (%s); delete them or make them independent first",
				ErrConflict, name, strings.Join(l, ", "))
		}
	}
	if st != virt.Stopped {
		if err := s.HV.Destroy(ctx, dom); err != nil {
			return err
		}
	}
	if err := s.HV.Undefine(ctx, dom); err != nil {
		return err
	}
	s.logErr("remove seed", s.Seeds.Remove(dom))
	// Backups are kept (a deleted VM can be restored); its daily schedule is not.
	s.logErr("remove backup schedule", s.SetSchedule(name, Schedule{}))
	s.logErr("remove firewall", s.dropFirewall(context.WithoutCancel(ctx), name))
	s.releaseIP(context.WithoutCancel(ctx), name)
	s.dropStates(dom) // memory of its snapshots: useless without the VM
	if keepDisk {
		return nil
	}
	if err := s.Disks.DestroyDisk(context.WithoutCancel(ctx), dom); err != nil {
		return fmt.Errorf("domain removed but disk cleanup failed: %w", err)
	}
	return nil
}

func (s *Service) state(ctx context.Context, dom string) (virt.State, error) {
	st, err := s.HV.State(ctx, dom)
	if errors.Is(err, virt.ErrNoDomain) {
		return st, fmt.Errorf("%w: vm %q", ErrNotFound, strings.TrimPrefix(dom, Prefix))
	}
	return st, err
}

func (s *Service) logErr(what string, err error) {
	if err != nil && s.Log != nil {
		s.Log.Error(what, "err", err)
	}
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
