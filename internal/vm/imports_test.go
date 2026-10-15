package vm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/imports"
	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

// fileDisks makes real files as "disks" so imports can write into them.
type fileDisks struct {
	*fakeDisks
	dir string
}

func (d *fileDisks) CreateDisk(ctx context.Context, vm string, gb int) (string, error) {
	if _, err := d.fakeDisks.CreateDisk(ctx, vm, gb); err != nil {
		return "", err
	}
	p := filepath.Join(d.dir, vm+"-disk0")
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return p, f.Truncate(int64(gb) << 20) // MiB instead of GiB keeps the test small
}

func (d *fileDisks) CreateExtra(ctx context.Context, vm string, i, gb int) (string, error) {
	if _, err := d.fakeDisks.CreateExtra(ctx, vm, i, gb); err != nil {
		return "", err
	}
	p := filepath.Join(d.dir, fmt.Sprintf("%s-disk%d", vm, i))
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return p, f.Truncate(int64(gb) << 20)
}

func TestImportProxmoxDataDisks(t *testing.T) {
	ctx := context.Background()
	svc, hv, d, _ := newSvc()
	dir := t.TempDir()
	svc.Disks, svc.Jobs = &fileDisks{fakeDisks: d, dir: dir}, &jobs.Manager{}
	svc.ImportDir = filepath.Join(dir, "import")
	root := bytes.Repeat([]byte("root"), 32<<10)
	d1 := bytes.Repeat([]byte("one!"), 16<<10)
	d2 := bytes.Repeat([]byte("two!"), 8<<10)
	conf := "name: db\nmemory: 2048\nscsi0: local:vm-1-disk-0,size=1G\nscsi1: local:vm-1-disk-1,size=1G\nvirtio0: local:vm-1-disk-2,size=1G\nide2: none,media=cdrom\nboot: order=scsi0;ide2\n"
	raw := imports.BuildVMA(map[string]string{"qemu-server.conf": conf}, []string{"drive-virtio0", "drive-scsi1", "drive-scsi0"},
		map[string][]byte{"drive-scsi0": root, "drive-scsi1": d1, "drive-virtio0": d2})
	f := "vzdump-qemu-102-2026_10_15-12_00_00.vma"
	if _, err := svc.SaveImport(f, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	pl, err := svc.ImportPlan(f)
	if err != nil || pl.BootDevice != "drive-scsi0" || len(pl.DataDisks) != 2 || pl.DataDisks[0].Key != "scsi1" || pl.DataDisks[1].Key != "virtio0" {
		t.Fatalf("plan %+v %v", pl, err)
	}
	j, err := svc.StartImport(ctx, ImportRequest{File: f})
	if err != nil {
		t.Fatal(err)
	}
	if j = waitJob(t, svc, j); j.State != "done" {
		t.Fatalf("job %+v", j)
	}
	for file, want := range map[string][]byte{"p8s-db-disk0": root, "p8s-db-disk1": d1, "p8s-db-disk2": d2} {
		got, _ := os.ReadFile(filepath.Join(dir, file))
		if len(got) < len(want) || !bytes.Equal(got[:len(want)], want) {
			t.Fatalf("%s differs", file)
		}
	}
	sp, _ := virt.ParseDomainXML(hv.doms["p8s-db"])
	if len(sp.Extra) != 2 || sp.Extra[0].Index != 1 || sp.Extra[1].Index != 2 {
		t.Fatalf("data disks %+v", sp.Extra)
	}
}

func TestImportProxmox(t *testing.T) {
	ctx := context.Background()
	svc, hv, d, _ := newSvc()
	dir := t.TempDir()
	disks := &fileDisks{fakeDisks: d, dir: dir}
	svc.Disks, svc.Jobs = disks, &jobs.Manager{}
	svc.ImportDir, svc.NVRAMDir = filepath.Join(dir, "import"), filepath.Join(dir, "nvram")

	// a Proxmox backup: 300 KiB disk with data, 4M efidisk
	data := make([]byte, 300<<10)
	for i := range data {
		if i%8192 < 4096 {
			data[i] = byte(i)
		}
	}
	efi := bytes.Repeat([]byte{7}, 540672)
	conf := "bios: ovmf\ncores: 2\nmemory: 3072\nname: shop\nnet0: virtio=BC:24:11:00:00:01,bridge=vmbr0\nostype: l26\nscsi0: local-lvm:vm-1-disk-0,size=1G\nscsihw: virtio-scsi-pci\n"
	raw := imports.BuildVMA(map[string]string{"qemu-server.conf": conf}, []string{"drive-scsi0", "drive-efidisk0"},
		map[string][]byte{"drive-scsi0": data, "drive-efidisk0": efi})
	if _, err := svc.SaveImport("vzdump-qemu-101-2026_10_13-12_00_00.vma", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveImport("../evil.vma", bytes.NewReader(raw)); err == nil {
		t.Fatal("path escape accepted")
	}
	if l, _ := svc.ListImports(); len(l) != 1 || l[0].Kind != imports.KindVMA {
		t.Fatalf("list %+v", l)
	}
	pl, err := svc.ImportPlan("vzdump-qemu-101-2026_10_13-12_00_00.vma")
	if err != nil || pl.Name != "shop" || pl.Firmware != "uefi" || pl.DiskBus != "scsi" || !pl.EFIVars {
		t.Fatalf("plan %+v %v", pl, err)
	}
	if _, err := svc.StartImport(ctx, ImportRequest{File: "vzdump-qemu-101-2026_10_13-12_00_00.vma", OSType: "beos"}); err == nil {
		t.Fatal("bad os accepted")
	}
	j, err := svc.StartImport(ctx, ImportRequest{File: "vzdump-qemu-101-2026_10_13-12_00_00.vma", KeepMAC: true, VCPUs: 4})
	if err != nil {
		t.Fatal(err)
	}
	if j = waitJob(t, svc, j); j.State != "done" {
		t.Fatalf("job %+v", j)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "p8s-shop-disk0"))
	if !bytes.Equal(got[:len(data)], data) {
		t.Fatal("disk data differs")
	}
	if nv, _ := os.ReadFile(filepath.Join(dir, "nvram", "p8s-shop_VARS.fd")); !bytes.Equal(nv, efi) {
		t.Fatal("uefi variables not carried over")
	}
	sp, _ := virt.ParseDomainXML(hv.doms["p8s-shop"])
	if sp.VCPUs != 4 || sp.MemoryMiB != 3072 || sp.DiskBus != "scsi" || sp.Firmware != "uefi" || sp.MAC != "bc:24:11:00:00:01" ||
		!strings.HasPrefix(sp.Template, "import:") {
		t.Fatalf("spec %+v", sp)
	}
	if st, _ := hv.State(ctx, "p8s-shop"); st != virt.Running {
		t.Fatalf("state %v", st)
	}
	// same name again: refused
	if _, err := svc.StartImport(ctx, ImportRequest{File: "vzdump-qemu-101-2026_10_13-12_00_00.vma"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if err := svc.DeleteImport("vzdump-qemu-101-2026_10_13-12_00_00.vma"); err != nil {
		t.Fatal(err)
	}
}

func TestImportDiskImage(t *testing.T) {
	if _, err := os.Stat("/usr/bin/qemu-img"); err != nil {
		t.Skip("no qemu-img")
	}
	ctx := context.Background()
	svc, hv, d, _ := newSvc()
	dir := t.TempDir()
	svc.Disks, svc.Jobs = &fileDisks{fakeDisks: d, dir: dir}, &jobs.Manager{}
	svc.ImportDir = filepath.Join(dir, "import")
	os.MkdirAll(svc.ImportDir, 0o755)
	data := bytes.Repeat([]byte("p8s!"), 64<<10) // 256 KiB
	img := filepath.Join(svc.ImportDir, "Old Server.img")
	os.WriteFile(img, data, 0o644)
	pl, err := svc.ImportPlan("Old Server.img")
	if err != nil || pl.Kind != imports.KindDisk || pl.Name != "old-server" || pl.DiskGB != 1 {
		t.Fatalf("plan %+v %v", pl, err)
	}
	j, err := svc.StartImport(ctx, ImportRequest{File: "Old Server.img", OSType: "windows", DiskBus: "sata", NICModel: "e1000e", Start: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	if j = waitJob(t, svc, j); j.State != "done" {
		t.Fatalf("job %+v", j)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "p8s-old-server-disk0"))
	if !bytes.Equal(got[:len(data)], data) {
		t.Fatal("disk differs")
	}
	if st, _ := hv.State(ctx, "p8s-old-server"); st != virt.Stopped {
		t.Fatalf("should stay stopped: %v", st)
	}
}
