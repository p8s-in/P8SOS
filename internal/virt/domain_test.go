package virt

import (
	"regexp"
	"strings"
	"testing"
)

func TestDomainXMLRoundTrip(t *testing.T) {
	in := Spec{
		Name: "p8s-web1", UUID: "4b1c2d3e-0000-4000-8000-000000000001", VCPUs: 2, MemoryMiB: 2048,
		DiskGB: 20, DiskPath: "/dev/zvol/tank/vms/p8s-web1-disk0", SeedISO: "/var/lib/p8s/seeds/p8s-web1-seed.iso",
		Bridge: "vmbr0", MAC: "52:54:00:aa:bb:cc", Template: "debian-13", CreatedAt: "2026-10-10T12:00:00Z", Video: VideoModel, DefVersion: DefVersion,
	}
	b, err := DomainXML(in)
	if err != nil {
		t.Fatal(err)
	}
	x := string(b)
	for _, want := range []string{
		`<domain type="kvm">`, `machine="q35"`, `<cpu mode="host-passthrough">`,
		`<source dev="/dev/zvol/tank/vms/p8s-web1-disk0">`, `<target dev="vda" bus="virtio">`,
		`<source bridge="vmbr0">`, `listen="127.0.0.1"`, `xmlns="https://p8s.dev/xmlns/vm/1"`,
		`org.qemu.guest_agent.0`,
		`<log file="/var/log/libvirt/qemu/p8s-web1-console.log" append="off">`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("xml missing %s\n%s", want, x)
		}
	}
	out, err := ParseDomainXML(b)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip mismatch\n in=%+v\nout=%+v", in, out)
	}
}

// libvirt rewrites our metadata with a prefix; parsing must still work.
func TestParseLibvirtStyleMetadata(t *testing.T) {
	x := `<domain type='kvm'><name>p8s-a1</name><metadata>
<p8s:vm xmlns:p8s="https://p8s.dev/xmlns/vm/1"><p8s:template>debian-13</p8s:template><p8s:disk_gb>32</p8s:disk_gb></p8s:vm>
</metadata><memory unit='KiB'>1048576</memory><vcpu placement='static'>1</vcpu></domain>`
	s, err := ParseDomainXML([]byte(x))
	if err != nil {
		t.Fatal(err)
	}
	if s.Template != "debian-13" || s.DiskGB != 32 || s.MemoryMiB != 1024 || s.VCPUs != 1 {
		t.Fatalf("got %+v", s)
	}
}

func TestRandomMAC(t *testing.T) {
	if m := RandomMAC(); !regexp.MustCompile(`^52:54:00(:[0-9a-f]{2}){3}$`).MatchString(m) {
		t.Fatal(m)
	}
}

func TestParseState(t *testing.T) {
	for in, want := range map[string]State{"running\n": Running, "shut off": Stopped, "in shutdown": Stopping, "x": Unknown} {
		if got := ParseState(in); got != want {
			t.Errorf("%q: %s != %s", in, got, want)
		}
	}
}

