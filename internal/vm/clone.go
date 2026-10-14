package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"p8sos/internal/cloudinit"
	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

// Cloning: a new VM with a full copy of every disk of another one (taken at
// one instant, also while it runs), its size, network and firewall. It gets
// its own name, MAC address and machine identity: VMs made from a template
// get a fresh cloud-init disk (new host keys, the new name, DHCP by MAC so the
// two never ask for the same address). VMs installed from an ISO keep
// everything as it is (Windows: run sysprep before cloning).

// cloneDisks is the storage part (storage.ZFS).
type cloneDisks interface {
	CloneDisks(ctx context.Context, vm, snap, newVM string, idx []int) error
}

// CloneRequest asks for a copy of a VM.
type CloneRequest struct {
	Name  string `json:"name"`
	Start bool   `json:"start"`
}

// CloneCheck says whether a VM can be cloned ("" = yes).
func (s *Service) CloneCheck(ctx context.Context, name string) (string, error) {
	if _, err := s.Get(ctx, name); err != nil {
		return "", err
	}
	if _, ok := s.Disks.(cloneDisks); !ok {
		return "this server's storage can't copy disks", nil
	}
	if _, err := s.snapDisks(); err != nil {
		return "this server's storage has no snapshots", nil
	}
	if s.Jobs == nil {
		return "background jobs aren't set up", nil
	}
	return "", nil
}

// StartClone copies a VM in the background.
func (s *Service) StartClone(ctx context.Context, name string, r CloneRequest) (Job, error) {
	if !reName.MatchString(r.Name) {
		return Job{}, fmt.Errorf("%w: name must be 2-49 chars: lowercase letters, digits, hyphens; start with a letter", ErrInvalid)
	}
	if why, err := s.CloneCheck(ctx, name); err != nil {
		return Job{}, err
	} else if why != "" {
		return Job{}, fmt.Errorf("%w: %s", ErrConflict, why)
	}
	if _, err := s.HV.State(ctx, Prefix+r.Name); err == nil {
		return Job{}, fmt.Errorf("%w: vm %q", ErrExists, r.Name)
	}
	j, err := s.Jobs.Start("clone", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return s.clone(ctx, name, r, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return Job{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return j, err
}

// snapshotNow takes a snapshot of all the VM's disks (filesystems frozen
// for that instant when it runs). The caller holds the VM lock.
func (s *Service) snapshotNow(ctx context.Context, dom, snap string) error {
	sd, err := s.snapDisks()
	if err != nil {
		return err
	}
	st, err := s.state(ctx, dom)
	if err != nil {
		return err
	}
	frozen := st == virt.Running && s.HV.FSFreeze(ctx, dom) == nil
	err = sd.Snapshot(ctx, dom, snap)
	if frozen {
		s.logErr("thaw "+dom, s.HV.FSThaw(context.WithoutCancel(ctx), dom))
	}
	return err
}

func (s *Service) clone(ctx context.Context, name string, r CloneRequest, p jobs.Progress) (string, error) {
	sd, _ := s.snapDisks()
	cd := s.Disks.(cloneDisks)
	src, dst := Prefix+name, Prefix+r.Name
	snap := fmt.Sprintf("p8s-clone-%d", time.Now().UnixNano())
	bg := context.WithoutCancel(ctx)

	p("taking a snapshot")
	unlock := s.lock(src)
	sp, err := s.spec(ctx, src)
	if err == nil {
		err = s.snapshotNow(ctx, src, snap)
	}
	storage := s.storageOf(src)
	unlock()
	if err != nil {
		return "", err
	}
	defer func() { s.logErr("drop clone snapshot", sd.DestroySnapshot(bg, src, snap)) }()

	defer s.lock(dst)()
	if _, err := s.HV.State(ctx, dst); err == nil {
		return "", fmt.Errorf("%w: vm %q", ErrExists, r.Name)
	}
	if err := s.place(dst, storage); err != nil {
		return "", err
	}
	idx := []int{0}
	for _, d := range sp.Extra {
		idx = append(idx, d.Index)
	}
	p(fmt.Sprintf("copying %d disk(s)", len(idx)))
	if err := cd.CloneDisks(ctx, src, snap, dst, idx); err != nil {
		return "", err
	}
	var undo []func()
	fail := func(err error) (string, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return "", err
	}
	undo = append(undo, func() { s.logErr("rollback cloned disks", s.Disks.DestroyDisk(bg, dst)) })

	p("making the new VM")
	ns := sp
	ns.Name, ns.UUID, ns.MAC = dst, newUUID(), virt.RandomMAC()
	ns.NICs = nil
	for _, n := range sp.NICs { // the extra cards too: two VMs with one MAC break the network
		ns.NICs = append(ns.NICs, virt.ExtraNIC{MAC: virt.RandomMAC(), Bridge: n.Bridge})
	}
	ns.PCI, ns.USB = nil, nil // a device can only be in one VM
	ns.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	ns.Video, ns.DefVersion = "", 0
	dw, ok := s.Disks.(deviceWaiter)
	if !ok {
		return fail(errors.New("this server's storage has no disk devices"))
	}
	if ns.DiskPath, err = dw.WaitDevice(ctx, dst); err != nil {
		return fail(err)
	}
	ns.Extra = nil
	for _, d := range sp.Extra {
		ew, ok := s.Disks.(extraWaiter)
		if !ok {
			return fail(errors.New("this server's storage can't open data disks"))
		}
		path, err := ew.WaitExtra(ctx, dst, d.Index)
		if err != nil {
			return fail(err)
		}
		ns.Extra = append(ns.Extra, virt.ExtraDisk{Index: d.Index, Path: path, SizeGB: d.SizeGB})
	}
	note := ""
	if sp.SeedISO != "" {
		seed, err := s.Seeds.Build(ctx, dst, cloudinit.Spec{InstanceID: ns.UUID, Hostname: r.Name,
			SSHKeys: s.defaultKeys(sp.Owner), DHCPByMAC: ns.MAC})
		if err != nil {
			return fail(err)
		}
		ns.SeedISO = seed
		undo = append(undo, func() { s.logErr("rollback seed", s.Seeds.Remove(dst)) })
	} else {
		note = " (installed from an ISO: it is an exact copy — same computer name and IDs inside; Windows: sysprep first)"
	}
	if b, err := os.ReadFile(s.nvramPath(src)); err == nil { // UEFI boot entries
		np := s.nvramPath(dst)
		if err := os.MkdirAll(filepath.Dir(np), 0o755); err == nil {
			s.logErr("copy uefi variables", os.WriteFile(np, b, 0o600))
			undo = append(undo, func() { _ = os.Remove(np) })
		}
	}
	x, err := virt.DomainXML(ns)
	if err != nil {
		return fail(err)
	}
	if err := s.HV.Define(ctx, x); err != nil {
		return fail(err)
	}
	undo = append(undo, func() { s.logErr("rollback define", s.HV.Undefine(bg, dst)) })
	if err := s.HV.SetAutostart(ctx, dst, false); err != nil {
		return fail(err)
	}
	if fw, err := s.GetFirewall(ctx, name); err == nil && (fw.Enabled || len(fw.Rules) > 0) {
		if _, err := s.SetFirewall(ctx, r.Name, fw); err != nil {
			note += "; firewall rules not copied: " + err.Error()
		}
	}
	msg := fmt.Sprintf("%s copied to %s%s", name, r.Name, note)
	if r.Start {
		if err := s.HV.Start(ctx, dst); err != nil {
			return msg + "; starting it failed: " + err.Error(), nil
		}
		msg += " and started"
	}
	return msg, nil
}
