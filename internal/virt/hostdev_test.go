package virt

import (
	"strings"
	"testing"
)

func TestHostdevRoundTrip(t *testing.T) {
	s := Spec{Name: "p8s-gpu", VCPUs: 2, MemoryMiB: 2048, DiskPath: "/dev/zvol/tank/vms/p8s-gpu-disk0", Bridge: "virbr0", MAC: "52:54:00:00:00:01",
		PCI: []string{"0000:01:00.0", "0000:01:00.1"}, USB: []USBDev{{"046d", "c52b"}}}
	x, err := DomainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<hostdev mode="subsystem" type="pci" managed="yes">`, `bus="0x01"`, `function="0x1"`, `<vendor id="0x046d">`, `<product id="0xc52b">`} {
		if !strings.Contains(string(x), want) {
			t.Errorf("missing %s in\n%s", want, x)
		}
	}
	back, err := ParseDomainXML(x)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(back.PCI, ",") != "0000:01:00.0,0000:01:00.1" || len(back.USB) != 1 || back.USB[0].ID() != "046d:c52b" {
		t.Fatalf("back %v %v", back.PCI, back.USB)
	}
	// libvirt writes short hex ("0x1", "0x0"): still read right
	got := Spec{}
	got.readHostdevs([]hostdevXML{{Type: "pci", Source: struct {
		Vendor  *hostdevID  `xml:"vendor"`
		Product *hostdevID  `xml:"product"`
		Address *pciAddrXML `xml:"address"`
	}{Address: &pciAddrXML{Domain: "0x0", Bus: "0x2", Slot: "0x0", Function: "0x0"}}}})
	if len(got.PCI) != 1 || got.PCI[0] != "0000:02:00.0" {
		t.Fatalf("short hex: %v", got.PCI)
	}
	if _, err := ParsePCI("01:00.0"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"1:0.0", "0000:01:00.9", "xx"} {
		if _, err := ParsePCI(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := ParseUSB("046D:C52B"); err != nil {
		t.Fatal(err)
	}
	if u, _ := USBXML(USBDev{"046d", "c52b"}); !strings.Contains(string(u), `<hostdev mode="subsystem" type="usb"`) {
		t.Fatalf("usb xml %s", u)
	}
}

func TestLimitsRoundTrip(t *testing.T) {
	s := Spec{Name: "p8s-db", VCPUs: 2, MemoryMiB: 2048, DiskPath: "/dev/zvol/t/p8s-db-disk0", Bridge: "virbr0", MAC: "52:54:00:00:00:02",
		Extra: []ExtraDisk{{Index: 1, Path: "/dev/zvol/t/p8s-db-disk1", SizeGB: 10}}, NICs: []ExtraNIC{{MAC: "52:54:00:00:00:03", Bridge: "virbr0"}},
		Limits: IOLimits{ReadMBs: 200, WriteMBs: 100, IOPS: 5000, NetDownMbit: 100, NetUpMbit: 50}}
	x, err := DomainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(x), "<write_bytes_sec>100000000</write_bytes_sec>"); n != 2 {
		t.Fatalf("both disks limited, got %d:\n%s", n, x)
	}
	if n := strings.Count(string(x), `<inbound average="12500">`); n != 2 || !strings.Contains(string(x), `<outbound average="6250">`) {
		t.Fatalf("both NICs limited:\n%s", x)
	}
	back, _ := ParseDomainXML(x)
	if back.Limits != s.Limits {
		t.Fatalf("back %+v", back.Limits)
	}
	if strings.Contains(string(mustXML(t, Spec{Name: "p8s-x", DiskPath: "/d", Bridge: "b", MAC: "m"})), "iotune") {
		t.Fatal("no limits: no iotune")
	}
	if (IOLimits{ReadMBs: 1}).Check() == nil || (IOLimits{IOPS: 500, NetUpMbit: 10}).Check() != nil {
		t.Fatal("check")
	}
	if got := DiskTargets(Spec{OSType: "windows", Extra: []ExtraDisk{{Index: 1}}}); strings.Join(got, ",") != "sdc,sdf" {
		t.Fatalf("targets %v", got)
	}
	if got := DiskTargets(Spec{}); strings.Join(got, ",") != "vda" {
		t.Fatalf("targets %v", got)
	}
}

func mustXML(t *testing.T, s Spec) []byte {
	x, err := DomainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	return x
}