func TestParseIfAddr(t *testing.T) {
	out := ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 vnet0      52:54:00:3a:91:c4    ipv4         192.168.122.57/24
 vnet0      52:54:00:3a:91:c4    ipv6         fe80::5054:ff:fe3a:91c4/64
 lo         00:00:00:00:00:00    ipv4         127.0.0.1/8
`
	got := ParseIfAddr(out)
	if len(got) != 1 || got[0] != "192.168.122.57" {
		t.Fatalf("got %v", got)
	}
	if ParseIfAddr("") != nil {
		t.Fatal("empty output should give no IPs")
	}
}

func TestDomainXMLInstallISO(t *testing.T) {
	for _, os := range []string{"linux", "windows"} {
		in := Spec{Name: "p8s-win1", UUID: "4b1c2d3e-0000-4000-8000-000000000002", VCPUs: 2, MemoryMiB: 4096, DiskGB: 64,
			DiskPath: "/dev/zvol/tank/vms/p8s-win1-disk0", InstallISO: "/var/lib/p8s/isos/install.iso", OSType: os,
			Bridge: "virbr0", MAC: "52:54:00:aa:bb:dd", Template: "iso:install.iso", CreatedAt: "2026-10-12T12:00:00Z", Video: VideoModel, DefVersion: DefVersion}
		b, err := DomainXML(in)
		if err != nil {
			t.Fatal(err)
		}
		x := string(b)
		want := []string{`<boot dev="hd"></boot>`, `<boot dev="cdrom"></boot>`, `<source file="/var/lib/p8s/isos/install.iso">`,
			`<target dev="sdb" bus="sata">`, `<input type="tablet" bus="usb">`, `<model type="vga" vram="16384" heads="1" primary="yes">`}
		if os == "windows" {
			want = append(want, `<target dev="sdc" bus="sata">`, `<model type="e1000e">`, `<hyperv>`, `<clock offset="localtime">`)
		} else {
			want = append(want, `<target dev="vda" bus="virtio">`, `<model type="virtio">`)
		}
		for _, w := range want {
			if !strings.Contains(x, w) {
				t.Errorf("%s: missing %s\n%s", os, w, x)
			}
		}
		out, err := ParseDomainXML(b)
		if err != nil || out != in {
			t.Fatalf("%s round trip\n in=%+v\nout=%+v (%v)", os, in, out, err)
		}
	}
}

func TestOwnerRoundTrip(t *testing.T) {
	x, err := DomainXML(Spec{Name: "p8s-a", UUID: "u", VCPUs: 1, MemoryMiB: 512, DiskGB: 4, DiskPath: "/dev/zvol/x", Bridge: "virbr0",
		MAC: "52:54:00:00:00:01", Template: "debian-13", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ParseDomainXML(x)
	if err != nil || sp.Owner != "alice" {
		t.Fatalf("owner %q (%v)", sp.Owner, err)
	}
	m, err := MetadataXML(sp)
	if err != nil || !strings.Contains(m, "<owner>alice</owner>") || !strings.HasPrefix(m, "<vm>") {
		t.Fatalf("metadata %s (%v)", m, err)
	}
	// libvirt hands metadata set with `virsh metadata --key p8s` back prefixed
	live := strings.Replace(string(x), `<vm xmlns="https://p8s.dev/xmlns/vm/1">`, `<p8s:vm xmlns:p8s="https://p8s.dev/xmlns/vm/1">`, 1)
	live = strings.Replace(live, "</vm>", "</p8s:vm>", 1)
	if sp, err := ParseDomainXML([]byte(live)); err != nil || sp.Owner != "alice" || sp.Template != "debian-13" {
		t.Fatalf("prefixed metadata: owner %q template %q (%v)", sp.Owner, sp.Template, err)
	}
}

func TestWindows11Domain(t *testing.T) {
	s := Spec{Name: "p8s-win", VCPUs: 4, MemoryMiB: 8192, DiskGB: 80, DiskPath: "/dev/zvol/tank/p8s/vms/p8s-win-disk0",
		InstallISO: "/var/lib/p8s/isos/Win11.iso", DriversISO: "/var/lib/p8s/isos/virtio-win.iso", OSType: "windows11",
		Bridge: "virbr0", MAC: "52:54:00:aa:bb:cc", Template: "iso:Win11.iso"}
	x, err := DomainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<os firmware="efi">`, `<feature enabled="yes" name="secure-boot"></feature>`, `<feature enabled="yes" name="enrolled-keys"></feature>`,
		`<loader secure="yes"></loader>`, `<smm state="on"></smm>`, `<tpm model="tpm-crb">`, `<backend type="emulator" version="2.0"></backend>`,
		`<synic state="on"></synic>`, `<timer name="hypervclock" present="yes"></timer>`, `<target dev="sdd" bus="sata">`, `bus="sata"`} {
		if !strings.Contains(string(x), want) {
			t.Fatalf("missing %s in\n%s", want, x)
		}
	}
	back, err := ParseDomainXML(x)
	if err != nil || back.OSType != "windows11" || back.DriversISO != s.DriversISO || back.InstallISO != s.InstallISO {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	// plain Windows stays on BIOS without a TPM; Linux gets neither
	for _, os := range []string{"windows", "linux"} {
		s.OSType = os
		x, _ = DomainXML(s)
		if strings.Contains(string(x), "efi") || strings.Contains(string(x), "<tpm") {
			t.Fatalf("%s got UEFI/TPM:\n%s", os, x)
		}
	}
}

