package imports

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanWindows(t *testing.T) {
	conf := `bootdisk: ide0
cores: 2
sockets: 2
memory: 8192
name: Win Server 2019
net0: e1000=AA:BB:CC:DD:EE:FF,bridge=vmbr0,tag=20
ostype: win10
ide0: local:100/vm-100-disk-0.qcow2,size=40G
ide2: local:iso/virtio.iso,media=cdrom
tpmstate0: local-lvm:vm-100-disk-2,size=4M,version=v2.0
virtio1: local-lvm:vm-100-disk-1,size=100G

[snap1]
cores: 1
`
	v := &VMA{Configs: map[string][]byte{"qemu-server.conf": []byte(conf)}, Devices: []Device{
		{1, "drive-ide0", 40 << 30}, {2, "drive-tpmstate0", 4 << 20}, {3, "drive-virtio1", 100 << 30}}}
	p := PlanFromVMA(v)
	w := strings.Join(p.Warnings, " | ")
	if p.Name != "win-server-2019" || p.VCPUs != 4 || p.MemoryMiB != 8192 || p.OSType != "windows" || p.Firmware != "bios" ||
		p.DiskBus != "sata" || p.BootDevice != "drive-ide0" || p.DiskGB != 40 || p.NICModel != "e1000" ||
		!strings.Contains(w, "IDE") || !strings.Contains(w, "TPM") || !strings.Contains(w, "virtio1") || !strings.Contains(w, "VLAN") {
		t.Fatalf("%+v\n%s", p, w)
	}
	if ParsePVEConfig(conf)["cores"] != "2" {
		t.Fatal("snapshot section leaked")
	}
	if cleanName("100 test") != "vm-100-test" {
		t.Fatal(cleanName("100 test"))
	}
}

func TestInspectDisk(t *testing.T) {
	old := QemuImg
	defer func() { QemuImg = old }()
	QemuImg = func(_ context.Context, args ...string) ([]byte, error) {
		return []byte(`{"format":"vmdk","virtual-size":21474836480}`), nil
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "Ubuntu Server.vmdk")
	os.WriteFile(f, []byte("x"), 0o644)
	p, err := Inspect(f)
	if err != nil || p.Kind != KindDisk || p.DiskGB != 20 || p.Name != "ubuntu-server" || p.Format != "vmdk" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := Inspect(filepath.Join(dir, "notes.txt")); err == nil {
		t.Fatal("txt accepted")
	}
	if KindOf("vzdump-qemu-101.vma.zst") != KindVMA || KindOf("a.QCOW2") != KindDisk || KindOf("a.iso") != "" {
		t.Fatal("kinds")
	}
}

func TestCompressedVMA(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("no zstd")
	}
	raw := writeVMA(t, map[string]string{"qemu-server.conf": "name: z\nscsi0: x,size=1M\n"}, []string{"drive-scsi0"},
		map[string][]byte{"drive-scsi0": pattern(3*vmaCluster, 4)})
	dir := t.TempDir()
	plain := filepath.Join(dir, "b.vma")
	os.WriteFile(plain, raw, 0o644)
	if out, err := exec.Command("zstd", "-q", plain).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	p, err := Inspect(plain + ".zst")
	if err != nil || p.Name != "z" || p.BootDevice != "drive-scsi0" {
		t.Fatalf("%+v %v", p, err)
	}
}
