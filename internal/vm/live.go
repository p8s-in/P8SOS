package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"p8sos/internal/cloudinit"
	"p8sos/internal/firewall"
	"p8sos/internal/virt"
)

// Live moves: a running VM goes to another server of the cluster and keeps
// running (memory and disks are copied while it works; it pauses for well
// under a second at the end). The cluster master runs the steps:
//
//	source LiveHeader → target LivePrepare → source LiveSend
//	→ target LiveFinish + source LiveCleanup   (or target LiveAbort)

// liveHV is the hypervisor's live-move part (virt.Virsh).
type liveHV interface {
	MigratableXML(ctx context.Context, name string, inactive bool) ([]byte, error)
	Migrate(ctx context.Context, name, ip string, xml, persistentXML []byte, progress func(int)) error
}

// liveDisks is the storage's live-move part (storage.ZFS).
type liveDisks interface {
	DiskBytes(ctx context.Context, vm string, i int) (int64, error)
	CreateSized(ctx context.Context, vm string, i int, bytes int64) (string, error)
}

// LiveDisk is one disk of a VM being moved live.
type LiveDisk struct {
	Index int    `json:"index"` // 0 = root
	Bytes int64  `json:"bytes"`
	Path  string `json:"path"` // on the server it comes from
}

// LiveHeader describes a running VM for a live move.
type LiveHeader struct {
	MoveHeader
	XML           string     `json:"xml"`            // running definition (migratable)
	PersistentXML string     `json:"persistent_xml"` // saved definition
	Disks         []LiveDisk `json:"disks"`
	SeedPath      string     `json:"seed_path,omitempty"`
	Bridge        string     `json:"bridge"`
	// CPUSig is set when the VM runs on the host's own CPU: the other server
	// must have exactly the same CPU.
	CPUSig string `json:"cpu_sig,omitempty"`
}

// LivePlan is what the target prepared: the definitions to use there.
type LivePlan struct {
	XML           string `json:"xml"`
	PersistentXML string `json:"persistent_xml"`
	Note          string `json:"note,omitempty"`
}

var (
	reCPUModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reCPUFeat  = regexp.MustCompile(`^[+-][a-z0-9][a-z0-9._-]{0,63}$`)
)

// CPUInfo is where the host's CPU description is read (tests replace it).
var CPUInfo = "/proc/cpuinfo"

// CPUSignature identifies this server's CPU (model and features).
func CPUSignature() string {
	b, err := os.ReadFile(CPUInfo)
	if err != nil {
		return ""
	}
	var model, flags string
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "model name":
			if model == "" {
				model = strings.TrimSpace(v)
			}
		case "flags":
			if flags == "" {
				flags = strings.TrimSpace(v)
			}
		}
	}
	h := sha256.Sum256([]byte(model + "|" + flags))
	return model + " #" + hex.EncodeToString(h[:4])
}

func (s *Service) liveParts() (liveHV, liveDisks, error) {
	hv, ok := s.HV.(liveHV)
	if !ok {
		return nil, nil, fmt.Errorf("%w: this server can't move running VMs", ErrConflict)
	}
	d, ok := s.Disks.(liveDisks)
	if !ok {
		return nil, nil, fmt.Errorf("%w: this server's storage can't move running VMs", ErrConflict)
	}
	return hv, d, nil
}

// LiveCheck says whether a VM can be moved while it runs ("" = yes).
func (s *Service) LiveCheck(ctx context.Context, name string) (string, error) {
	v, err := s.Get(ctx, name)
	if err != nil {
		return "", err
	}
	if _, _, err := s.liveParts(); err != nil {
		return "this server can't move running VMs", nil
	}
	sp, err := s.spec(ctx, Prefix+name)
	if err != nil {
		return "", err
	}
	switch {
	case v.State != virt.Running:
		return "it isn't running (use a normal move)", nil
	case v.Public != nil:
		return "it has a public IP from this server's pool; release it first", nil
	case v.ISO != "" || v.Drivers != "":
		return "eject its CDs first (Manage → Installer CD)", nil
	case hostDevsNote(sp) != "":
		return hostDevsNote(sp), nil
	}
	return "", nil
}

