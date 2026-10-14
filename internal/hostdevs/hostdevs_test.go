package hostdevs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"p8sos/internal/testfake"
)

func mk(t *testing.T, root, rel, data string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pci(t *testing.T, root, addr, class, ven, dev string, group int, driver string) {
	d := filepath.Join("sys/bus/pci/devices", addr)
	mk(t, root, filepath.Join(d, "class"), "0x"+class+"00\n")
	mk(t, root, filepath.Join(d, "vendor"), "0x"+ven+"\n")
	mk(t, root, filepath.Join(d, "device"), "0x"+dev+"\n")
	g := filepath.Join(root, "sys/kernel/iommu_groups", itoa(group))
	_ = os.MkdirAll(g, 0o755)
	_ = os.Symlink(g, filepath.Join(root, d, "iommu_group"))
	if driver != "" {
		dd := filepath.Join(root, "sys/bus/pci/drivers", driver)
		_ = os.MkdirAll(dd, 0o755)
		_ = os.Symlink(dd, filepath.Join(root, d, "driver"))
	}
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestList(t *testing.T) {
	root := t.TempDir()
	pci(t, root, "0000:00:00.0", "0600", "8086", "3e10", 0, "")             // host bridge
	pci(t, root, "0000:00:02.0", "0300", "8086", "3e9b", 1, "i915")         // laptop's own screen
	mk(t, root, "sys/bus/pci/devices/0000:00:02.0/boot_vga", "1")
	pci(t, root, "0000:01:00.0", "0300", "10de", "1f91", 2, "nouveau")      // GPU
	pci(t, root, "0000:01:00.1", "0403", "10de", "10fa", 2, "snd_hda_intel") // its audio
	pci(t, root, "0000:03:00.0", "0200", "10ec", "8168", 3, "r8169")        // wired NIC
	mk(t, root, "sys/bus/pci/devices/0000:03:00.0/net/enp3s0/x", "")
	mk(t, root, "proc/cpuinfo", "vendor_id\t: GenuineIntel\n")
	mk(t, root, "sys/bus/usb/devices/usb1/idVendor", "1d6b")       // root hub
	mk(t, root, "sys/bus/usb/devices/usb1/idProduct", "0002")
	mk(t, root, "sys/bus/usb/devices/1-2/idVendor", "046d")
	mk(t, root, "sys/bus/usb/devices/1-2/idProduct", "c52b")
	mk(t, root, "sys/bus/usb/devices/1-2/manufacturer", "Logitech")
	mk(t, root, "sys/bus/usb/devices/1-2/product", "USB Receiver")
	mk(t, root, "sys/bus/usb/devices/1-2:1.0/idVendor", "046d")    // an interface: skipped
	run := &testfake.Runner{Out: map[string]string{"lspci -Dmm": `0000:01:00.0 "VGA compatible controller" "NVIDIA Corporation" "TU117M [GeForce GTX 1650 Mobile]" -ra1 "ASUSTeK" "x"` + "\n"}}
	in := (&Lister{Root: root, Run: run}).List(context.Background())
	if !in.IOMMU || in.CPU != "intel" || len(in.PCI) != 5 || len(in.USB) != 1 {
		t.Fatalf("info %+v", in)
	}
	by := map[string]PCI{}
	for _, p := range in.PCI {
		by[p.Addr] = p
	}
	if g := by["0000:01:00.0"]; g.Kind != "graphics" || g.Name != "NVIDIA Corporation TU117M [GeForce GTX 1650 Mobile]" || len(g.Others) != 1 || g.Others[0] != "0000:01:00.1" || g.Warn != "" || g.Driver != "nouveau" {
		t.Fatalf("gpu %+v", g)
	}
	if !by["0000:00:00.0"].Bridge || by["0000:00:02.0"].Warn == "" || by["0000:03:00.0"].Warn == "" {
		t.Fatalf("warnings %+v", in.PCI)
	}
	if u := in.USB[0]; u.ID != "046d:c52b" || u.Name != "Logitech USB Receiver" || u.Port != "1-2" {
		t.Fatalf("usb %+v", u)
	}
	// IOMMU off: no groups
	root2 := t.TempDir()
	mk(t, root2, "sys/bus/pci/devices/0000:01:00.0/class", "0x030000")
	if in := (&Lister{Root: root2}).List(context.Background()); in.IOMMU || in.PCI[0].Group != -1 {
		t.Fatalf("no iommu %+v", in)
	}
}
