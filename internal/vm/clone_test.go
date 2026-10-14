package vm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

// cloneHV can run scripts in guests and shuts down at once.
type cloneHV struct {
	*fakeHV
	scripts []string
	failRun bool
}

func (h *cloneHV) GuestExec(_ context.Context, n, script string, _ time.Duration) (int, string, error) {
	if h.failRun {
		return -1, "", errors.New("guest agent is not connected")
	}
	h.scripts = append(h.scripts, n+":"+script)
	return 0, "", nil
}
func (h *cloneHV) Shutdown(_ context.Context, n string) error { return h.set(n, virt.Stopped) }

// cloneDisksFake copies disks and makes templates in memory.
type cloneDisksFake struct {
	*fakeDisks
	copies    []string
	templates map[string]bool
	inUse     bool
}

func (d *cloneDisksFake) CloneDisks(_ context.Context, vm, snap, newVM string, idx []int) error {
	for _, i := range idx {
		d.copies = append(d.copies, fmt.Sprintf("%s/%d@%s->%s", vm, i, snap, newVM))
	}
	d.live[newVM] = true
	return nil
}
func (d *cloneDisksFake) WaitDevice(_ context.Context, vm string) (string, error) {
	return "/dev/zvol/tank/vms/" + vm + "-disk0", nil
}
func (d *cloneDisksFake) WaitExtra(_ context.Context, vm string, i int) (string, error) {
	return fmt.Sprintf("/dev/zvol/tank/vms/%s-disk%d", vm, i), nil
}
func (d *cloneDisksFake) MakeTemplate(_ context.Context, vm, snap, ds string) (string, error) {
	d.templates[ds] = true
	return ds + "@" + snap, nil
}
func (d *cloneDisksFake) DestroyTemplate(_ context.Context, ds string) error {
	if d.inUse {
		return errors.New("VMs made from it still use it")
	}
	delete(d.templates, ds)
	return nil
}

func cloneSvc(t *testing.T) (*Service, *cloneHV, *cloneDisksFake, *fakeSeeds) {
	svc, hv, d, seeds := newSvc()
	ch, cd := &cloneHV{fakeHV: hv}, &cloneDisksFake{fakeDisks: d, templates: map[string]bool{}}
	svc.HV, svc.Disks = ch, cd
	svc.Jobs = &jobs.Manager{}
	svc.TemplatesFile = filepath.Join(t.TempDir(), "templates.json")
	svc.NVRAMDir = t.TempDir()
	return svc, ch, cd, seeds
}

func TestClone(t *testing.T) {
	ctx := context.Background()
	svc, hv, cd, seeds := cloneSvc(t)
	if _, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 20}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddDisk(ctx, "web1", 50); err != nil {
		t.Fatal(err)
	}
	src, _ := virt.ParseDomainXML(hv.doms["p8s-web1"])
	if _, err := svc.StartClone(ctx, "web1", CloneRequest{Name: "Bad Name"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := svc.StartClone(ctx, "web1", CloneRequest{Name: "web1"}); !errors.Is(err, ErrExists) {
		t.Fatalf("same name: %v", err)
	}
	j, err := svc.StartClone(ctx, "web1", CloneRequest{Name: "web2", Start: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, svc, j); got.State != "done" || !strings.Contains(got.Message, "copied to web2 and started") {
		t.Fatalf("job %+v", got)
	}
	if len(cd.copies) != 2 || !strings.HasPrefix(cd.copies[0], "p8s-web1/0@p8s-clone-") || !strings.HasSuffix(cd.copies[1], "->p8s-web2") {
		t.Fatalf("copies %v", cd.copies)
	}
	sp, _ := virt.ParseDomainXML(hv.doms["p8s-web2"])
	if sp.UUID == src.UUID || sp.MAC == src.MAC || sp.VCPUs != 2 || sp.MemoryMiB != 2048 || len(sp.Extra) != 1 || sp.Extra[0].Path != "/dev/zvol/tank/vms/p8s-web2-disk1" {
		t.Fatalf("clone spec %+v (source %+v)", sp, src)
	}
	if seeds.last.Hostname != "web2" || seeds.last.InstanceID != sp.UUID || seeds.last.DHCPByMAC != sp.MAC {
		t.Fatalf("seed %+v", seeds.last)
	}
	if hv.frozen == 0 {
		t.Fatal("running source not frozen for the snapshot")
	}
	if snaps, _ := svc.Snapshots(ctx, "web1"); len(snaps) != 0 {
		t.Fatalf("clone snapshot left behind: %v", snaps)
	}
	if v, _ := svc.Get(ctx, "web2"); v.State != virt.Running || v.Autostart {
		t.Fatalf("clone %+v", v)
	}
}

func TestTemplateFromVM(t *testing.T) {
	ctx := context.Background()
	svc, hv, cd, seeds := cloneSvc(t)
	if _, err := svc.Create(ctx, CreateRequest{Name: "base1", Template: "debian-13", VCPUs: 1, MemoryMiB: 1024, DiskGB: 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartMakeTemplate(ctx, "base1", TemplateRequest{Name: "debian-13"}); !errors.Is(err, ErrExists) {
		t.Fatalf("name of a built-in template: %v", err)
	}
	hv.failRun = true
	j, err := svc.StartMakeTemplate(ctx, "base1", TemplateRequest{Name: "web-base", Label: "Web server base"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, svc, j); got.State != "failed" || !strings.Contains(got.Error, "qemu-guest-agent") {
		t.Fatalf("no agent: %+v", got)
	}
	hv.failRun = false
	j, _ = svc.StartMakeTemplate(ctx, "base1", TemplateRequest{Name: "web-base", Label: "Web server base"})
	if got := waitJob(t, svc, j); got.State != "done" || !strings.Contains(got.Message, "cleaned and shut down") {
		t.Fatalf("job %+v", got)
	}
	if len(hv.scripts) != 1 || !strings.Contains(hv.scripts[0], "cloud-init clean") || !strings.Contains(hv.scripts[0], "/etc/machine-id") {
		t.Fatalf("seal %v", hv.scripts)
	}
	if !cd.templates["tank/t/web-base"] {
		t.Fatalf("template dataset %v", cd.templates)
	}
	l := svc.ListTemplates(ctx)
	if len(l) != 2 || !l[1].Custom || l[1].Name != "web-base" || l[1].From != "base1" || l[1].MinDiskGB != 12 || l[1].Label != "Web server base" {
		t.Fatalf("templates %+v", l)
	}
	// a VM from it: DHCP by MAC (it may share the machine id with others from the same image)
	v, err := svc.Create(ctx, CreateRequest{Name: "web7", Template: "web-base"})
	if err != nil {
		t.Fatal(err)
	}
	if v.DiskGB != 12 || seeds.last.DHCPByMAC == "" {
		t.Fatalf("vm from template %+v, seed %+v", v, seeds.last)
	}
	cd.inUse = true
	if err := svc.DeleteTemplate(ctx, "web-base"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete while used: %v", err)
	}
	cd.inUse = false
	if err := svc.DeleteTemplate(ctx, "debian-13"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete built-in: %v", err)
	}
	if err := svc.DeleteTemplate(ctx, "web-base"); err != nil || len(svc.ListTemplates(ctx)) != 1 {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.DeleteTemplate(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
