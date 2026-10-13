package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"p8sos/internal/cloudinit"
	"p8sos/internal/firewall"
	"p8sos/internal/virt"
)

// Moving a VM to another server of the cluster (it must be shut down):
// ExportVM writes a header line (JSON) and then the disk; ImportMovedVM on
// the other server makes the same VM from that stream. The cluster code
// connects the two and deletes the original once the copy is defined.

// diskMover is the storage that can stream disks (ZFS).
type diskMover interface {
	SendDisk(ctx context.Context, vm string, w io.Writer) error
	RecvDisk(ctx context.Context, vm string, r io.Reader) (string, error)
}

// MoveHeader describes the VM in the stream.
type MoveHeader struct {
	Name      string             `json:"name"`
	UUID      string             `json:"uuid"`
	VCPUs     int                `json:"vcpus"`
	MemoryMiB int                `json:"memory_mib"`
	DiskGB    int                `json:"disk_gb"`
	OSType    string             `json:"os_type,omitempty"`
	Firmware  string             `json:"firmware,omitempty"`
	DiskBus   string             `json:"disk_bus,omitempty"`
	NICModel  string             `json:"nic_model,omitempty"`
	MAC       string             `json:"mac"`
	Template  string             `json:"template"`
	CreatedAt string             `json:"created_at"`
	Owner     string             `json:"owner,omitempty"`
	Network   string             `json:"network"` // nat | lan
	Seed      bool               `json:"seed"`    // had a cloud-init disk
	Autostart bool               `json:"autostart"`
	Firewall  *firewall.Settings `json:"firewall,omitempty"`
	Schedule  *Schedule          `json:"schedule,omitempty"`
}

// MoveCheck says whether a VM can be moved now ("" = yes).
func (s *Service) MoveCheck(ctx context.Context, name string) (string, error) {
	v, err := s.Get(ctx, name)
	if err != nil {
		return "", err
	}
	if _, ok := s.Disks.(diskMover); !ok {
		return "this server's storage can't move disks", nil
	}
	switch {
	case v.State != virt.Stopped:
		return "shut it down first (it is " + string(v.State) + ")", nil
	case v.Public != nil:
		return "it has a public IP from this server's pool; release it first", nil
	case v.ISO != "" || v.Drivers != "":
		return "eject its CDs first (Manage → Installer CD)", nil
	}
	return "", nil
}

// ExportVM streams a stopped VM (header line + disk) to w.
func (s *Service) ExportVM(ctx context.Context, name string, w io.Writer) error {
	dom := Prefix + name
	defer s.lock(dom)()
	why, err := s.MoveCheck(ctx, name)
	if err != nil {
		return err
	}
	if why != "" {
		return fmt.Errorf("%w: %s can't be moved: %s", ErrConflict, name, why)
	}
	x, err := s.HV.XML(ctx, dom)
	if err != nil {
		return err
	}
	sp, err := virt.ParseDomainXML(x)
	if err != nil {
		return err
	}
	auto, _ := s.HV.Autostart(ctx, dom)
	h := MoveHeader{Name: name, UUID: sp.UUID, VCPUs: sp.VCPUs, MemoryMiB: sp.MemoryMiB, DiskGB: sp.DiskGB, OSType: sp.OSType,
		Firmware: sp.Firmware, DiskBus: sp.DiskBus, NICModel: sp.NICModel, MAC: sp.MAC, Template: sp.Template,
		CreatedAt: sp.CreatedAt, Owner: sp.Owner, Network: NetNAT, Seed: sp.SeedISO != "", Autostart: auto}
	if s.LANBridge != "" && sp.Bridge == s.LANBridge {
		h.Network = NetLAN
	}
	if fw, err := s.GetFirewall(ctx, name); err == nil && (fw.Enabled || len(fw.Rules) > 0) {
		h.Firewall = &fw
	}
	if sch, err := s.GetSchedule(name); err == nil && sch.Daily {
		h.Schedule = &sch
	}
	b, _ := json.Marshal(h)
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return s.Disks.(diskMover).SendDisk(ctx, dom, w)
}

// ImportMovedVM makes a VM from an ExportVM stream (left stopped).
func (s *Service) ImportMovedVM(ctx context.Context, r io.Reader) (VM, string, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	line, err := br.ReadSlice('\n')
	if err != nil {
		return VM{}, "", fmt.Errorf("%w: no VM header in the stream", ErrInvalid)
	}
	var h MoveHeader
	if err := json.Unmarshal(line, &h); err != nil || !reName.MatchString(h.Name) || h.VCPUs < 1 || h.MemoryMiB < 256 {
		return VM{}, "", fmt.Errorf("%w: bad VM header", ErrInvalid)
	}
	mv, ok := s.Disks.(diskMover)
	if !ok {
		return VM{}, "", fmt.Errorf("%w: this server's storage can't receive disks", ErrConflict)
	}
	dom := Prefix + h.Name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return VM{}, "", fmt.Errorf("%w: a VM called %q is already on this server", ErrExists, h.Name)
	}
	bg := context.WithoutCancel(ctx)
	var undo []func()
	fail := func(err error) (VM, string, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return VM{}, "", err
	}
	_ = s.Disks.DestroyDisk(ctx, dom) // leftover of an interrupted move
	disk, err := mv.RecvDisk(ctx, dom, br)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, func() { s.logErr("rollback moved disk", s.Disks.DestroyDisk(bg, dom)) })
	note := ""
	network := h.Network
	if network == NetLAN && !s.LANAvailable() {
		network, note = NetNAT, "; this server has no home network, so it is on the private network (new IP)"
	}
	seed := ""
	if h.Seed {
		// the same instance-id: cloud-init sees the same machine and changes nothing
		seed, err = s.Seeds.Build(ctx, dom, cloudinit.Spec{InstanceID: h.UUID, Hostname: h.Name, SSHKeys: s.defaultKeys(h.Owner)})
		if err != nil {
			return fail(err)
		}
		undo = append(undo, func() { s.logErr("rollback seed", s.Seeds.Remove(dom)) })
	}
	spec := virt.Spec{Name: dom, UUID: h.UUID, VCPUs: h.VCPUs, MemoryMiB: h.MemoryMiB, DiskGB: h.DiskGB, DiskPath: disk, SeedISO: seed,
		Bridge: s.bridgeFor(network), MAC: h.MAC, OSType: h.OSType, Template: h.Template, CreatedAt: h.CreatedAt, Owner: h.Owner,
		DiskBus: h.DiskBus, NICModel: h.NICModel, Firmware: h.Firmware}
	if spec.UUID == "" {
		spec.UUID = newUUID()
	}
	if spec.CreatedAt == "" {
		spec.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	x, err := virt.DomainXML(spec)
	if err != nil {
		return fail(err)
	}
	if err := s.HV.Define(ctx, x); err != nil {
		return fail(err)
	}
	undo = append(undo, func() { s.logErr("rollback define", s.HV.Undefine(bg, dom)) })
	if err := s.HV.SetAutostart(ctx, dom, h.Autostart); err != nil {
		return fail(err)
	}
	if h.Firewall != nil {
		if _, err := s.SetFirewall(ctx, h.Name, *h.Firewall); err != nil {
			note += "; firewall rules not applied: " + err.Error()
		}
	}
	if h.Schedule != nil {
		s.logErr("daily backup schedule of moved vm", s.SetSchedule(h.Name, *h.Schedule))
	}
	v, err := s.get(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	return v, note, nil
}

