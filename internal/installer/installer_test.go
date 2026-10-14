package installer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lsblkJSON = `{"blockdevices":[
 {"name":"loop0","path":"/dev/loop0","type":"loop","size":"900000000","model":null,"serial":null,"tran":null,"rota":"0","rm":"0","fstype":"squashfs","label":null,"mountpoints":["/run/live/rootfs/filesystem.squashfs"]},
 {"name":"sda","path":"/dev/sda","type":"disk","size":1000204886016,"model":"ST1000LM035-1RK172","serial":"WL1A2B3C","tran":"sata","rota":true,"rm":false,"fstype":null,"label":null,"mountpoints":[null],
  "children":[{"name":"sda1","path":"/dev/sda1","type":"part","size":1000203837440,"fstype":"ntfs","label":"DATA","rota":true,"rm":false,"mountpoints":[null]}]},
 {"name":"sdb","path":"/dev/sdb","type":"disk","size":500107862016,"model":"Samsung SSD 860","serial":"S3Z9NB0K","tran":"sata","rota":false,"rm":false,"fstype":null,"label":null,"mountpoints":[null]},
 {"name":"sdc","path":"/dev/sdc","type":"disk","size":500107862016,"model":"Samsung SSD 860","serial":"S3Z9NB0L","tran":"sata","rota":false,"rm":false,"fstype":null,"label":null,"mountpoints":[null]},
 {"name":"sdd","path":"/dev/sdd","type":"disk","size":32015679488,"model":"Flash Drive","serial":"0123","tran":"usb","rota":false,"rm":true,"fstype":"iso9660","label":"P8SOS","mountpoints":[null],
  "children":[{"name":"sdd1","path":"/dev/sdd1","type":"part","size":2000000000,"fstype":"iso9660","label":"P8SOS","rota":false,"rm":true,"mountpoints":["/run/live/medium"]},
              {"name":"sdd2","path":"/dev/sdd2","type":"part","size":5000000,"fstype":"vfat","label":"EFI","rota":false,"rm":true,"mountpoints":[null]}]},
 {"name":"nvme0n1","path":"/dev/nvme0n1","type":"disk","size":256060514304,"model":"INTEL SSDPEKNW256G8","serial":"BTNH0001","tran":"nvme","rota":false,"rm":false,"fstype":null,"label":null,"mountpoints":[null],
  "children":[{"name":"nvme0n1p1","path":"/dev/nvme0n1p1","type":"part","size":256060000000,"fstype":"ext4","label":null,"rota":false,"rm":false,"mountpoints":["/mnt/old"]}]},
 {"name":"sde","path":"/dev/sde","type":"disk","size":8000000000,"model":"Tiny","serial":"T1","tran":"sata","rota":false,"rm":false,"fstype":null,"label":null,"mountpoints":[null]},
 {"name":"sr0","path":"/dev/sr0","type":"rom","size":1073741312,"model":"DVD","serial":null,"tran":"sata","rota":true,"rm":true,"fstype":null,"label":null,"mountpoints":[null]}
]}`