// LiveHeader describes a running VM for a live move (on the server it is on).
func (s *Service) LiveHeader(ctx context.Context, name string) (LiveHeader, error) {
	hv, ld, err := s.liveParts()
	if err != nil {
		return LiveHeader{}, err
	}
	if why, err := s.LiveCheck(ctx, name); err != nil {
		return LiveHeader{}, err
	} else if why != "" {
		return LiveHeader{}, fmt.Errorf("%w: %s can't move live: %s", ErrConflict, name, why)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	mh, err := s.moveHeader(ctx, name)
	if err != nil {
		return LiveHeader{}, err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return LiveHeader{}, err
	}
	live, err := hv.MigratableXML(ctx, dom, false)
	if err != nil {
		return LiveHeader{}, err
	}
	saved, err := hv.MigratableXML(ctx, dom, true)
	if err != nil {
		return LiveHeader{}, err
	}
	h := LiveHeader{MoveHeader: mh, XML: string(live), PersistentXML: string(saved), SeedPath: sp.SeedISO, Bridge: sp.Bridge}
	disks := append([]virt.ExtraDisk{{Index: 0, Path: sp.DiskPath}}, sp.Extra...)
	for _, d := range disks {
		b, err := ld.DiskBytes(ctx, dom, d.Index)
		if err != nil {
			return LiveHeader{}, fmt.Errorf("size of disk %d: %w", d.Index, err)
		}
		h.Disks = append(h.Disks, LiveDisk{Index: d.Index, Bytes: b, Path: d.Path})
	}
	if sp.CPUModel == "" {
		h.CPUSig = CPUSignature()
	}
	return h, nil
}

// LivePrepare gets this server ready to receive a running VM: empty disks of
// the same sizes, its cloud-init CD, the network; it returns the definitions
// to send it with.
func (s *Service) LivePrepare(ctx context.Context, h LiveHeader) (LivePlan, error) {
	_, ld, err := s.liveParts()
	if err != nil {
		return LivePlan{}, err
	}
	if !reName.MatchString(h.Name) || h.XML == "" || h.PersistentXML == "" || len(h.Disks) == 0 || h.Disks[0].Index != 0 {
		return LivePlan{}, fmt.Errorf("%w: bad live-move header", ErrInvalid)
	}
	if h.CPUSig != "" {
		if mine := CPUSignature(); mine != h.CPUSig {
			return LivePlan{}, fmt.Errorf("%w: the CPUs differ (%s here, %s there). Set the VM's CPU type to "+
				"\"compatible\" (Manage → Options), shut it down and start it once; then it can move live", ErrConflict, mine, h.CPUSig)
		}
	}
	dom := Prefix + h.Name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return LivePlan{}, fmt.Errorf("%w: a VM called %q is already on this server", ErrExists, h.Name)
	}
	if h.Network == NetLAN && !s.LANAvailable() {
		return LivePlan{}, fmt.Errorf("%w: this server has no home network, which the VM uses", ErrConflict)
	}
	bridge, err := s.bridgeVLAN(ctx, h.Network, h.VLAN)
	if err != nil {
		return LivePlan{}, fmt.Errorf("%w: network: %v", ErrConflict, err)
	}
	bg := context.WithoutCancel(ctx)
	var undo []func()
	fail := func(err error) (LivePlan, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return LivePlan{}, err
	}
	repl := map[string]string{h.Bridge: bridge}
	if h.Shared != "" { // the same disks: open them here too
		disk, extra, err := s.attachShared(ctx, h.MoveHeader)
		if err != nil {
			return fail(err)
		}
		undo = append(undo, func() { s.logErr("close shared disks", s.release(bg, dom)) })
		for _, d := range h.Disks {
			if d.Index == 0 {
				repl[d.Path] = disk
			}
			for _, e := range extra {
				if e.Index == d.Index {
					repl[d.Path] = e.Path
				}
			}
		}
		h.Disks = nil
	} else {
		_ = s.dropLeftover(ctx, dom) // leftover of an interrupted move
	}
	for _, d := range h.Disks {
		if d.Bytes <= 0 || d.Index < 0 || d.Index > virt.MaxExtraDisks {
			return fail(fmt.Errorf("%w: bad disk %d", ErrInvalid, d.Index))
		}
		dev, err := ld.CreateSized(ctx, dom, d.Index, d.Bytes)
		if err != nil {
			return fail(err)
		}
		if d.Index == 0 {
			undo = append(undo, func() { s.logErr("rollback live disks", s.Disks.DestroyDisk(bg, dom)) })
		}
		repl[d.Path] = dev
	}
	if h.Seed && h.SeedPath != "" {
		seed, err := s.Seeds.Build(ctx, dom, cloudinit.Spec{InstanceID: h.UUID, Hostname: h.Name, SSHKeys: s.defaultKeys(h.Owner)})
		if err != nil {
			return fail(err)
		}
		undo = append(undo, func() { s.logErr("rollback seed", s.Seeds.Remove(dom)) })
		repl[h.SeedPath] = seed
	}
	if len(h.NVRAM) > 0 { // UEFI: QEMU needs the variables file to start; the live state follows
		np := s.nvramPath(dom)
		if err := os.MkdirAll(filepath.Dir(np), 0o755); err == nil {
			s.logErr("write uefi variables", os.WriteFile(np, h.NVRAM, 0o600))
		}
		undo = append(undo, func() { _ = os.Remove(np) })
	}
	if len(h.TPM) > 0 && h.UUID != "" { // Windows 11: the software TPM's state (libvirt sends the live state too)
		if err := s.writeTPM(h.UUID, h.TPM); err != nil {
			return fail(fmt.Errorf("TPM state: %w", err))
		}
		undo = append(undo, func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(s.tpmFile(h.UUID)))) })
	}
	return LivePlan{
		XML:           string(virt.RewriteXML([]byte(h.XML), repl)),
		PersistentXML: string(virt.RewriteXML([]byte(h.PersistentXML), repl)),
	}, nil
}

