package imports

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeVMA(t *testing.T, configs map[string]string, names []string, disks map[string][]byte) []byte {
	t.Helper()
	return BuildVMA(configs, names, disks)
}

type memDisk struct{ b []byte }

func (m *memDisk) WriteAt(p []byte, off int64) (int, error) {
	if int(off)+len(p) > len(m.b) {
		m.b = append(m.b, make([]byte, int(off)+len(p)-len(m.b))...)
	}
	return copy(m.b[off:], p), nil
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		if (i/vmaBlock)%3 != 0 { // every third block zero
			b[i] = seed + byte(i*7)
		}
	}
	return b
}

func TestVMA(t *testing.T) {
	scsi := pattern(5*vmaCluster+12345, 1) // not cluster aligned
	efi := pattern(540672, 9)
	conf := "bios: ovmf\nboot: order=scsi0;net0\ncores: 2\nmemory: 4096\nname: web\nnet0: virtio=BC:24:11:AA:BB:CC,bridge=vmbr0\nostype: l26\nscsi0: local-lvm:vm-100-disk-0,size=1M\nscsihw: virtio-scsi-single\n"
	raw := writeVMA(t, map[string]string{"qemu-server.conf": conf}, []string{"drive-scsi0", "drive-efidisk0"},
		map[string][]byte{"drive-scsi0": scsi, "drive-efidisk0": efi})
	v, err := OpenVMA(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Configs["qemu-server.conf"]) != conf || len(v.Devices) != 2 || v.Devices[0].Name != "drive-scsi0" || v.Devices[0].Size != int64(len(scsi)) {
		t.Fatalf("header %+v", v.Devices)
	}
	d1, d2 := &memDisk{make([]byte, len(scsi))}, &memDisk{make([]byte, len(efi))}
	var last int64
	if err := v.Extract(map[int]io.WriterAt{1: d1, 2: d2}, func(n int64) { last = n }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d1.b, scsi) || !bytes.Equal(d2.b, efi) || last == 0 {
		t.Fatal("extracted data differs")
	}

	// damage: checksum and truncation are noticed
	bad := bytes.Clone(raw)
	bad[100] ^= 1
	if _, err := OpenVMA(bytes.NewReader(bad)); err == nil {
		t.Fatal("bad header accepted")
	}
	v, _ = OpenVMA(bytes.NewReader(raw[:len(raw)-100]))
	if err := v.Extract(nil, nil); err == nil {
		t.Fatal("cut-off file accepted")
	}
	if _, err := OpenVMA(bytes.NewReader([]byte("hello world, not a vma"))); err == nil {
		t.Fatal("not a vma")
	}
	// the file helpers find it by name
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "vzdump-qemu-100-2026_10_13-10_00_00.vma"), raw, 0o644)
	p, err := Inspect(filepath.Join(dir, "vzdump-qemu-100-2026_10_13-10_00_00.vma"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindVMA || p.Name != "web" || p.VCPUs != 2 || p.MemoryMiB != 4096 || p.DiskBus != "scsi" || p.Firmware != "uefi" ||
		p.MAC != "bc:24:11:aa:bb:cc" || p.NICModel != "virtio" || p.BootDevice != "drive-scsi0" || p.DiskGB != 1 || !p.EFIVars {
		t.Fatalf("plan %+v", p)
	}
}