func TestFastIO(t *testing.T) {
	s := Spec{Name: "p8s-fast", VCPUs: 4, MemoryMiB: 4096, DiskGB: 20, DiskPath: "/dev/zvol/tank/p8s/vms/p8s-fast-disk0",
		Bridge: "virbr0", MAC: "52:54:00:aa:bb:01", Template: "debian-13", OSType: "linux"}
	x, err := DomainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<iothreads>1</iothreads>`, `io="native"`, `iothread="1"`, `queues="4"`, `<driver name="vhost" queues="4">`} {
		if !strings.Contains(string(x), want) {
			t.Fatalf("linux missing %s in\n%s", want, x)
		}
	}
	s.OSType = "windows"
	x, _ = DomainXML(s)
	for _, bad := range []string{"iothread", "queues="} {
		if strings.Contains(string(x), bad) {
			t.Fatalf("windows got %s:\n%s", bad, x)
		}
	}
	// one socket with 4 cores (Windows Home/Pro only use 1-2 sockets)
	if !strings.Contains(string(x), `<topology sockets="1" dies="1" cores="4" threads="1"></topology>`) || !strings.Contains(string(x), `io="native"`) {
		t.Fatalf("windows cpu/disk:\n%s", x)
	}
}

func TestBalloon(t *testing.T) {
	b, err := DomainXML(Spec{Name: "b", VCPUs: 1, MemoryMiB: 512, DiskPath: "/dev/zvol/x", Bridge: "virbr0", MAC: "52:54:00:00:00:01"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `<memballoon model="virtio" autodeflate="on" freePageReporting="on">`) || !strings.Contains(string(b), `<stats period="10"></stats>`) {
		t.Fatal(string(b))
	}
}

func TestImportOverrides(t *testing.T) {
	s := Spec{Name: "imp", VCPUs: 2, MemoryMiB: 2048, DiskGB: 32, DiskPath: "/dev/zvol/t/imp-disk0", Bridge: "virbr0",
		MAC: "52:54:00:00:00:09", OSType: "windows", DiskBus: "scsi", NICModel: "vmxnet3", Firmware: "uefi"}
	x, err := DomainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<controller type="scsi" model="virtio-scsi">`, `bus="scsi"`, `<model type="vmxnet3">`, `<os firmware="efi">`} {
		if !strings.Contains(string(x), want) {
			t.Fatalf("missing %s:\n%s", want, x)
		}
	}
	if strings.Contains(string(x), "secure-boot") {
		t.Fatal("plain UEFI got secure boot")
	}
	back, err := ParseDomainXML(x)
	if err != nil || back.DiskBus != "scsi" || back.NICModel != "vmxnet3" || back.Firmware != "uefi" {
		t.Fatalf("round trip %+v %v", back, err)
	}
	// a Linux VM imported from a SATA disk keeps it
	s = Spec{Name: "l", VCPUs: 4, MemoryMiB: 1024, DiskPath: "/dev/x", Bridge: "b", MAC: "52:54:00:00:00:0a", DiskBus: "sata"}
	x, _ = DomainXML(s)
	if !strings.Contains(string(x), `bus="sata"`) || strings.Contains(string(x), "iothread=") {
		t.Fatalf("linux sata:\n%s", x)
	}
}
