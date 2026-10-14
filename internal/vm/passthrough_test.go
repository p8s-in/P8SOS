package vm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"p8sos/internal/hostdevs"
	"p8sos/internal/virt"
)

type fakeDevs struct{ in hostdevs.Info }

func (f *fakeDevs) List(context.Context) hostdevs.Info { return f.in }

type plugHV struct {
	*fakeHV
	attached, detached []string
}

func (h *plugHV) AttachDisk(_ context.Context, _ string, x []byte) error {
	h.attached = append(h.attached, string(x))
	return nil
}
func (h *plugHV) DetachDevice(_ context.Context, _ string, x []byte) error {
	h.detached = append(h.detached, string(x))
	return nil
}

func TestPassthrough(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	ah := &plugHV{fakeHV: hv}
	svc.HV = ah
	devs := &fakeDevs{hostdevs.Info{IOMMU: true, PCI: []hostdevs.PCI{
		{Addr: "0000:00:00.0", Bridge: true, Name: "Host bridge"},
		{Addr: "0000:01:00.0", Name: "NVIDIA GTX 1650", Kind: "graphics", Group: 2, Others: []string{"0000:01:00.1"}},
		{Addr: "0000:01:00.1", Name: "NVIDIA audio", Kind: "audio", Group: 2, Others: []string{"0000:01:00.0"}},
	}, USB: []hostdevs.USB{{ID: "046d:c52b", Name: "Logitech USB Receiver", Port: "1-2"}}}}
	if _, _, err := svc.AddPCI(ctx, "web1", "01:00.0"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no lister: %v", err)
	}
	svc.HostDevs = devs
	for _, n := range []string{"web1", "web2"} {
		if _, err := svc.Create(ctx, CreateRequest{Name: n, Template: "debian-13", DiskGB: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.AddPCI(ctx, "web1", "01:00.0"); !errors.Is(err, ErrConflict) {
		t.Fatalf("running: %v", err)
	}
	_, _ = svc.Stop(ctx, "web1", true)
	if _, _, err := svc.AddPCI(ctx, "web1", "00:00.0"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bridge: %v", err)
	}
	if _, _, err := svc.AddPCI(ctx, "web1", "05:00.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	devs.in.IOMMU = false
	if _, _, err := svc.AddPCI(ctx, "web1", "01:00.0"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "IOMMU") {
		t.Fatalf("iommu off: %v", err)
	}
	devs.in.IOMMU = true
	v, note, err := svc.AddPCI(ctx, "web1", "01:00.0")
	if err != nil || strings.Join(v.PCI, ",") != "0000:01:00.0,0000:01:00.1" || !strings.Contains(note, "partners") {
		t.Fatalf("add: %v %+v %q", err, v.PCI, note)
	}
	if _, _, err := svc.AddPCI(ctx, "web1", "0000:01:00.0"); !errors.Is(err, ErrExists) {
		t.Fatalf("twice: %v", err)
	}
	_, _ = svc.Stop(ctx, "web2", true)
	if _, _, err := svc.AddPCI(ctx, "web2", "0000:01:00.1"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "web1") {
		t.Fatalf("used by another VM: %v", err)
	}
	// moves and live moves refuse; clones don't take the devices
	if sp, _ := svc.spec(ctx, "p8s-web1"); !strings.Contains(hostDevsNote(sp), "PCI/USB") {
		t.Fatalf("move note: %q", hostDevsNote(sp))
	}
	d, err := svc.Devices(ctx, "web2")
	if err != nil || d.UsedBy["0000:01:00.0"] != "web1" || len(d.Available.PCI) != 3 {
		t.Fatalf("devices: %v %+v", err, d)
	}
	if v, err = svc.RemovePCI(ctx, "web1", "0000:01:00.1"); err != nil || len(v.PCI) != 1 {
		t.Fatalf("remove: %v %+v", err, v.PCI)
	}
	// USB: live plug into a running VM
	_, _ = svc.Start(ctx, "web2")
	v, note, err = svc.AddUSB(ctx, "web2", "046D:C52B")
	if err != nil || len(v.USB) != 1 || v.USB[0] != "046d:c52b" || len(ah.attached) != 1 || !strings.Contains(note, "in the VM now") {
		t.Fatalf("usb: %v %+v %q %v", err, v.USB, note, ah.attached)
	}
	if _, _, err := svc.AddUSB(ctx, "web1", "046d:c52b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("usb used: %v", err)
	}
	if _, note, err := svc.AddUSB(ctx, "web1", "1234:5678"); err != nil || !strings.Contains(note, "isn't plugged in") {
		t.Fatalf("not plugged: %v %q", err, note)
	}
	if v, err = svc.RemoveUSB(ctx, "web2", "046d:c52b"); err != nil || len(v.USB) != 0 || len(ah.detached) != 1 {
		t.Fatalf("usb remove: %v %+v", err, v)
	}
	// the XML keeps them across other changes (redefine)
	sp, _ := svc.spec(ctx, "p8s-web1")
	if len(sp.PCI) != 1 || len(sp.USB) != 1 || sp.USB[0] != (virt.USBDev{Vendor: "1234", Product: "5678"}) {
		t.Fatalf("spec %+v %+v", sp.PCI, sp.USB)
	}
}

type tuneHV struct {
	*fakeHV
	disks, nics []string
}

func (h *tuneHV) SetDiskLimits(_ context.Context, _, target string, l virt.IOLimits) error {
	h.disks = append(h.disks, target)
	return nil
}
func (h *tuneHV) SetNICLimits(_ context.Context, _, mac string, l virt.IOLimits) error {
	h.nics = append(h.nics, mac)
	return nil
}

func TestIOLimits(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	th := &tuneHV{fakeHV: hv}
	svc.HV = th
	if _, err := svc.Create(ctx, CreateRequest{Name: "db1", Template: "debian-13", DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetIOLimits(ctx, "db1", virt.IOLimits{ReadMBs: 2}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too low: %v", err)
	}
	v, err := svc.SetIOLimits(ctx, "db1", virt.IOLimits{ReadMBs: 200, WriteMBs: 100, IOPS: 3000, NetDownMbit: 100})
	if err != nil || v.IOLimits == nil || v.IOLimits.WriteMBs != 100 || len(th.disks) != 1 || th.disks[0] != "vda" || len(th.nics) != 1 {
		t.Fatalf("set: %v %+v %v %v", err, v.IOLimits, th.disks, th.nics)
	}
	_, _ = svc.Stop(ctx, "db1", true)
	if v, err = svc.SetIOLimits(ctx, "db1", virt.IOLimits{}); err != nil || v.IOLimits != nil || len(th.disks) != 1 {
		t.Fatalf("clear while stopped: %v %+v", err, v.IOLimits)
	}
}

type hotHV struct {
	*fakeHV
	vcpus []int
	mems  []int
}

func (h *hotHV) SetVCPUs(_ context.Context, _ string, n int) error { h.vcpus = append(h.vcpus, n); return nil }
func (h *hotHV) AddMemory(_ context.Context, _ string, mib int) error {
	h.mems = append(h.mems, mib)
	return nil
}

func TestHotplug(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	hh := &hotHV{fakeHV: hv}
	svc.HV = hh
	if _, err := svc.Create(ctx, CreateRequest{Name: "app", Template: "debian-13", DiskGB: 10, VCPUs: 2, MemoryMiB: 2048}); err != nil {
		t.Fatal(err)
	}
	// off: a running VM can't be resized
	if _, err := svc.Resize(ctx, "app", ResizeRequest{VCPUs: 3}); !errors.Is(err, ErrConflict) {
		t.Fatalf("no hotplug: %v", err)
	}
	if _, err := svc.SetHotplug(ctx, "app", true, 0, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("running: %v", err)
	}
	_, _ = svc.Stop(ctx, "app", true)
	v, err := svc.SetHotplug(ctx, "app", true, 0, 0)
	if err != nil || v.HotCPUs != 4 || v.HotMemMiB != 4096 || v.VCPUs != 2 {
		t.Fatalf("on: %v %+v", err, v)
	}
	sp, _ := svc.spec(ctx, "p8s-app")
	x, _ := virt.DomainXML(sp)
	if !strings.Contains(string(x), `current="2"`) || !strings.Contains(string(x), `<maxMemory slots="16" unit="MiB">4096</maxMemory>`) || !strings.Contains(string(x), `cpus="0-3"`) {
		t.Fatalf("xml:\n%s", x)
	}
	_, _ = svc.Start(ctx, "app")
	v, err = svc.Resize(ctx, "app", ResizeRequest{VCPUs: 4, MemoryMiB: 3000})
	if err != nil || v.VCPUs != 4 || v.MemoryMiB != 3072 || len(hh.vcpus) != 1 || hh.mems[0] != 1024 {
		t.Fatalf("hot resize: %v %+v %v %v", err, v, hh.vcpus, hh.mems)
	}
	if _, err := svc.Resize(ctx, "app", ResizeRequest{VCPUs: 5}); !errors.Is(err, ErrConflict) {
		t.Fatalf("above ceiling: %v", err)
	}
	if _, err := svc.Resize(ctx, "app", ResizeRequest{MemoryMiB: 2048}); !errors.Is(err, ErrConflict) {
		t.Fatalf("less RAM live: %v", err)
	}
	if _, err := svc.Resize(ctx, "app", ResizeRequest{DiskGB: 20}); !errors.Is(err, ErrConflict) {
		t.Fatalf("disk live: %v", err)
	}
	// stopped resize above the ceiling lifts the ceiling
	_, _ = svc.Stop(ctx, "app", true)
	if v, err = svc.Resize(ctx, "app", ResizeRequest{VCPUs: 6}); err != nil || v.HotCPUs != 6 {
		t.Fatalf("cold resize: %v %+v", err, v)
	}
	if v, err = svc.SetHotplug(ctx, "app", false, 0, 0); err != nil || v.HotCPUs != 0 || v.HotMemMiB != 0 {
		t.Fatalf("off: %v %+v", err, v)
	}
}
