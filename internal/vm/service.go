// Package vm orchestrates storage, cloud-init and the hypervisor into VM
// lifecycle operations. It is the only layer the API talks to.
package vm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/cloudinit"
	"p8sos/internal/config"
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
	Bridge    string // NAT bridge (network "nat", the default)
	LANBridge string // bridge onto the home network (network "lan"); empty = not set up
	// BridgeUp reports whether a host bridge exists; nil means "assume yes".
	BridgeUp func(name string) bool
	Limits   Limits
	SSHKeys  []string // default keys for creates that pass none
	// AgentRetry is the pause between guest-agent retries; 0 = 3s (tests shorten it).
	AgentRetry time.Duration
	// AgentWait is how long to keep retrying; 0 = 45s.
	AgentWait time.Duration
	Log       *slog.Logger

	locks sync.Map // name -> *sync.Mutex; serializes ops per VM
}

type Limits struct{ MaxVCPUs, MaxMemoryMiB, MaxDiskGB int }

type VM struct {
	Name      string     `json:"name"`
	State     virt.State `json:"state"`
	Template  string     `json:"template"`
	User      string     `json:"user"` // login user with the SSH keys (debian, ubuntu, rocky, ...)
	VCPUs     int        `json:"vcpus"`
	MemoryMiB int        `json:"memory_mib"`
	DiskGB    int        `json:"disk_gb"`
	MAC       string     `json:"mac"`
	Network   string     `json:"network"`   // "nat" (private, behind the host) or "lan" (home network)
	IP        string     `json:"ip"`        // first IPv4; empty while booting or stopped
	Autostart bool       `json:"autostart"` // starts when the host boots
	UUID      string     `json:"uuid"`
	CreatedAt string     `json:"created_at"`
}

type CreateRequest struct {
	Name      string   `json:"name"`
	Template  string   `json:"template"`
	VCPUs     int      `json:"vcpus"`
	MemoryMiB int      `json:"memory_mib"`
	DiskGB    int      `json:"disk_gb"`
	SSHKeys   []string `json:"ssh_keys"`
	UserData  string   `json:"user_data,omitempty"`
	Network   string   `json:"network,omitempty"` // "nat" (default) or "lan"
	// RootPassword sets root's password (8-128 chars). It is stored only as a
	// SHA-512 crypt hash inside the VM's seed and never returned by the API.
	RootPassword string `json:"root_password,omitempty"`
	// SSHPasswordAuth allows SSH logins as root with RootPassword (default true when one is set).
	SSHPasswordAuth *bool `json:"ssh_password_auth,omitempty"`
	Start           *bool `json:"start,omitempty"`     // default true
	Autostart       *bool `json:"autostart,omitempty"` // default true: start when the host boots
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

func (s *Service) bridgeFor(network string) string {
	if network == NetLAN {
		return s.LANBridge
	}
	return s.Bridge
}

func (s *Service) networkOf(bridge string) string {
	if s.LANBridge != "" && bridge == s.LANBridge {
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
	for _, t := range s.Templates {
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
	switch r.Network {
	case "", NetNAT:
		r.Network = NetNAT
	case NetLAN:
		if !s.LANAvailable() {
			errs = append(errs, "network \"lan\" is not set up on this host (run: sudo p8 lan-setup)")
		}
	default:
		errs = append(errs, `network must be "nat" or "lan"`)
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
	if err := s.Disks.DestroyDisk(ctx, dom); err != nil {
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

	disk, err := s.Disks.CloneDisk(ctx, t.Snapshot, dom, r.DiskGB)
	if err != nil {
		return VM{}, err
	}
	undo = append(undo, func() { s.logErr("rollback disk", s.Disks.DestroyDisk(bg, dom)) })

	uuid := newUUID()
	keys := r.SSHKeys
	if len(keys) == 0 {
		keys = s.SSHKeys
	}
	ci := cloudinit.Spec{InstanceID: uuid, Hostname: r.Name, SSHKeys: keys, UserData: r.UserData}
	if r.RootPassword != "" {
		ci.RootPasswordHash = cloudinit.HashPassword(r.RootPassword)
		ci.RootSSHPassword = r.SSHPasswordAuth == nil || *r.SSHPasswordAuth
	}
	seed, err := s.Seeds.Build(ctx, dom, ci)
	if err != nil {
		rollback()
		return VM{}, err
	}
	undo = append(undo, func() { s.logErr("rollback seed", s.Seeds.Remove(dom)) })

	spec := virt.Spec{
		Name: dom, UUID: uuid, VCPUs: r.VCPUs, MemoryMiB: r.MemoryMiB, DiskGB: r.DiskGB,
		DiskPath: disk, SeedISO: seed, Bridge: s.bridgeFor(r.Network), MAC: virt.RandomMAC(),
		Template: t.Name, CreatedAt: time.Now().UTC().Format(time.RFC3339),
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
	}
	return s.get(ctx, dom)
}

func (s *Service) Get(ctx context.Context, name string) (VM, error) {
	return s.get(ctx, Prefix+name)
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
		MAC: spec.MAC, Network: s.networkOf(spec.Bridge), UUID: spec.UUID, CreatedAt: spec.CreatedAt,
		User: "debian",
	}
	if t, ok := s.template(spec.Template); ok {
		v.User = t.User()
	}
	// Best effort: these must never make GET/list fail.
	v.Autostart, _ = s.HV.Autostart(ctx, dom)
	if st == virt.Running {
		if ips, _ := s.HV.Addresses(ctx, dom); len(ips) > 0 {
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
	vms := []VM{}
	for _, n := range names {
		if !strings.HasPrefix(n, Prefix) {
			continue
		}
		v, err := s.get(ctx, n)
		if errors.Is(err, ErrNotFound) {
			continue // deleted between list and get
		}
		if err != nil {
			return nil, err
		}
		vms = append(vms, v)
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
	}
	return s.get(ctx, dom)
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
	st, err := s.state(ctx, dom)
	if err != nil {
		return err
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