// LiveSend moves the running VM to the server at ip (its tunnel address).
func (s *Service) LiveSend(ctx context.Context, name, ip string, plan LivePlan, progress func(int)) error {
	hv, _, err := s.liveParts()
	if err != nil {
		return err
	}
	if why, err := s.LiveCheck(ctx, name); err != nil {
		return err
	} else if why != "" {
		return fmt.Errorf("%w: %s can't move live: %s", ErrConflict, name, why)
	}
	// a live mirror (P8S Mirror) can't run during the move; high availability
	// starts it again from the new server
	if _, ok := s.HV.(monitor); ok {
		if err := s.MirrorStop(ctx, name); err != nil {
			return fmt.Errorf("stop the live mirror before the move: %w", err)
		}
	}
	dom := Prefix + name
	defer s.lock(dom)()
	return hv.Migrate(ctx, dom, ip, []byte(plan.XML), []byte(plan.PersistentXML), progress)
}

// LiveFinish runs on the new server once the VM is there: autostart, firewall,
// backup schedule as they were.
func (s *Service) LiveFinish(ctx context.Context, name string, autostart bool, fw *firewall.Settings, sch *Schedule) (VM, string, error) {
	dom := Prefix + name
	note := ""
	if st, err := s.state(ctx, dom); err != nil {
		return VM{}, "", err
	} else if st != virt.Running {
		note = " (it is " + string(st) + " here)"
	}
	if err := s.HV.SetAutostart(ctx, dom, autostart); err != nil {
		note += "; autostart: " + err.Error()
	}
	if fw != nil {
		if _, err := s.SetFirewall(ctx, name, *fw); err != nil {
			note += "; firewall rules not applied: " + err.Error()
		}
	} else {
		s.logErr("firewall after live move", s.ApplyFirewalls(ctx))
	}
	if sch != nil {
		s.logErr("daily backup schedule of moved vm", s.SetSchedule(name, *sch))
	}
	v, err := s.get(ctx, dom)
	return v, note, err
}

