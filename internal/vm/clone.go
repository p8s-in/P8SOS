package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	Name    string `json:"name"`
	Start   bool   `json:"start"`
	Storage string `json:"storage,omitempty"` // "" = the same storage as the VM
	// Linked: an instant clone sharing the VM's disk blocks (ZFS clones of a
	// snapshot); the VM can't be deleted while it has linked clones.
	Linked bool `json:"linked,omitempty"`
}

// linker makes and tracks linked clones (storage.ZFS).
type linker interface {
	LinkDisks(ctx context.Context, vm, snap, newVM string, idx []int) error
	LinkedClones(ctx context.Context, vm string) []string
	LinkedTo(ctx context.Context, vm string) string
	Unlink(ctx context.Context, vm string, progress func(string)) error
}

// linkedClones are the VMs (domains) linked to dom's disks.
func (s *Service) linkedClones(ctx context.Context, dom string) []string {
	l, ok := s.Disks.(linker)
	if !ok {
		return nil
	}
	var out []string
	for _, c := range l.LinkedClones(ctx, dom) {
		out = append(out, strings.TrimPrefix(c, Prefix))
	}
	return out
}

// StartUnlink makes a linked clone independent (a full copy of its disks) in
// the background; the VM must be shut down.
func (s *Service) StartUnlink(ctx context.Context, name string) (Job, error) {
	l, ok := s.Disks.(linker)
	if !ok || s.Jobs == nil {
		return Job{}, fmt.Errorf("%w: this server's storage has no linked clones", ErrConflict)
	}
	dom := Prefix + name
	st, err := s.state(ctx, dom)
	if err != nil {
		return Job{}, err
	}
	src := l.LinkedTo(ctx, dom)
	if src == "" {
		return Job{}, fmt.Errorf("%w: %s isn't a linked clone", ErrConflict, name)
	}
	if st != virt.Stopped {
		return Job{}, fmt.Errorf("%w: shut %s down first (its disks are copied while it is off)", ErrConflict, name)
	}
	j, err := s.Jobs.Start("unlink", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		defer s.lock(dom)()
		if st, err := s.state(ctx, dom); err != nil {
			return "", err
		} else if st != virt.Stopped {
			return "", fmt.Errorf("%s was started meanwhile; shut it down and try again", name)
		}
		if err := l.Unlink(ctx, dom, func(m string) { p(m) }); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s is independent of %s now (its disks are full copies)", name, strings.TrimPrefix(src, Prefix)), nil
	})
	if errors.Is(err, jobs.ErrBusy) {
		return Job{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return j, err
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
	if r.Linked {
		if _, ok := s.Disks.(linker); !ok {
			return Job{}, fmt.Errorf("%w: this server's storage can't make linked clones", ErrConflict)
		}
		cur := s.storageOf(Prefix + name)
		if cur == "" {
			cur = "local"
		}
		if r.Storage != "" && r.Storage != cur {
			return Job{}, fmt.Errorf("%w: a linked clone stays on its VM's storage; make a full copy to put it elsewhere", ErrInvalid)
		}
		if s.shared(Prefix + name) {
			return Job{}, fmt.Errorf("%w: linked clones need ZFS storage (local, SAN or NAS); on Ceph make a full copy", ErrInvalid)
		}
		r.Storage = ""
	}
	if r.Storage != "" {
		if msg := s.checkStorage(r.Storage); msg != "" {
			return Job{}, fmt.Errorf("%w: %s", ErrInvalid, msg)
		}
		if _, ok := s.Disks.(diskRelocator); !ok {
			return Job{}, fmt.Errorf("%w: this server can't move disks between storages", ErrInvalid)
		}
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
	if r.Linked { // stays as long as the clone exists
		snap = fmt.Sprintf("p8s-link-%s-%d", r.Name, time.Now().Unix())
	}
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
	linked := false
	defer func() {
		if !linked {
			s.logErr("drop clone snapshot", sd.DestroySnapshot(bg, src, snap))
		}
	}()

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
	if r.Linked {
		p(fmt.Sprintf("linking %d disk(s)", len(idx)))
		if err := s.Disks.(linker).LinkDisks(ctx, src, snap, dst, idx); err != nil {
			return "", err
		}
		linked = true // from here the snapshot goes with the clone's disks (DestroyDisk)
	} else {
		p(fmt.Sprintf("copying %d disk(s)", len(idx)))
		if err := cd.CloneDisks(ctx, src, snap, dst, idx); err != nil {
			return "", err
		}
	}
	var undo []func()
	fail := func(err error) (string, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return "", err
	}
	undo = append(undo, func() { s.logErr("rollback cloned disks", s.Disks.DestroyDisk(bg, dst)) })
	cur := storage
	if cur == "" {
		cur = "local"
	}
	if r.Storage != "" && r.Storage != cur { // the copy goes to another storage
		p("moving the copy to " + r.Storage)
		if _, err := s.Disks.(diskRelocator).Relocate(ctx, dst, r.Storage, idx, func(m string) { p(m) }); err != nil {
			return fail(err)
		}
	}

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
	if r.Linked {
		msg = fmt.Sprintf("%s made as a linked clone of %s%s", r.Name, name, note)
	}
	if r.Start {
		if err := s.HV.Start(ctx, dst); err != nil {
			return msg + "; starting it failed: " + err.Error(), nil
		}
		msg += " and started"
	}
	return msg, nil
}
