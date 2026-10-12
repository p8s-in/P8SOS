package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"p8sos/internal/isos"
	"p8sos/internal/jobs"
	"p8sos/internal/screen"
	"p8sos/internal/virt"
)

// ISO is an installer image in the host's library.
type ISO = isos.ISO

type diskCreator interface {
	CreateDisk(ctx context.Context, vm string, sizeGB int) (string, error)
}

func (s *Service) isoLib() (*isos.Library, error) {
	if s.ISOs == nil {
		return nil, fmt.Errorf("%w: the ISO library is not configured", ErrConflict)
	}
	return s.ISOs, nil
}

func isoErr(err error) error {
	switch {
	case errors.Is(err, isos.ErrNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, isos.ErrExists):
		return fmt.Errorf("%w: %v", ErrExists, err)
	case errors.Is(err, isos.ErrInvalid):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return err
}

// createFromISO makes a VM with an empty disk that boots an installer CD.
func (s *Service) createFromISO(ctx context.Context, r CreateRequest) (VM, error) {
	lib, err := s.isoLib()
	if err != nil {
		return VM{}, err
	}
	var errs []string
	if !reName.MatchString(r.Name) {
		errs = append(errs, "name must be 2-49 chars: lowercase letters, digits, hyphens; start with a letter")
	}
	isoPath, perr := lib.Path(r.ISO)
	if perr != nil {
		errs = append(errs, fmt.Sprintf("no ISO %q in the library", r.ISO))
	}
	if r.Template != "" || r.RootPassword != "" || r.UserData != "" || len(r.SSHKeys) > 0 {
		errs = append(errs, "template, root_password, user_data and ssh_keys don't apply to ISO installs (you set them up in the installer)")
	}
	if r.VCPUs == 0 {
		r.VCPUs = 2
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = 2048
	}
	if r.DiskGB == 0 {
		r.DiskGB = 32
	}
	if r.VCPUs < 1 || r.VCPUs > s.Limits.MaxVCPUs {
		errs = append(errs, fmt.Sprintf("vcpus must be 1-%d", s.Limits.MaxVCPUs))
	}
	if r.MemoryMiB < 256 || r.MemoryMiB > s.Limits.MaxMemoryMiB {
		errs = append(errs, fmt.Sprintf("memory_mib must be 256-%d", s.Limits.MaxMemoryMiB))
	}
	if r.DiskGB < 1 || r.DiskGB > s.Limits.MaxDiskGB {
		errs = append(errs, fmt.Sprintf("disk_gb must be 1-%d", s.Limits.MaxDiskGB))
	}
	switch r.OSType {
	case "":
		r.OSType = "linux"
	case "linux", "windows":
	default:
		errs = append(errs, `os_type must be "linux" or "windows"`)
	}
	if msg := s.checkPool(&r); msg != "" {
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
		errs = append(errs, `network must be "nat", "lan" or "public"`)
	}
	if len(errs) > 0 {
		return VM{}, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
	}
	dc, ok := s.Disks.(diskCreator)
	if !ok {
		return VM{}, errors.New("this disk backend can't create empty disks")
	}
	dom := Prefix + r.Name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return VM{}, fmt.Errorf("%w: vm %q", ErrExists, r.Name)
	} else if !errors.Is(err, virt.ErrNoDomain) {
		return VM{}, err
	}
	if err := s.Disks.DestroyDisk(ctx, dom); err != nil { // leftover of an interrupted create
		return VM{}, fmt.Errorf("remove leftover disk: %w", err)
	}
	bg := context.WithoutCancel(ctx)
	disk, err := dc.CreateDisk(ctx, dom, r.DiskGB)
	if err != nil {
		return VM{}, err
	}
	bridge := s.bridgeFor(r.Network)
	release := func() {}
	if r.IPPool != "" { // the address is typed into the installer (shown in Manage)
		l, rel, err := s.lease(r.IPPool, r.Name, "")
		if err != nil {
			s.logErr("rollback disk", s.Disks.DestroyDisk(bg, dom))
			return VM{}, err
		}
		bridge, release = l.Bridge, rel
	}
	fail := func(err error, undefine bool) (VM, error) {
		if undefine {
			s.logErr("rollback define", s.HV.Undefine(bg, dom))
		}
		s.logErr("rollback disk", s.Disks.DestroyDisk(bg, dom))
		release()
		return VM{}, err
	}
	spec := virt.Spec{
		Name: dom, UUID: newUUID(), VCPUs: r.VCPUs, MemoryMiB: r.MemoryMiB, DiskGB: r.DiskGB,
		DiskPath: disk, InstallISO: isoPath, OSType: r.OSType, Bridge: bridge, MAC: virt.RandomMAC(),
		Template: "iso:" + r.ISO, CreatedAt: time.Now().UTC().Format(time.RFC3339), Owner: r.Owner,
	}
	x, err := virt.DomainXML(spec)
	if err != nil {
		return fail(err, false)
	}
	if err := s.HV.Define(ctx, x); err != nil {
		return fail(err, false)
	}
	if err := s.HV.SetAutostart(ctx, dom, r.Autostart == nil || *r.Autostart); err != nil {
		return fail(err, true)
	}
	if r.Start == nil || *r.Start {
		if err := s.HV.Start(ctx, dom); err != nil {
			return fail(err, true)
		}
		s.afterStart(ctx, r.Name)
	}
	return s.get(ctx, dom)
}

