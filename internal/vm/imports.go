package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"p8sos/internal/imports"
	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

// Importing VMs from other hypervisors (Proxmox backups, VMware/VirtualBox/
// Hyper-V disk images): files are put in ImportDir (upload in the dashboard,
// or scp), inspected into a Plan, then imported as a job.

// ImportFile is a file waiting to be imported.
type ImportFile struct {
	Name     string `json:"name"`
	Bytes    int64  `json:"bytes"`
	Kind     string `json:"kind"` // proxmox-vma | disk-image
	Modified string `json:"modified"`
}

// ImportRequest: the plan's fields, changed where the person wants.
type ImportRequest struct {
	File      string `json:"file"`
	Name      string `json:"name"`
	VCPUs     int    `json:"vcpus,omitempty"`
	MemoryMiB int    `json:"memory_mib,omitempty"`
	DiskGB    int    `json:"disk_gb,omitempty"` // grow the disk (never smaller than the source)
	OSType    string `json:"os_type,omitempty"`
	Firmware  string `json:"firmware,omitempty"`
	DiskBus   string `json:"disk_bus,omitempty"`
	NICModel  string `json:"nic_model,omitempty"`
	KeepMAC   bool   `json:"keep_mac,omitempty"` // keep the old network card address (stop the original first!)
	Network   string `json:"network,omitempty"`  // nat (default) or lan
	Owner     string `json:"owner,omitempty"`
	Start     *bool  `json:"start,omitempty"`     // default true
	Autostart *bool  `json:"autostart,omitempty"` // default true
}

var reImportFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+ -]{0,200}$`)

func (s *Service) importPath(name string) (string, error) {
	if s.ImportDir == "" {
		return "", fmt.Errorf("%w: imports are not set up on this host", ErrConflict)
	}
	if !reImportFile.MatchString(name) || strings.Contains(name, "..") || imports.KindOf(name) == "" {
		return "", fmt.Errorf("%w: %q isn't an importable file name (.vma[.zst|.lzo|.gz], .ova, .qcow2, .vmdk, .vhd, .vhdx, .vdi, .img, .raw)", ErrInvalid, name)
	}
	return filepath.Join(s.ImportDir, name), nil
}

// ListImports lists the import folder.
func (s *Service) ListImports() ([]ImportFile, error) {
	if s.ImportDir == "" {
		return []ImportFile{}, nil
	}
	ents, err := os.ReadDir(s.ImportDir)
	if errors.Is(err, os.ErrNotExist) {
		return []ImportFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []ImportFile{}
	for _, e := range ents {
		k := imports.KindOf(e.Name())
		if e.IsDir() || k == "" || !reImportFile.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, ImportFile{Name: e.Name(), Bytes: fi.Size(), Kind: k, Modified: fi.ModTime().UTC().Format(time.RFC3339)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified > out[j].Modified })
	return out, nil
}

// ImportPlan inspects a file.
func (s *Service) ImportPlan(name string) (imports.Plan, error) {
	p, err := s.importPath(name)
	if err != nil {
		return imports.Plan{}, err
	}
	if _, err := os.Stat(p); err != nil {
		return imports.Plan{}, fmt.Errorf("%w: no file %q in the import folder", ErrNotFound, name)
	}
	pl, err := imports.Inspect(p)
	if err != nil {
		return imports.Plan{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	pl.Warnings = append(pl.Warnings, s.nameWarning(pl.Name)...)
	return pl, nil
}

func (s *Service) nameWarning(name string) []string {
	if _, err := s.HV.State(context.Background(), Prefix+name); err == nil {
		return []string{fmt.Sprintf("a VM called %q exists already: pick another name", name)}
	}
	return nil
}

// SaveImport stores an uploaded file in the import folder.
func (s *Service) SaveImport(name string, r io.Reader) (ImportFile, error) {
	p, err := s.importPath(name)
	if err != nil {
		return ImportFile{}, err
	}
	if err := os.MkdirAll(s.ImportDir, 0o750); err != nil {
		return ImportFile{}, err
	}
	if _, err := os.Stat(p); err == nil {
		return ImportFile{}, fmt.Errorf("%w: %q is already in the import folder", ErrExists, name)
	}
	tmp := p + ".partial"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return ImportFile{}, err
	}
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
		return ImportFile{}, err
	}
	return ImportFile{Name: name, Bytes: n, Kind: imports.KindOf(name), Modified: time.Now().UTC().Format(time.RFC3339)}, nil
}

// DeleteImport removes a file from the import folder.
func (s *Service) DeleteImport(name string) error {
	p, err := s.importPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: no file %q in the import folder", ErrNotFound, name)
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Service) nvramPath(dom string) string {
	d := s.NVRAMDir
	if d == "" {
		d = "/var/lib/libvirt/qemu/nvram"
	}
	return filepath.Join(d, dom+"_VARS.fd")
}

// StartImport checks the request and imports the file in the background.
func (s *Service) StartImport(ctx context.Context, r ImportRequest) (Job, error) {
	path, err := s.importPath(r.File)
	if err != nil {
		return Job{}, err
	}
	pl, err := s.ImportPlan(r.File)
	if err != nil {
		return Job{}, err
	}
	if pl.Kind == imports.KindVMA && pl.BootDevice == "" {
		return Job{}, fmt.Errorf("%w: the backup has no disk to import", ErrInvalid)
	}
	// the request overrides the plan
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&pl.Name, r.Name)
	set(&pl.OSType, r.OSType)
	set(&pl.Firmware, r.Firmware)
	set(&pl.DiskBus, r.DiskBus)
	set(&pl.NICModel, r.NICModel)
	if r.VCPUs > 0 {
		pl.VCPUs = r.VCPUs
	}
	if r.MemoryMiB > 0 {
		pl.MemoryMiB = r.MemoryMiB
	}
	diskGB := max(pl.DiskGB, r.DiskGB, 1)
	var errs []string
	if !reName.MatchString(pl.Name) {
		errs = append(errs, "name must be 2-49 chars: lowercase letters, digits, hyphens; start with a letter")
	}
	if pl.VCPUs < 1 || pl.VCPUs > s.Limits.MaxVCPUs {
		errs = append(errs, fmt.Sprintf("vcpus must be 1-%d", s.Limits.MaxVCPUs))
	}
	if pl.MemoryMiB < 256 || pl.MemoryMiB > s.Limits.MaxMemoryMiB {
		errs = append(errs, fmt.Sprintf("memory_mib must be 256-%d", s.Limits.MaxMemoryMiB))
	}
	if diskGB > s.Limits.MaxDiskGB {
		errs = append(errs, fmt.Sprintf("the disk (%d GB) is larger than this host allows (%d GB)", diskGB, s.Limits.MaxDiskGB))
	}
	switch pl.OSType {
	case "linux", "windows":
	case "windows11":
		if s.TPMReady != nil && !s.TPMReady() {
			errs = append(errs, "Windows 11 needs the software TPM (swtpm), which isn't installed; run: sudo bash update.sh")
		}
	default:
		errs = append(errs, `os_type must be "linux", "windows" or "windows11"`)
	}
	if pl.Firmware != "bios" && pl.Firmware != "uefi" {
		errs = append(errs, `firmware must be "bios" or "uefi"`)
	}
	if !virt.DiskBuses[pl.DiskBus] {
		errs = append(errs, `disk_bus must be "virtio", "scsi" or "sata"`)
	}
	if !virt.NICModels[pl.NICModel] {
		errs = append(errs, `nic_model must be virtio, e1000e, e1000, rtl8139 or vmxnet3`)
	}
	network := r.Network
	switch network {
	case "", NetNAT:
		network = NetNAT
	case NetLAN:
		if !s.LANAvailable() {
			errs = append(errs, "network \"lan\" is not set up on this host")
		}
	default:
		if IsVNet(network) {
			if msg := s.checkVNet(network); msg != "" {
				errs = append(errs, msg)
			}
			break
		}
		errs = append(errs, `network must be "nat", "lan" or "vnet:NAME"`)
	}
	if _, err := s.HV.State(ctx, Prefix+pl.Name); err == nil {
		errs = append(errs, fmt.Sprintf("a VM called %q exists already", pl.Name))
	}
	if len(errs) > 0 {
		return Job{}, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
	}
	mac := virt.RandomMAC()
	if r.KeepMAC && pl.MAC != "" {
		mac = pl.MAC
	}
	start := r.Start == nil || *r.Start
	auto := r.Autostart == nil || *r.Autostart
	j, err := s.Jobs.Start("import", pl.Name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return s.runImport(ctx, path, pl, diskGB, mac, network, r.Owner, start, auto, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return Job{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return j, err
}

func (s *Service) runImport(ctx context.Context, path string, pl imports.Plan, diskGB int, mac, network, owner string, start, auto bool, p jobs.Progress) (string, error) {
	dom := Prefix + pl.Name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return "", fmt.Errorf("vm %q was created meanwhile", pl.Name)
	}
	dc, ok := s.Disks.(diskCreator)
	if !ok {
		return "", errors.New("this disk backend can't create disks")
	}
	bg := context.WithoutCancel(ctx)
	var undo []func()
	fail := func(err error) (string, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return "", err
	}
	if err := s.dropLeftover(ctx, dom); err != nil { // leftover of an interrupted import
		return "", fmt.Errorf("remove leftover disk: %w", err)
	}
	p(fmt.Sprintf("creating a %d GB disk", diskGB))
	disk, err := dc.CreateDisk(ctx, dom, diskGB)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, func() { s.logErr("rollback imported disk", s.Disks.DestroyDisk(bg, dom)) })

	var nvram []byte
	var extra []virt.ExtraDisk
	switch pl.Kind {
	case imports.KindOVA:
		ex, err := s.fillFromOVA(ctx, path, pl, dom, disk, p)
		if err != nil {
			return fail(err)
		}
		extra = ex
		for _, e := range ex {
			i := e.Index
			undo = append(undo, func() {
				if ed, ok := s.Disks.(extraDisks); ok {
					s.logErr("rollback imported disk", ed.DestroyExtra(bg, dom, i))
				}
			})
		}
	case imports.KindVMA:
		nv, err := s.fillFromVMA(ctx, path, pl, disk, p)
		if err != nil {
			return fail(err)
		}
		nvram = nv
	default:
		p("copying the disk image")
		last := -10.0
		if err := imports.WriteDisk(ctx, path, disk, func(pct float64) {
			if pct-last >= 2 {
				last = pct
				p(fmt.Sprintf("copying the disk image: %.0f%%", pct))
			}
		}); err != nil {
			return fail(err)
		}
	}
	if pl.Firmware == "uefi" && len(nvram) > 0 {
		np := s.nvramPath(dom)
		if err := os.MkdirAll(filepath.Dir(np), 0o755); err == nil {
			if err := os.WriteFile(np, nvram, 0o600); err != nil {
				s.logErr("write uefi variables", err)
			} else {
				undo = append(undo, func() { _ = os.Remove(np) })
			}
		}
	}
	firmware := ""
	if pl.Firmware == "uefi" {
		firmware = "uefi"
	}
	spec := virt.Spec{
		Name: dom, UUID: newUUID(), VCPUs: pl.VCPUs, MemoryMiB: pl.MemoryMiB, DiskGB: diskGB, DiskPath: disk,
		Bridge: s.bridgeFor(network), MAC: mac, OSType: pl.OSType, Template: "import:" + pl.File,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Owner: owner,
		DiskBus: pl.DiskBus, NICModel: pl.NICModel, Firmware: firmware, Extra: extra,
	}
	x, err := virt.DomainXML(spec)
	if err != nil {
		return fail(err)
	}
	if err := s.HV.Define(ctx, x); err != nil {
		return fail(err)
	}
	undo = append(undo, func() { s.logErr("rollback define", s.HV.Undefine(bg, dom)) })
	if err := s.HV.SetAutostart(ctx, dom, auto); err != nil {
		return fail(err)
	}
	if start {
		p("starting")
		if err := s.HV.Start(ctx, dom); err != nil {
			return fail(fmt.Errorf("imported, but it didn't start: %w", err))
		}
		s.afterStart(ctx, pl.Name)
	}
	msg := fmt.Sprintf("%s imported from %s", pl.Name, pl.File)
	if start {
		msg += " and started — open its console to check it boots"
	}
	return msg, nil
}

// fillFromOVA unpacks an appliance's disks next to the .ova, writes the
// first onto disk and the others onto new data disks.
func (s *Service) fillFromOVA(ctx context.Context, path string, pl imports.Plan, dom, disk string, p jobs.Progress) ([]virt.ExtraDisk, error) {
	tmp := filepath.Join(filepath.Dir(path), ".unpack-"+pl.Name)
	defer os.RemoveAll(tmp)
	files, err := imports.ExtractOVA(ctx, path, pl, tmp, func(m string) { p(m) })
	if err != nil {
		return nil, err
	}
	write := func(src, dev, what string) error {
		last := -10.0
		return imports.WriteDisk(ctx, src, dev, func(pct float64) {
			if pct-last >= 2 {
				last = pct
				p(fmt.Sprintf("copying %s: %.0f%%", what, pct))
			}
		})
	}
	if err := write(files[pl.Disks[0].Href], disk, "the system disk"); err != nil {
		return nil, err
	}
	var extra []virt.ExtraDisk
	for k, d := range pl.Disks[1:] {
		if k+1 > virt.MaxExtraDisks {
			break
		}
		ed, ok := s.Disks.(extraDisks)
		if !ok {
			return extra, fmt.Errorf("this server can't add data disks; only the system disk was imported")
		}
		gb := int((d.Bytes + (1 << 30) - 1) >> 30)
		dev, err := ed.CreateExtra(ctx, dom, k+1, max(gb, 1))
		if err != nil {
			return extra, err
		}
		extra = append(extra, virt.ExtraDisk{Index: k + 1, Path: dev, SizeGB: max(gb, 1)})
		if err := write(files[d.Href], dev, fmt.Sprintf("disk %d", k+2)); err != nil {
			return extra, err
		}
	}
	return extra, nil
}

func (s *Service) fillFromVMA(ctx context.Context, path string, pl imports.Plan, disk string, p jobs.Progress) ([]byte, error) {
	rc, err := imports.OpenStream(ctx, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	v, err := imports.OpenVMA(rc)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(disk, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open the new disk: %w", err)
	}
	defer f.Close()
	targets := map[int]io.WriterAt{}
	var efi *memWriter
	var total int64
	for _, d := range v.Devices {
		switch {
		case d.Name == pl.BootDevice:
			targets[d.ID] = f
			total += d.Size
		case d.Name == "drive-efidisk0" && pl.EFIVars:
			efi = &memWriter{b: make([]byte, d.Size)}
			targets[d.ID] = efi
		}
	}
	last := -10.0
	p("reading the Proxmox backup")
	err = v.Extract(targets, func(done int64) {
		if total > 0 {
			pct := float64(done) * 100 / float64(total)
			if pct-last >= 2 {
				last = pct
				p(fmt.Sprintf("writing the disk: %.0f%%", min(pct, 100)))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if efi != nil {
		return efi.b, nil
	}
	return nil, nil
}

type memWriter struct{ b []byte }

func (m *memWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || int(off)+len(p) > len(m.b) {
		return 0, errors.New("write past the end")
	}
	return copy(m.b[off:], p), nil
}
