package vm

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"p8sos/internal/isos"
	"p8sos/internal/virt"
)

func isoBytes() []byte { b := make([]byte, 40000); copy(b[32769:], "CD001"); return b }

func TestCreateFromISO(t *testing.T) {
	ctx := context.Background()
	svc, hv, disks, seeds := newSvc()
	if _, err := svc.Create(ctx, CreateRequest{Name: "w1", ISO: "x.iso"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("no library: %v", err)
	}
	svc.ISOs = &isos.Library{Dir: t.TempDir()}
	if _, err := svc.SaveISO("win2022.iso", bytes.NewReader(isoBytes())); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveISO("bad.iso", strings.NewReader("nope")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("not an iso: %v", err)
	}

	v, err := svc.Create(ctx, CreateRequest{Name: "win1", ISO: "win2022.iso", OSType: "windows", DiskGB: 64, MemoryMiB: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != virt.Running || v.ISO != "win2022.iso" || v.User != "" || v.Template != "iso:win2022.iso" || v.DiskGB != 64 || v.VCPUs != 2 {
		t.Fatalf("vm %+v", v)
	}
	if !disks.live["p8s-win1"] || disks.size["p8s-win1"] != 64 || seeds.live["p8s-win1"] {
		t.Fatalf("disk %v size %v seed %v", disks.live, disks.size, seeds.live)
	}
	sp, _ := virt.ParseDomainXML(hv.doms["p8s-win1"])
	if sp.OSType != "windows" || !strings.HasSuffix(sp.InstallISO, "/win2022.iso") {
		t.Fatalf("spec %+v", sp)
	}
	for _, bad := range []CreateRequest{
		{Name: "a1", ISO: "missing.iso"},
		{Name: "a1", ISO: "win2022.iso", OSType: "beos"},
		{Name: "a1", ISO: "win2022.iso", RootPassword: "longenough"},
		{Name: "a1", ISO: "win2022.iso", DiskGB: 99999},
	} {
		if _, err := svc.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	// In use: can't delete; eject (stopped) then delete.
	if err := svc.DeleteISO(ctx, "win2022.iso"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete in-use iso: %v", err)
	}
	if _, err := svc.EjectISO(ctx, "win1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("eject running: %v", err)
	}
	_, _ = svc.Stop(ctx, "win1", true)
	if v, err = svc.EjectISO(ctx, "win1"); err != nil || v.ISO != "" {
		t.Fatalf("eject: %v %+v", err, v)
	}
	if sp, _ := virt.ParseDomainXML(hv.doms["p8s-win1"]); sp.OSType != "windows" || sp.InstallISO != "" {
		t.Fatalf("after eject %+v", sp)
	}
	if err := svc.DeleteISO(ctx, "win2022.iso"); err != nil {
		t.Fatal(err)
	}
	if l, _ := svc.ListISOs(); len(l) != 0 {
		t.Fatalf("list %+v", l)
	}
	if err := svc.DeleteISO(ctx, "win2022.iso"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestUpgradeDomains(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	_, _ = svc.Create(ctx, CreateRequest{Name: "old1", Template: "debian-13"})
	_, _ = svc.Create(ctx, CreateRequest{Name: "old2", Template: "debian-13"})
	_, _ = svc.Stop(ctx, "old2", true)
	for _, d := range []string{"p8s-old1", "p8s-old2"} { // pretend: defined before the video change
		hv.doms[d] = []byte(strings.Replace(string(hv.doms[d]), `type="vga"`, `type="cirrus"`, 1))
	}
	hv.state["p8s-old1"] = virt.Running
	need, err := svc.UpgradeDomains(ctx)
	if err != nil || len(need) != 1 || need[0] != "old1" {
		t.Fatalf("need restart %v %v", need, err)
	}
	for _, d := range []string{"p8s-old1", "p8s-old2"} {
		if sp, _ := virt.ParseDomainXML(hv.doms[d]); sp.Video != virt.VideoModel {
			t.Fatalf("%s video %q", d, sp.Video)
		}
	}
	if need, _ = svc.UpgradeDomains(ctx); len(need) != 0 {
		t.Fatalf("second run should do nothing: %v", need)
	}
}

func TestCreateWindows11(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	svc.ISOs = &isos.Library{Dir: t.TempDir()}
	for _, n := range []string{"Win11.iso", "virtio-win.iso"} {
		if _, err := svc.SaveISO(n, bytes.NewReader(isoBytes())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "w11", ISO: "Win11.iso", OSType: "windows11", DiskGB: 40, MemoryMiB: 4096}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too small: %v", err)
	}
	svc.TPMReady = func() bool { return false }
	if _, err := svc.Create(ctx, CreateRequest{Name: "w11", ISO: "Win11.iso", OSType: "windows11", DiskGB: 64, MemoryMiB: 4096}); err == nil || !strings.Contains(err.Error(), "swtpm") {
		t.Fatalf("no swtpm: %v", err)
	}
	svc.TPMReady = nil
	v, err := svc.Create(ctx, CreateRequest{Name: "w11", ISO: "Win11.iso", DriversISO: "virtio-win.iso", OSType: "windows11", DiskGB: 64, MemoryMiB: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if v.OSType != "windows11" || v.Drivers != "virtio-win.iso" || v.VCPUs != 2 {
		t.Fatalf("vm %+v", v)
	}
	x := string(hv.doms["p8s-w11"])
	if !strings.Contains(x, `firmware="efi"`) || !strings.Contains(x, "<tpm") || !strings.Contains(x, "virtio-win.iso") {
		t.Fatalf("xml:\n%s", x)
	}
	if err := svc.DeleteISO(ctx, "virtio-win.iso"); !errors.Is(err, ErrConflict) {
		t.Fatalf("drivers iso in use: %v", err)
	}
	// resizing keeps UEFI + TPM (the definition is re-rendered)
	if _, err := svc.Stop(ctx, "w11", true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EjectISO(ctx, "w11"); err != nil {
		t.Fatal(err)
	}
	x = string(hv.doms["p8s-w11"])
	if !strings.Contains(x, `firmware="efi"`) || !strings.Contains(x, "<tpm") || strings.Contains(x, `device="cdrom"`) {
		t.Fatalf("after eject:\n%s", x)
	}
}