// Screenshot returns a running VM's screen as PNG.
func (s *Service) Screenshot(ctx context.Context, name string) ([]byte, error) {
	dom := Prefix + name
	st, err := s.state(ctx, dom)
	if err != nil {
		return nil, err
	}
	if st != virt.Running {
		return nil, fmt.Errorf("%w: vm %q is %s", ErrConflict, name, st)
	}
	b, err := s.HV.Screenshot(ctx, dom)
	if err != nil {
		return nil, err
	}
	return screen.ToPNG(b)
}

// EjectISO takes the installer CD out of a stopped VM (after installing).
func (s *Service) EjectISO(ctx context.Context, name string) (VM, error) {
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if st != virt.Stopped {
		return VM{}, fmt.Errorf("%w: stop vm %q before ejecting the CD (it is %s)", ErrConflict, name, st)
	}
	if err := s.redefine(ctx, dom, func(sp *virt.Spec) { sp.InstallISO = "" }); err != nil {
		return VM{}, err
	}
	return s.get(ctx, dom)
}

// ListISOs returns the library.
func (s *Service) ListISOs() ([]ISO, error) {
	lib, err := s.isoLib()
	if err != nil {
		return nil, err
	}
	return lib.List()
}

// SaveISO stores an uploaded ISO.
func (s *Service) SaveISO(name string, r io.Reader) (ISO, error) {
	lib, err := s.isoLib()
	if err != nil {
		return ISO{}, err
	}
	iso, err := lib.Save(name, r)
	return iso, isoErr(err)
}

// StartISODownload fetches an ISO in the background.
func (s *Service) StartISODownload(ctx context.Context, url, name, sha256 string) (Job, error) {
	lib, err := s.isoLib()
	if err != nil {
		return Job{}, err
	}
	if s.Jobs == nil {
		return Job{}, errBackupsOff
	}
	if name == "" {
		name = isos.NameFromURL(url)
	}
	if !isos.ValidName(name) {
		return Job{}, fmt.Errorf("%w: give a name ending in .iso", ErrInvalid)
	}
	if _, err := lib.Path(name); err == nil {
		return Job{}, fmt.Errorf("%w: ISO %q", ErrExists, name)
	}
	j, err := s.Jobs.Start("download", "iso:"+name, func(ctx context.Context, p jobs.Progress) (string, error) {
		iso, err := lib.Download(ctx, url, name, sha256, func(done, total int64) {
			if total > 0 {
				p(fmt.Sprintf("%s of %s (%d%%)", humanBytes(done), humanBytes(total), done*100/total))
			} else {
				p(humanBytes(done) + " so far")
			}
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s downloaded (%s)", iso.Name, humanBytes(iso.Bytes)), nil
	})
	if errors.Is(err, jobs.ErrBusy) {
		return Job{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return j, err
}

// DeleteISO removes an ISO that no VM is using.
func (s *Service) DeleteISO(ctx context.Context, name string) error {
	lib, err := s.isoLib()
	if err != nil {
		return err
	}
	p, err := lib.Path(name)
	if err != nil {
		return isoErr(err)
	}
	names, err := s.HV.List(ctx)
	if err != nil {
		return err
	}
	for _, n := range names {
		if !strings.HasPrefix(n, Prefix) {
			continue
		}
		if b, err := s.HV.XML(ctx, n); err == nil {
			if sp, err := virt.ParseDomainXML(b); err == nil && sp.InstallISO == p {
				return fmt.Errorf("%w: VM %q still has %s in its CD drive (stop it and eject first)", ErrConflict, strings.TrimPrefix(n, Prefix), name)
			}
		}
	}
	return isoErr(lib.Delete(name))
}

// UpgradeDomains brings older VM definitions up to the current device set
// (e.g. the VGA display adapter). Running VMs pick changes up at their next
// power-off/start; their names are returned.
func (s *Service) UpgradeDomains(ctx context.Context) (needRestart []string, err error) {
	names, err := s.HV.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, dom := range names {
		if !strings.HasPrefix(dom, Prefix) {
			continue
		}
		b, err := s.HV.XML(ctx, dom)
		if err != nil {
			continue
		}
		sp, err := virt.ParseDomainXML(b)
		if err != nil || sp.Video == virt.VideoModel {
			continue
		}
		unlock := s.lock(dom)
		st, _ := s.HV.State(ctx, dom)
		err = s.redefine(ctx, dom, func(*virt.Spec) {})
		unlock()
		if err != nil {
			s.logErr("upgrade "+dom, err)
			continue
		}
		if st != virt.Stopped {
			needRestart = append(needRestart, strings.TrimPrefix(dom, Prefix))
		}
	}
	return needRestart, nil
}