func testDisks(t *testing.T) []Disk {
	t.Helper()
	d, err := ParseLsblk([]byte(lsblkJSON), map[string]string{"/dev/sdb": "/dev/disk/by-id/ata-Samsung_SSD_860_S3Z9NB0K"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseLsblk(t *testing.T) {
	disks := testDisks(t)
	var names []string
	for _, d := range disks {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, " "); got != "nvme0n1 sda sdb sdc sdd sde" {
		t.Fatalf("disks = %q", got)
	}
	why := map[string]string{}
	for _, d := range disks {
		why[d.Name] = d.Why
	}
	if !strings.Contains(why["sdd"], "installer") || !strings.Contains(why["nvme0n1"], "/mnt/old") || !strings.Contains(why["sde"], "too small") {
		t.Fatalf("unusable disks not marked: %v", why)
	}
	sda, _ := Find(disks, "sda")
	if !sda.Usable() || sda.Empty() || sda.State() != "HAS DATA: 1 partition(s), ntfs DATA" || !sda.Rota {
		t.Fatalf("sda = %+v / %s", sda, sda.State())
	}
	sdb, ok := Find(disks, "/dev/disk/by-id/ata-Samsung_SSD_860_S3Z9NB0K")
	if !ok || !sdb.Empty() || sdb.State() != "empty" || sdb.Dev() != "/dev/disk/by-id/ata-Samsung_SSD_860_S3Z9NB0K" {
		t.Fatalf("sdb = %+v", sdb)
	}
	if !strings.Contains(sdb.Label(), "500.1 GB") || !strings.Contains(sdb.Label(), "S3Z9NB0K") || !strings.HasSuffix(sdb.Label(), "SSD") {
		t.Fatalf("label %q", sdb.Label())
	}
}

func TestPartNames(t *testing.T) {
	for _, c := range []struct {
		d    Disk
		want string
	}{
		{Disk{Path: "/dev/sda"}, "/dev/sda4"},
		{Disk{Path: "/dev/nvme0n1"}, "/dev/nvme0n1p4"},
		{Disk{Path: "/dev/mmcblk0"}, "/dev/mmcblk0p4"},
		{Disk{Path: "/dev/sda", ByID: "/dev/disk/by-id/ata-X_1"}, "/dev/disk/by-id/ata-X_1-part4"},
	} {
		if got := c.d.Part(4); got != c.want {
			t.Errorf("%+v: %s, want %s", c.d, got, c.want)
		}
	}
}

func TestByIDLinks(t *testing.T) {
	dir := t.TempDir()
	dev := t.TempDir()
	for _, n := range []string{"sda", "sda1", "nvme0n1"} {
		os.WriteFile(filepath.Join(dev, n), nil, 0o644)
	}
	link := func(name, target string) { os.Symlink(filepath.Join(dev, target), filepath.Join(dir, name)) }
	link("wwn-0x5000c500a1b2c3d4", "sda")
	link("ata-ST1000LM035_WL1A2B3C", "sda")
	link("ata-ST1000LM035_WL1A2B3C-part1", "sda1")
	link("nvme-eui.0025385b71b0aa11", "nvme0n1")
	link("nvme-INTEL_SSDPEKNW256G8_BTNH0001", "nvme0n1")
	m := ByIDLinks(dir)
	if m[filepath.Join(dev, "sda")] != filepath.Join(dir, "ata-ST1000LM035_WL1A2B3C") ||
		m[filepath.Join(dev, "nvme0n1")] != filepath.Join(dir, "nvme-INTEL_SSDPEKNW256G8_BTNH0001") {
		t.Fatalf("by-id = %v", m)
	}
	if _, ok := m[filepath.Join(dev, "sda1")]; ok {
		t.Fatal("partition links must be skipped")
	}
}

func TestLayouts(t *testing.T) {
	cases := map[Layout][2]int{Single: {1, 2}, Ext4: {1, 2}, Mirror: {2, 1}, RAID10: {4, 3}, RAIDZ1: {3, 2}, RAIDZ2: {4, 3}, RAIDZ3: {5, 4}}
	for l, c := range cases {
		if err := l.Check(c[0]); err != nil {
			t.Errorf("%s with %d disks: %v", l, c[0], err)
		}
		if err := l.Check(c[1]); err == nil {
			t.Errorf("%s with %d disks must fail", l, c[1])
		}
	}
	if RAID10.Check(5) == nil || RAID10.Check(6) != nil {
		t.Error("RAID 10 needs an even number")
	}
	if Layout("raid5").Check(3) == nil {
		t.Error("unknown layout accepted")
	}
	if got := Fits(4); len(got) != 4 { // mirror, raid10, raidz1, raidz2
		t.Errorf("Fits(4) = %v", got)
	}
	G := int64(1e9)
	if u := RAIDZ2.Usable([]int64{4 * G, 4 * G, 4 * G, 3 * G}); u != 6*G {
		t.Errorf("raidz2 usable %d", u)
	}
	if u := RAID10.Usable([]int64{2 * G, 2 * G, 2 * G, 2 * G}); u != 4*G {
		t.Errorf("raid10 usable %d", u)
	}
	if v := strings.Join(RAID10.vdevs([]string{"a", "b", "c", "d"}), " "); v != "mirror a b mirror c d" {
		t.Errorf("raid10 vdevs %q", v)
	}
	if v := strings.Join(RAIDZ1.vdevs([]string{"a", "b", "c"}), " "); v != "raidz1 a b c" {
		t.Errorf("raidz1 vdevs %q", v)
	}
	if v := strings.Join(Single.vdevs([]string{"a"}), " "); v != "a" {
		t.Errorf("single vdevs %q", v)
	}
}

func good() Answers {
	return Answers{Disks: []string{"sdb", "sdc"}, Layout: Mirror, Hostname: "P8S1.example.com", User: "admin",
		Password: "correct horse", Erase: true}
}

func TestAnswersCheck(t *testing.T) {
	a := good()
	if err := a.Check(nil); err != nil {
		t.Fatal(err)
	}
	if a.Hostname != "p8s1.example.com" || a.Short() != "p8s1" || a.Timezone != "Etc/UTC" || a.Keyboard != "us" ||
		a.Network.Interface != "auto" || a.Network.Mode != "dhcp" {
		t.Fatalf("defaults: %+v", a)
	}
	bad := []func(*Answers){
		func(a *Answers) { a.Hostname = "bad_name" },
		func(a *Answers) { a.Hostname = "-x" },
		func(a *Answers) { a.User = "root" },
		func(a *Answers) { a.User = "Admin" },
		func(a *Answers) { a.Password = "short" },
		func(a *Answers) { a.Password = "line\nbreak!" },
		func(a *Answers) { a.Layout = RAIDZ1 },
		func(a *Answers) { a.Disks = nil },
		func(a *Answers) { a.Timezone = "../../etc/passwd" },
		func(a *Answers) { a.Keyboard = "us; rm" },
		func(a *Answers) { a.Network.Mode = "static"; a.Network.Address = "192.168.1.10" },
		func(a *Answers) {
			a.Network.Mode = "static"
			a.Network.Address = "192.168.1.10/24"
			a.Network.Gateway = "10.0.0.1"
		},
		func(a *Answers) {
			a.Network.Mode = "static"
			a.Network.Address = "192.168.1.0/24"
			a.Network.Gateway = "192.168.1.1"
		},
		func(a *Answers) { a.Network.WifiPassword = "short" },
		func(a *Answers) { a.Network.WifiSSID = `x"y` },
	}
	for i, f := range bad {
		a := good()
		f(&a)
		if err := a.Check(nil); err == nil {
			t.Errorf("case %d: accepted %+v", i, a)
		}
	}
	a = good()
	a.Network = Network{Mode: "static", Address: "192.168.1.10/24", Gateway: "192.168.1.1"}
	if err := a.Check(func(z string) bool { return z == "Etc/UTC" }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Network.DNS, " ") != "1.1.1.1 8.8.8.8" {
		t.Fatalf("dns %v", a.Network.DNS)
	}
	a.Timezone = "Mars/Olympus"
	if err := a.Check(func(z string) bool { return z == "Etc/UTC" }); err == nil {
		t.Fatal("unknown zone accepted")
	}
	one := Answers{Disks: []string{"sdb"}, User: "admin", Password: "12345678"}
	if err := one.Check(nil); err != nil || one.Layout != Single {
		t.Fatalf("one disk default: %v %s", err, one.Layout)
	}
}

func TestReadAnswers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.json")
	os.WriteFile(p, []byte(`{"disks":["sdb"],"layout":"single","user":"admin","password":"12345678","erase":true}`), 0o600)
	a, err := ReadAnswers(p)
	if err != nil || a.Layout != Single || !a.Erase {
		t.Fatalf("%+v %v", a, err)
	}
	os.WriteFile(p, []byte(`{"disk":["sdb"]}`), 0o600)
	if _, err := ReadAnswers(p); err == nil {
		t.Fatal("unknown field accepted (a typo would silently pick no disks)")
	}
}

var nics = []NIC{{Name: "wlo1", Wireless: true}, {Name: "enp2s0", MAC: "aa:bb", Link: false}, {Name: "enp3s0", MAC: "cc:dd", Link: true}}

func TestNewPlanChecks(t *testing.T) {
	disks := testDisks(t)
	if _, err := NewPlan(good(), disks, nics, true, nil); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*Answers){
		"not found":   func(a *Answers) { a.Disks = []string{"sdb", "sdz"} },
		"live medium": func(a *Answers) { a.Disks = []string{"sdb", "sdd"} },
		"mounted":     func(a *Answers) { a.Disks = []string{"sdb", "nvme0n1"} },
		"twice":       func(a *Answers) { a.Disks = []string{"sdb", "/dev/sdb"} },
		"no port":     func(a *Answers) { a.Network.Interface = "eth9" },
		"wifi no ssid": func(a *Answers) {
			a.Network.Interface = "wlo1"
		},
		"ext4 too big": func(a *Answers) { a.Disks = []string{"sdb"}; a.Layout = Ext4; a.RootGB = 480 },
	} {
		a := good()
		f(&a)
		if _, err := NewPlan(a, disks, nics, true, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p, _ := NewPlan(good(), disks, nics, true, nil)
	if p.NIC.Name != "enp3s0" {
		t.Fatalf("auto port = %s, want the wired one with a cable", p.NIC.Name)
	}
	if _, err := NewPlan(good(), disks, nil, true, nil); err == nil {
		t.Fatal("no network ports accepted")
	}
}

func joined(steps []Step) string {
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s.String())
		b.WriteString("\n")
	}
	return b.String()
}

func TestStepsMirrorUEFI(t *testing.T) {
	disks := testDisks(t)
	a := good()
	a.Encrypt = true
	p, err := NewPlan(a, disks, nics, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	steps := p.Steps()
	all := joined(steps)
	b0 := "/dev/disk/by-id/ata-Samsung_SSD_860_S3Z9NB0K"
	for _, want := range []string{
		"sgdisk -a1 -n1:24K:+1000K -t1:EF02 -c1:p8s-bios0 /dev/sdb",
		"-c4:p8s-pool1 /dev/sdc",
		"wait for " + b0 + "-part2",
		"mkdosfs -F 32 -s 1 -n EFI /dev/sdc2",
		"-o compatibility=grub2",
		"bpool mirror " + b0 + "-part3 /dev/sdc3",
		"-R /mnt tank mirror " + b0 + "-part4 /dev/sdc4",
		"zfs create -o canmount=noauto -o mountpoint=/ tank/ROOT/debian",
		"zpool set bootfs=tank/ROOT/debian tank",
		"unsquashfs -f -no-progress -d /mnt /run/live/medium/live/filesystem.squashfs",
		"mount " + b0 + "-part2 /mnt/boot/efi",
		"write /etc/default/grub.d/p8s-zfs.cfg",
		"write /etc/p8s/encrypt-storage",
		"[new system] env P8S_ENCRYPT=1 bash /opt/p8sos/iso/late.sh",
		"[new system] useradd -m -s /bin/bash -G sudo,adm -c 'P8S admin' admin",
		"[new system] chpasswd  <<< (hidden input)",
		"efibootmgr -q -c -d /dev/sdc -p 2 -L 'P8S OS (disk 2)'",
		"[new system] grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=p8s",
		"--removable --no-nvram",
		"[new system] update-grub",
		"dd if=" + b0 + "-part2 of=/dev/sdc2 bs=4M conv=fsync status=none",
		"write /etc/apt/apt.conf.d/99p8s-esp-sync",
		"zpool export -a",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "correct horse") {
		t.Fatal("the password shows in the dry-run")
	}
	if strings.Contains(all, "/dev/sda") || strings.Contains(all, "sdd") {
		t.Fatal("a disk that wasn't chosen appears in the plan")
	}
	// order: the efibootmgr spares come before grub-install (so disk 1 ends up first)
	if strings.Index(all, "efibootmgr") > strings.Index(all, "--bootloader-id=p8s") {
		t.Error("spare boot entries must be made before grub-install")
	}
	if strings.Index(all, "umount /mnt/boot/efi") > strings.Index(all, "dd if=") {
		t.Error("the EFI partition must be unmounted before it is copied")
	}
	for _, s := range steps {
		n := 0
		if s.Cmd != nil {
			n++
		}
		if s.File != nil {
			n++
		}
		if s.Wait != nil {
			n++
		}
		if n != 1 {
			t.Errorf("step %q: exactly one of Cmd/File/Wait", s.Title)
		}
	}
}

func TestStepsRAID10AndExt4BIOS(t *testing.T) {
	disks := []Disk{{Name: "sda", Path: "/dev/sda", Size: 1e12}, {Name: "sdb", Path: "/dev/sdb", Size: 1e12},
		{Name: "sdc", Path: "/dev/sdc", Size: 1e12}, {Name: "sdd", Path: "/dev/sdd", Size: 1e12}}
	a := good()
	a.Disks, a.Layout = []string{"sda", "sdb", "sdc", "sdd"}, RAID10
	p, err := NewPlan(a, disks, nics, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	all := joined(p.Steps())
	if !strings.Contains(all, "tank mirror /dev/sda4 /dev/sdb4 mirror /dev/sdc4 /dev/sdd4") ||
		!strings.Contains(all, "bpool mirror /dev/sda3 /dev/sdb3 /dev/sdc3 /dev/sdd3") {
		t.Fatalf("raid10 pools:\n%s", all)
	}
	for _, d := range []string{"sda", "sdb", "sdc", "sdd"} {
		if !strings.Contains(all, "grub-install --target=i386-pc --recheck /dev/"+d) {
			t.Errorf("no BIOS boot loader on %s", d)
		}
	}
	if strings.Contains(all, "efibootmgr") || strings.Contains(all, "x86_64-efi") {
		t.Error("UEFI steps in a BIOS install")
	}
	if !strings.Contains(all, "grub-pc/install_devices multiselect /dev/sda, /dev/sdb, /dev/sdc, /dev/sdd") {
		t.Error("grub-pc install devices not set")
	}

	a = good()
	a.Disks, a.Layout = []string{"sda"}, Ext4
	p, err = NewPlan(a, disks, nics, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.A.RootGB != 32 {
		t.Fatalf("root %d", p.A.RootGB)
	}
	all = joined(p.Steps())
	for _, want := range []string{"-n3:0:+32G -t3:8300 -c3:p8s-root", "mkfs.ext4 -F -q -L p8s-root /dev/sda3", "mount /dev/sda3 /mnt", "-R /mnt tank /dev/sda4"} {
		if !strings.Contains(all, want) {
			t.Errorf("ext4: missing %q", want)
		}
	}
	if strings.Contains(all, "bpool") || strings.Contains(all, "root=ZFS") || strings.Contains(all, "dd if=") {
		t.Error("ext4 install has ZFS-root or multi-disk steps")
	}
	if !strings.Contains(p.fstab(), "PARTLABEL=p8s-root / ext4") {
		t.Errorf("fstab:\n%s", p.fstab())
	}
}

func TestFiles(t *testing.T) {
	disks := testDisks(t)
	a := good()
	a.Network = Network{Interface: "wlo1", Mode: "static", Address: "192.168.1.10/24", Gateway: "192.168.1.1", WifiSSID: "Home", WifiPassword: "secret123"}
	a.Keyboard = "de-nodeadkeys"
	p, err := NewPlan(a, disks, nics, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := p.hosts(); !strings.Contains(h, "192.168.1.10\tp8s1.example.com p8s1\n") {
		t.Errorf("hosts:\n%s", h)
	}
	i := p.interfaces()
	for _, want := range []string{"allow-hotplug wlo1", "iface wlo1 inet static", "address 192.168.1.10/24", "gateway 192.168.1.1", `wpa-ssid "Home"`, `wpa-psk "secret123"`} {
		if !strings.Contains(i, want) {
			t.Errorf("interfaces missing %q:\n%s", want, i)
		}
	}
	if r := p.resolv(); r != "search example.com\nnameserver 1.1.1.1\nnameserver 8.8.8.8\n" {
		t.Errorf("resolv:\n%s", r)
	}
	if f := p.fstab(); !strings.Contains(f, "PARTLABEL=p8s-esp0 /boot/efi vfat") || !strings.Contains(f, "zfs-mount.service") {
		t.Errorf("fstab:\n%s", f)
	}
	all := joined(p.Steps())
	if !strings.Contains(all, "write /etc/resolv.conf") {
		t.Error("static DNS not written")
	}
	var kb string
	for _, s := range p.Steps() {
		if s.File != nil && s.File.Path == "/etc/default/keyboard" {
			kb = s.File.Data
		}
		if s.File != nil && s.File.Path == "/etc/network/interfaces" && s.File.Mode != 0o600 {
			t.Error("interfaces holds the Wi-Fi password: must not be world-readable")
		}
	}
	if !strings.Contains(kb, `XKBLAYOUT="de"`) || !strings.Contains(kb, `XKBVARIANT="nodeadkeys"`) {
		t.Errorf("keyboard:\n%s", kb)
	}
	if !strings.Contains(p.Summary(), "survives 1 failed disk") || !strings.Contains(p.Summary(), "ERASED:") {
		t.Errorf("summary:\n%s", p.Summary())
	}
	if d := p.HasData(); len(d) != 0 {
		t.Errorf("empty disks reported as having data: %v", d)
	}
}

func TestRunner(t *testing.T) {
	disks := []Disk{{Name: "sda", Path: "/dev/sda", Size: 1e12}}
	a := good()
	a.Disks, a.Layout = []string{"sda"}, Single
	p, err := NewPlan(a, disks, nics, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Target = t.TempDir()
	var ran [][]string
	var stdins []string
	last := -1
	r := &Runner{Plan: p, Exists: func(string) bool { return true },
		Exec: func(_ context.Context, stdin string, argv []string, _ io.Writer) error {
			ran = append(ran, argv)
			if stdin != "" {
				stdins = append(stdins, stdin)
			}
			if argv[0] == "swapoff" {
				return errors.New("no swap") // may fail
			}
			return nil
		},
		Progress: func(pct int, _ string) {
			if pct < last {
				t.Errorf("progress went back: %d after %d", pct, last)
			}
			last = pct
		}}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last != 100 {
		t.Fatalf("progress ended at %d", last)
	}
	if len(stdins) != 1 || stdins[0] != "admin:correct horse\n" {
		t.Fatalf("stdin %q", stdins)
	}
	b, err := os.ReadFile(filepath.Join(p.Target, "etc/hostname"))
	if err != nil || string(b) != "p8s1\n" {
		t.Fatalf("hostname file %q %v", b, err)
	}
	var chroot []string
	for _, c := range ran {
		if c[0] == "chroot" {
			chroot = c
			break
		}
	}
	if len(chroot) < 4 || chroot[1] != p.Target || chroot[2] != "env" || chroot[3] != "DEBIAN_FRONTEND=noninteractive" {
		t.Fatalf("chroot argv %v", chroot)
	}

	// nothing happens without "erase" (the menus set it after the person types ERASE)
	p.A.Erase = false
	if err := r.Run(context.Background()); !errors.Is(err, ErrNotErase) {
		t.Fatalf("ran without erase: %v", err)
	}
	p.A.Erase = true

	// a failing step stops the install
	r.Progress = nil
	n := 0
	r.Exec = func(_ context.Context, _ string, argv []string, _ io.Writer) error {
		n++
		if argv[0] == "sgdisk" {
			return errors.New("boom")
		}
		return nil
	}
	err = r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Partitioning sda") {
		t.Fatalf("err = %v", err)
	}
	if n > 5 {
		t.Fatalf("kept going after the failure (%d commands)", n)
	}
	// partitions that never appear
	r.Exec = func(context.Context, string, []string, io.Writer) error { return nil }
	r.Exists = func(string) bool { return false }
	r.Wait = 1
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Fatalf("wait err = %v", err)
	}
}

func TestWriteFileStaysInside(t *testing.T) {
	root := t.TempDir()
	if err := writeFile(root, File{Path: "/../../etc/x", Mode: 0o644}); err == nil {
		t.Fatal("path outside the target accepted")
	}
	if err := writeFile(root, File{Path: "etc/x", Mode: 0o644}); err == nil {
		t.Fatal("relative path accepted")
	}
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.Symlink("/etc/passwd", filepath.Join(root, "etc/resolv.conf"))
	if err := writeFile(root, File{Path: "/etc/resolv.conf", Data: "nameserver 1.1.1.1\n", Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "etc/resolv.conf")); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("wrote through a symlink")
	}
}

func TestReadNICs(t *testing.T) {
	sys := t.TempDir()
	mk := func(name string, files map[string]string, dirs ...string) {
		os.MkdirAll(filepath.Join(sys, name), 0o755)
		for f, v := range files {
			os.WriteFile(filepath.Join(sys, name, f), []byte(v+"\n"), 0o644)
		}
		for _, d := range dirs {
			os.MkdirAll(filepath.Join(sys, name, d), 0o755)
		}
	}
	mk("lo", map[string]string{"address": "00:00:00:00:00:00"})
	mk("br0", map[string]string{"address": "aa"})
	mk("wlo1", map[string]string{"address": "w1", "carrier": "0"}, "device", "wireless")
	mk("enp2s0", map[string]string{"address": "e2", "carrier": "0"}, "device")
	mk("enp3s0", map[string]string{"address": "e3", "carrier": "1"}, "device")
	got := ReadNICs(sys)
	var names []string
	for _, n := range got {
		names = append(names, n.Name)
	}
	if strings.Join(names, " ") != "enp3s0 enp2s0 wlo1" {
		t.Fatalf("nics %v", names)
	}
	if !got[2].Wireless || !got[0].Link {
		t.Fatalf("%+v", got)
	}
}