// LiveAbort cleans up a live move that failed (on the target).
func (s *Service) LiveAbort(ctx context.Context, name string) error {
	if !reName.MatchString(name) {
		return fmt.Errorf("%w: bad name", ErrInvalid)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if st, err := s.HV.State(ctx, dom); err == nil {
		if st == virt.Running {
			return fmt.Errorf("%w: %s is running here; not removing it", ErrConflict, name)
		}
		s.logErr("undefine after failed live move", s.HV.Undefine(ctx, dom))
	} else if !errors.Is(err, virt.ErrNoDomain) {
		return err
	}
	s.logErr("remove seed", s.Seeds.Remove(dom))
	_ = os.Remove(s.nvramPath(dom))
	if s.shared(dom) {
		return s.release(ctx, dom) // the VM still runs on its old server
	}
	return s.Disks.DestroyDisk(ctx, dom)
}

// LiveCleanup removes what a VM left on the old server after a live move
// (libvirt already took its definition away).
func (s *Service) LiveCleanup(ctx context.Context, name string) error {
	if !reName.MatchString(name) {
		return fmt.Errorf("%w: bad name", ErrInvalid)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return fmt.Errorf("%w: %s is still defined here; not removing its disks", ErrConflict, name)
	}
	s.logErr("remove seed", s.Seeds.Remove(dom))
	s.logErr("remove backup schedule", s.SetSchedule(name, Schedule{}))
	s.logErr("remove firewall", s.dropFirewall(context.WithoutCancel(ctx), name))
	_ = os.Remove(s.nvramPath(dom))
	if s.shared(dom) {
		return s.release(ctx, dom) // the new server uses the same disks
	}
	return s.Disks.DestroyDisk(ctx, dom)
}

// SetCPU sets the VM's CPU type: model "" = this server's CPU as it is
// (fastest), else a named model (e.g. the cluster's common CPU). It takes
// effect when the VM is next started from shut down.
func (s *Service) SetCPU(ctx context.Context, name string, cpu virt.CPU) (VM, error) {
	if cpu.Model != "" && !reCPUModel.MatchString(cpu.Model) {
		return VM{}, fmt.Errorf("%w: bad CPU model %q", ErrInvalid, cpu.Model)
	}
	for _, f := range cpu.Features {
		if !reCPUFeat.MatchString(f) {
			return VM{}, fmt.Errorf("%w: bad CPU feature %q", ErrInvalid, f)
		}
	}
	dom := Prefix + name
	defer s.lock(dom)()
	if _, err := s.state(ctx, dom); err != nil {
		return VM{}, err
	}
	if err := s.redefine(ctx, dom, func(sp *virt.Spec) { sp.CPUModel, sp.CPUFeatures = cpu.Model, cpu.Features }); err != nil {
		return VM{}, err
	}
	return s.get(ctx, dom)
}

type cpuHV interface {
	HostCPU(ctx context.Context) ([]byte, error)
	CPUBaseline(ctx context.Context, caps [][]byte) (virt.CPU, error)
}

// HostCPU is this server's CPU description (libvirt domcapabilities).
func (s *Service) HostCPU(ctx context.Context) ([]byte, error) {
	h, ok := s.HV.(cpuHV)
	if !ok {
		return nil, fmt.Errorf("%w: CPU types aren't available here", ErrConflict)
	}
	return h.HostCPU(ctx)
}

// CPUBaseline is the CPU model all the given servers can run.
func (s *Service) CPUBaseline(ctx context.Context, caps [][]byte) (virt.CPU, error) {
	h, ok := s.HV.(cpuHV)
	if !ok {
		return virt.CPU{}, fmt.Errorf("%w: CPU types aren't available here", ErrConflict)
	}
	return h.CPUBaseline(ctx, caps)
}
