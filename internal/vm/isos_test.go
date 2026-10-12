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
