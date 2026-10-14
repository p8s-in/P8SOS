package installer

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Pool names: the installer's pools match what the rest of P8S OS expects, so
// VM disks land in tank/p8s/vms as on every other P8S server.
const (
	RootPool = "tank"
	BootPool = "bpool" // /boot: a small pool with only the features GRUB can read
	RootDS   = RootPool + "/ROOT/debian"
)

// Plan is a checked set of answers bound to real disks and a network port.
type Plan struct {
	A        Answers
	Disks    []Disk
	NIC      NIC
	UEFI     bool   // the machine booted the installer in UEFI mode
	Target   string // where the new system is mounted while installing: /mnt
	Squashfs string // the system image to copy: /run/live/medium/live/filesystem.squashfs
	Log      string // installer log, copied onto the new system at the end
}

// NewPlan checks the answers against the machine.
func NewPlan(a Answers, disks []Disk, nics []NIC, uefi bool, zoneOK func(string) bool) (*Plan, error) {
	if err := a.Check(zoneOK); err != nil {
		return nil, err
	}
	p := &Plan{A: a, UEFI: uefi, Target: "/mnt", Squashfs: LiveMount + "/live/filesystem.squashfs", Log: "/var/log/p8s-install.log"}
	seen := map[string]bool{}
	for _, name := range a.Disks {
		d, ok := Find(disks, name)
		if !ok {
			return nil, fmt.Errorf("disk %q not found (run lsblk to see the disks)", name)
		}
		if !d.Usable() {
			return nil, fmt.Errorf("disk %s can't be used: %s", d.Name, d.Why)
		}
		if seen[d.Path] {
			return nil, fmt.Errorf("disk %s is listed twice", d.Name)
		}
		seen[d.Path] = true
		if a.Layout == Ext4 && d.Size < int64(a.RootGB+16)<<30 {
			return nil, fmt.Errorf("disk %s (%s) is too small for a %d GB system partition plus VM storage", d.Name, HumanBytes(d.Size), a.RootGB)
		}
		p.Disks = append(p.Disks, d)
	}
	if a.Network.Interface == "auto" {
		n, ok := PickNIC(nics)
		if !ok {
			return nil, fmt.Errorf("no network port found; the server needs one (it downloads updates on first start)")
		}
		p.NIC = n
	} else {
		for _, n := range nics {
			if n.Name == a.Network.Interface {
				p.NIC = n
			}
		}
		if p.NIC.Name == "" {
			return nil, fmt.Errorf("network port %q not found", a.Network.Interface)
		}
	}
	if p.NIC.Wireless && p.A.Network.WifiSSID == "" {
		return nil, fmt.Errorf("%s is Wi-Fi: give the Wi-Fi name (and password)", p.NIC.Name)
	}
	if !p.NIC.Wireless {
		p.A.Network.WifiSSID, p.A.Network.WifiPassword = "", ""
	}
	return p, nil
}

// Sizes of the chosen disks.
func (p *Plan) Sizes() []int64 {
	var s []int64
	for _, d := range p.Disks {
		s = append(s, d.Size)
	}
	return s
}

// Summary is what the person confirms before anything is written.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Layout:   %s\n", p.A.Layout.Title())
	usable := p.A.Layout.Usable(p.Sizes())
	if p.A.Layout == Ext4 {
		fmt.Fprintf(&b, "Space:    %d GB system, about %s for VMs\n", p.A.RootGB, HumanBytes(usable-int64(p.A.RootGB)<<30))
	} else {
		fmt.Fprintf(&b, "Space:    about %s for the system and VMs", HumanBytes(usable))
		if s := p.A.Layout.Survives(len(p.Disks)); s > 0 {
			fmt.Fprintf(&b, ", survives %d failed disk(s)", s)
		}
		b.WriteString("\n")
	}
	b.WriteString("ERASED:\n")
	for _, d := range p.Disks {
		fmt.Fprintf(&b, "  %s\n     now: %s\n", d.Label(), d.State())
	}
	fmt.Fprintf(&b, "Server:   %s, time zone %s, keyboard %s\n", p.A.Hostname, p.A.Timezone, p.A.Keyboard)
	n := p.A.Network
	if n.Mode == "static" {
		fmt.Fprintf(&b, "Network:  %s %s, gateway %s, DNS %s\n", p.NIC.Name, n.Address, n.Gateway, strings.Join(n.DNS, " "))
	} else {
		fmt.Fprintf(&b, "Network:  %s, automatic address (DHCP)\n", p.NIC.Name)
	}
	if n.WifiSSID != "" {
		fmt.Fprintf(&b, "Wi-Fi:    %s\n", n.WifiSSID)
	}
	fmt.Fprintf(&b, "Admin:    %s (with sudo; root login off)\n", p.A.User)
	if p.A.Encrypt {
		b.WriteString("VM disks: encrypted at rest\n")
	}
	boot := "BIOS (legacy)"
	if p.UEFI {
		boot = "UEFI"
	}
	fmt.Fprintf(&b, "Boot:     %s, from every disk", boot)
	return b.String()
}

// HasData lists chosen disks that are not empty.
func (p *Plan) HasData() []Disk {
	var out []Disk
	for _, d := range p.Disks {
		if !d.Empty() {
			out = append(out, d)
		}
	}
	return out
}

// Step is one thing the installer does. Exactly one of Cmd, File and Wait is set.
type Step struct {
	Title   string
	Cmd     []string
	Chroot  bool   // run Cmd inside the new system
	Stdin   string // fed to Cmd; never logged (passwords)
	File    *File  // write a file into the new system
	Wait    []string
	MayFail bool // keep going if it fails
	Weight  int  // share of the progress bar (default 1)
}

// File is written below the target.
type File struct {
	Path string // absolute path in the new system: /etc/hostname
	Data string
	Mode os.FileMode
}

// String is the dry-run line.
func (s Step) String() string {
	switch {
	case s.File != nil:
		return fmt.Sprintf("write %s (%d bytes)", s.File.Path, len(s.File.Data))
	case len(s.Wait) > 0:
		return "wait for " + strings.Join(s.Wait, " ")
	}
	c := shellJoin(s.Cmd)
	if s.Chroot {
		c = "[new system] " + c
	}
	if s.Stdin != "" {
		c += "  <<< (hidden input)"
	}
	if s.MayFail {
		c += "  || true"
	}
	return c
}

func shellJoin(args []string) string {
	var out []string
	for _, a := range args {
		out = append(out, shq(a))
	}
	return strings.Join(out, " ")
}

// shq quotes a word for a shell when it needs it.
func shq(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (p *Plan) parts(n int) []string {
	var out []string
	for _, d := range p.Disks {
		out = append(out, d.Part(n))
	}
	return out
}

// Steps is the whole install, in order.
func (p *Plan) Steps() []Step {
	t := p.Target
	zroot := p.A.Layout.ZFSRoot()
	var s []Step
	cmd := func(title string, c ...string) { s = append(s, Step{Title: title, Cmd: c}) }
	soft := func(title string, c ...string) { s = append(s, Step{Title: title, Cmd: c, MayFail: true}) }
	in := func(title string, c ...string) { s = append(s, Step{Title: title, Cmd: c, Chroot: true}) }
	inSoft := func(title string, c ...string) {
		s = append(s, Step{Title: title, Cmd: c, Chroot: true, MayFail: true})
	}
	file := func(path, data string, mode os.FileMode) {
		s = append(s, Step{Title: "Writing " + path, File: &File{Path: path, Data: data, Mode: mode}})
	}

	// 1. Disks: wipe and partition
	soft("Turning off swap", "swapoff", "-a")
	cmd("Loading ZFS", "modprobe", "zfs")
	cmd("Loading ZFS", "mkdir", "-p", "/etc/zfs")
	// the pools remember the host id of the machine that has them open; the new
	// system gets the same id (below), so it imports them as its own on the first start
	cmd("Host id for the pools", "bash", "-c", "[ -s /etc/hostid ] || zgenhostid")
	for _, d := range p.Disks {
		cmd("Wiping "+d.Name, "bash", "-c", wipeScript(d.Path))
	}
	for i, d := range p.Disks {
		cmd("Partitioning "+d.Name, "sgdisk", "-a1", "-n1:24K:+1000K", "-t1:EF02", fmt.Sprintf("-c1:p8s-bios%d", i), d.Path)
		args := []string{"sgdisk", "-n2:1M:+512M", "-t2:EF00", fmt.Sprintf("-c2:p8s-esp%d", i)}
		if zroot {
			args = append(args, "-n3:0:+1G", "-t3:BF01", fmt.Sprintf("-c3:p8s-boot%d", i))
		} else {
			args = append(args, fmt.Sprintf("-n3:0:+%dG", p.A.RootGB), "-t3:8300", "-c3:p8s-root")
		}
		args = append(args, "-n4:0:0", "-t4:BF00", fmt.Sprintf("-c4:p8s-pool%d", i), d.Path)
		cmd("Partitioning "+d.Name, args...)
		soft("Re-reading "+d.Name, "partprobe", d.Path)
	}
	soft("Waiting for the new partitions", "udevadm", "settle")
	var wait []string
	for _, d := range p.Disks {
		for n := 2; n <= 4; n++ {
			wait = append(wait, d.Part(n))
		}
	}
	s = append(s, Step{Title: "Waiting for the new partitions", Wait: wait})
	for _, d := range p.Disks {
		cmd("Boot partition on "+d.Name, "mkdosfs", "-F", "32", "-s", "1", "-n", "EFI", d.Part(2))
	}

	// 2. File systems
	poolOpts := []string{"-f", "-o", "ashift=12", "-o", "autotrim=on", "-o", "cachefile=/etc/zfs/zpool.cache",
		"-O", "acltype=posixacl", "-O", "xattr=sa", "-O", "dnodesize=auto", "-O", "compression=lz4",
		"-O", "normalization=formD", "-O", "relatime=on", "-O", "canmount=off", "-O", "mountpoint=/tank", "-R", t}
	if zroot {
		boot := []string{"zpool", "create", "-f", "-o", "ashift=12", "-o", "autotrim=on", "-o", "compatibility=grub2",
			"-o", "cachefile=/etc/zfs/zpool.cache", "-O", "devices=off", "-O", "acltype=posixacl", "-O", "xattr=sa",
			"-O", "compression=lz4", "-O", "normalization=formD", "-O", "relatime=on", "-O", "canmount=off",
			"-O", "mountpoint=/boot", "-R", t, BootPool}
		if len(p.Disks) > 1 {
			boot = append(boot, "mirror") // GRUB reads mirrors; /boot is small, so every disk has a copy
		}
		cmd("Creating the boot pool", append(boot, p.parts(3)...)...)
		cmd("Creating the ZFS pool ("+string(p.A.Layout)+")", append(append([]string{"zpool", "create"}, poolOpts...),
			append([]string{RootPool}, p.A.Layout.vdevs(p.parts(4))...)...)...)
		cmd("Creating the system file system", "zfs", "create", "-o", "canmount=off", "-o", "mountpoint=none", RootPool+"/ROOT")
		cmd("Creating the system file system", "zfs", "create", "-o", "canmount=noauto", "-o", "mountpoint=/", RootDS)
		cmd("Mounting the system file system", "zfs", "mount", RootDS)
		cmd("Creating /boot", "zfs", "create", "-o", "canmount=off", "-o", "mountpoint=none", BootPool+"/BOOT")
		cmd("Creating /boot", "zfs", "create", "-o", "mountpoint=/boot", BootPool+"/BOOT/debian")
		cmd("Marking the system to boot", "zpool", "set", "bootfs="+RootDS, RootPool)
	} else {
		d := p.Disks[0]
		cmd("Creating the system file system", "mkfs.ext4", "-F", "-q", "-L", "p8s-root", d.Part(3))
		cmd("Mounting the system file system", "mount", d.Part(3), t)
		cmd("Creating the ZFS pool for VMs", append(append([]string{"zpool", "create"}, poolOpts...), RootPool, d.Part(4))...)
	}

	// 3. Copy the system
	s = append(s, Step{Title: "Copying P8S OS onto the disks", Cmd: []string{"unsquashfs", "-f", "-no-progress", "-d", t, p.Squashfs}, Weight: 40})
	cmd("Mounting the EFI partition", "mkdir", "-p", t+"/boot/efi", t+"/etc/zfs")
	cmd("Mounting the EFI partition", "mount", p.Disks[0].Part(2), t+"/boot/efi")
	cmd("Saving the pool list", "cp", "/etc/zfs/zpool.cache", t+"/etc/zfs/zpool.cache")
	cmd("Saving the host id", "cp", "/etc/hostid", t+"/etc/hostid")
	for _, m := range []string{"dev", "proc", "sys"} {
		cmd("Preparing the new system", "mount", "--make-private", "--rbind", "/"+m, t+"/"+m)
	}

	// 4. Configure it
	a := p.A
	file("/etc/hostname", a.Short()+"\n", 0o644)
	file("/etc/hosts", p.hosts(), 0o644)
	file("/etc/fstab", p.fstab(), 0o644)
	file("/etc/network/interfaces", p.interfaces(), 0o600) // may hold the Wi-Fi password
	if a.Network.Mode == "static" {
		soft("Writing /etc/resolv.conf", "rm", "-f", t+"/etc/resolv.conf")
		file("/etc/resolv.conf", p.resolv(), 0o644)
	}
	soft("Writing the package sources", "rm", "-f", t+"/etc/apt/sources.list")
	file("/etc/apt/sources.list.d/debian.sources", debianSources, 0o644)
	file("/etc/default/locale", "LANG=C.UTF-8\n", 0o644)
	layout, variant := keyboardLayout(a.Keyboard)
	file("/etc/default/keyboard", fmt.Sprintf("XKBMODEL=\"pc105\"\nXKBLAYOUT=\"%s\"\nXKBVARIANT=\"%s\"\nXKBOPTIONS=\"\"\nBACKSPACE=\"guess\"\n", layout, variant), 0o644)
	file("/etc/timezone", a.Timezone+"\n", 0o644)
	cmd("Setting the time zone", "ln", "-sfn", "/usr/share/zoneinfo/"+a.Timezone, t+"/etc/localtime")
	// the boot menu says P8S OS, and GRUB lives in EFI/p8s (Debian derives that from the name on upgrades)
	file("/etc/default/grub.d/p8s.cfg", "GRUB_DISTRIBUTOR=\"P8S OS\"\n", 0o644)
	if zroot {
		file("/etc/default/grub.d/p8s-zfs.cfg", "GRUB_CMDLINE_LINUX=\"$GRUB_CMDLINE_LINUX root=ZFS="+RootDS+"\"\n", 0o644)
	}
	file("/etc/p8s/installed.json", p.record(), 0o644)
	if a.Encrypt {
		file("/etc/p8s/encrypt-storage", "", 0o644)
	}

	inSoft("Removing the installer's live-system parts", "bash", "-c",
		`dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' 'live-*' 2>/dev/null | awk '$1=="ii" {print $2}' | xargs -r apt-get purge -y`)
	in("New machine ID", "bash", "-c", "rm -f /etc/machine-id /var/lib/dbus/machine-id && systemd-machine-id-setup")
	in("New SSH host keys", "bash", "-c", "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A")
	in("Creating the admin user "+a.User, "useradd", "-m", "-s", "/bin/bash", "-G", "sudo,adm", "-c", "P8S admin", a.User)
	s = append(s, Step{Title: "Setting the admin password", Cmd: []string{"chpasswd"}, Chroot: true, Stdin: a.User + ":" + a.Password + "\n"})
	in("Turning off root login", "passwd", "-l", "root")
	in("Turning on SSH", "systemctl", "enable", "ssh.service")
	enc := []string{}
	if a.Encrypt {
		enc = []string{"P8S_ENCRYPT=1"}
	}
	in("Arming the first-boot setup", append(append([]string{"env"}, enc...), "bash", "/opt/p8sos/iso/late.sh")...)
	inSoft("Removing the installer", "bash", "-c", "systemctl disable p8s-installer.service; rm -f /etc/systemd/system/p8s-installer.service /usr/local/sbin/p8s-install")
	if len(p.Disks) > 1 {
		in("Keeping the boot partitions in sync", "install", "-m", "0755", "/opt/p8sos/scripts/esp-sync.sh", "/usr/local/sbin/p8s-esp-sync")
		file("/etc/apt/apt.conf.d/99p8s-esp-sync", "DPkg::Post-Invoke { \"[ -x /usr/local/sbin/p8s-esp-sync ] && /usr/local/sbin/p8s-esp-sync || true\"; };\n", 0o644)
	}

	// 5. Boot loader on every disk
	if p.UEFI {
		in("Installing the boot loader (UEFI)", "bash", "-c",
			"echo 'grub-efi-amd64 grub2/force_efi_extra_removable boolean true' | debconf-set-selections && dpkg -i /opt/p8s-debs/efi/*.deb")
		for i := len(p.Disks) - 1; i >= 1; i-- { // spare boot entries; the first disk's comes last, so it's tried first
			soft("Boot entry for "+p.Disks[i].Name, "efibootmgr", "-q", "-c", "-d", p.Disks[i].Path, "-p", "2",
				"-L", fmt.Sprintf("P8S OS (disk %d)", i+1), "-l", `\EFI\p8s\grubx64.efi`)
		}
		in("Installing the boot loader (UEFI)", "grub-install", "--target=x86_64-efi", "--efi-directory=/boot/efi",
			"--bootloader-id=p8s", "--recheck", "--no-floppy")
		in("Installing the fallback boot loader", "grub-install", "--target=x86_64-efi", "--efi-directory=/boot/efi",
			"--removable", "--no-nvram", "--no-floppy")
	} else {
		var devs []string
		for _, d := range p.Disks {
			devs = append(devs, d.Dev())
		}
		in("Installing the boot loader (BIOS)", "bash", "-c",
			"echo 'grub-pc grub-pc/install_devices multiselect "+strings.Join(devs, ", ")+"' | debconf-set-selections && dpkg -i /opt/p8s-debs/bios/*.deb")
		for _, d := range p.Disks {
			in("Boot loader on "+d.Name, "grub-install", "--target=i386-pc", "--recheck", d.Path)
		}
	}
	inSoft("Cleaning up", "rm", "-rf", "/opt/p8s-debs")
	in("Building the boot image", "update-initramfs", "-u", "-k", "all")
	in("Writing the boot menu", "update-grub")

	// 6. Finish
	soft("Saving the install log", "cp", p.Log, t+"/var/log/p8s-install.log")
	cmd("Unmounting", "umount", t+"/boot/efi")
	for i := 1; i < len(p.Disks); i++ {
		cmd("Copying the boot partition to "+p.Disks[i].Name, "dd", "if="+p.Disks[0].Part(2), "of="+p.Disks[i].Part(2), "bs=4M", "conv=fsync", "status=none")
	}
	// programs started inside the new system (by package scripts) still hold it: stop them
	soft("Stopping programs in the new system", "bash", "-c", killInTarget(t))
	soft("Unmounting the new system", "bash", "-c", unmountScript(t))
	cmd("Closing the pools", "bash", "-c", exportScript)
	return s
}

// wipeScript clears every partition's signatures (old ZFS labels too, which
// would otherwise make the new pool look like an old one) and the partition table.
func wipeScript(dev string) string {
	q := shq(dev)
	return "set -e\n" +
		"for p in $(lsblk -lnpo NAME,TYPE " + q + " | awk '$2==\"part\" {print $1}'); do zpool labelclear -f \"$p\" 2>/dev/null || true; wipefs -af \"$p\" >/dev/null; done\n" +
		"zpool labelclear -f " + q + " 2>/dev/null || true\n" +
		"wipefs -af " + q + " >/dev/null\n" +
		"sgdisk --zap-all " + q + " >/dev/null\n"
}

func (p *Plan) hosts() string {
	a := p.A
	names := a.Short()
	if a.Hostname != a.Short() {
		names = a.Hostname + " " + a.Short()
	}
	ip := "127.0.1.1"
	if a.Network.Mode == "static" {
		ip = strings.SplitN(a.Network.Address, "/", 2)[0]
	}
	return fmt.Sprintf("127.0.0.1\tlocalhost\n%s\t%s\n\n::1\tlocalhost ip6-localhost ip6-loopback\nff02::1\tip6-allnodes\nff02::2\tip6-allrouters\n", ip, names)
}

func (p *Plan) fstab() string {
	var b strings.Builder
	b.WriteString("# <file system> <mount point> <type> <options> <dump> <pass>\n")
	if p.A.Layout.ZFSRoot() {
		b.WriteString("# / and /boot are ZFS (tank/ROOT/debian, bpool/BOOT/debian): mounted by ZFS, not from here.\n")
		b.WriteString("PARTLABEL=p8s-esp0 /boot/efi vfat defaults,nofail,x-systemd.device-timeout=10s,x-systemd.requires=zfs-mount.service 0 1\n")
	} else {
		b.WriteString("PARTLABEL=p8s-root / ext4 errors=remount-ro 0 1\n")
		b.WriteString("PARTLABEL=p8s-esp0 /boot/efi vfat defaults,nofail,x-systemd.device-timeout=10s 0 1\n")
	}
	if len(p.Disks) > 1 {
		b.WriteString("# Every disk has a copy of the EFI partition (p8s-esp1, ...); p8s-esp-sync keeps them equal.\n")
	}
	return b.String()
}

func (p *Plan) interfaces() string {
	n := p.A.Network
	var b strings.Builder
	b.WriteString("# Written by the P8S OS installer. See interfaces(5).\nsource /etc/network/interfaces.d/*\n\nauto lo\niface lo inet loopback\n\n")
	if p.NIC.Wireless {
		fmt.Fprintf(&b, "allow-hotplug %s\n", p.NIC.Name)
	} else {
		fmt.Fprintf(&b, "auto %s\n", p.NIC.Name)
	}
	if n.Mode == "static" {
		fmt.Fprintf(&b, "iface %s inet static\n\taddress %s\n\tgateway %s\n", p.NIC.Name, n.Address, n.Gateway)
	} else {
		fmt.Fprintf(&b, "iface %s inet dhcp\n", p.NIC.Name)
	}
	if n.WifiSSID != "" {
		fmt.Fprintf(&b, "\twpa-ssid \"%s\"\n", n.WifiSSID)
		if n.WifiPassword != "" {
			fmt.Fprintf(&b, "\twpa-psk \"%s\"\n", n.WifiPassword)
		} else {
			b.WriteString("\twpa-key-mgmt NONE\n")
		}
	}
	return b.String()
}

func (p *Plan) resolv() string {
	var b strings.Builder
	if p.A.Hostname != p.A.Short() {
		fmt.Fprintf(&b, "search %s\n", strings.SplitN(p.A.Hostname, ".", 2)[1])
	}
	for _, d := range p.A.Network.DNS {
		fmt.Fprintf(&b, "nameserver %s\n", d)
	}
	return b.String()
}

// record is /etc/p8s/installed.json: how this server was installed.
func (p *Plan) record() string {
	var disks []string
	for _, d := range p.Disks {
		disks = append(disks, fmt.Sprintf("%q", d.Dev()))
	}
	sort.Strings(disks)
	return fmt.Sprintf("{\n  \"layout\": %q,\n  \"disks\": [%s],\n  \"root\": %q,\n  \"uefi\": %t\n}\n",
		p.A.Layout, strings.Join(disks, ", "), map[bool]string{true: RootDS, false: "ext4"}[p.A.Layout.ZFSRoot()], p.UEFI)
}

// keyboardLayout splits "de-nodeadkeys" into layout and variant.
func keyboardLayout(k string) (string, string) {
	l, v, _ := strings.Cut(k, "-")
	return l, v
}

const debianSources = `Types: deb
URIs: http://deb.debian.org/debian
Suites: trixie trixie-updates
Components: main contrib non-free-firmware
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://security.debian.org/debian-security
Suites: trixie-security
Components: main contrib non-free-firmware
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
`

// killInTarget stops processes whose root or working directory is inside the target.
func killInTarget(t string) string {
	return fmt.Sprintf(holdersScript, shq(t)) + `
for sig in TERM KILL; do
  pids=$(holders)
  [ -z "$pids" ] && break
  for p in $pids; do echo "stopping $p $(cat "/proc/$p/comm" 2>/dev/null) ($sig)"; kill -$sig "$p" 2>/dev/null; done
  sleep 2
done
true`
}

// holdersScript defines holders(): the processes (not this shell) whose root,
// working directory, program, open files or mapped files are below %s, read
// from /proc (the installer has no fuser). The installer's own files are never
// below the new system's folder.
const holdersScript = `T=%s
holders() {
  for d in /proc/[0-9]*; do
    p=${d#/proc/}
    [ "$p" = "$$" ] && continue
    hit=
    for l in "$d/root" "$d/cwd" "$d/exe" "$d"/fd/*; do
      case "$(readlink "$l" 2>/dev/null)" in "$T"|"$T"/*) hit=1; break;; esac
    done
    [ -z "$hit" ] && grep -q " $T/" "$d/maps" 2>/dev/null && hit=1
    [ -n "$hit" ] && echo "$p"
  done
}
`

// unmountScript unmounts everything below the new system's folder, deepest
// first, without "lazy" unmounts (a lazily unmounted ZFS file system keeps its
// pool busy); lazy only as the very last resort for non-ZFS mounts.
func unmountScript(t string) string {
	return fmt.Sprintf(holdersScript, shq(t)) + `sync
below() { awk -v t="$T" '$2==t || index($2, t"/")==1 {print $2}' /proc/self/mounts | sort -r; }
for _ in 1 2 3 4 5; do
  [ -z "$(below)" ] && exit 0
  for m in $(below); do umount "$m" 2>/dev/null; done
  [ -z "$(below)" ] && exit 0
  for p in $(holders); do kill -KILL "$p" 2>/dev/null; done
  sleep 2
done
echo "still mounted:"; below
for m in $(below); do
  if [ "$(awk -v m="$m" '$2==m {print $3}' /proc/self/mounts | tail -1)" = zfs ]; then zfs unmount -f "$m" 2>&1 || umount -f "$m"; else umount -l "$m"; fi
done
[ -z "$(below)" ]`
}

// exportScript closes the pools so the new system imports them cleanly. If
// one stays busy it says why (mounts, holders, datasets) and fails: the new
// system shares this machine's host id, so it still imports a pool that wasn't
// closed, but a busy pool means something is wrong and must be looked at.
const exportScript = `zfs unmount -a 2>/dev/null
for _ in 1 2 3 4 5 6 7 8 9 10; do zpool export -a 2>/dev/null && exit 0; sleep 2; done
zpool export -a && exit 0
echo "== mounts of the pools:"; awk '$3=="zfs"' /proc/self/mounts
echo "== datasets:"; zfs list -H -o name,mounted,mountpoint 2>&1 | head -20
echo "== volumes in use:"; find /dev/zvol -maxdepth 3 2>&1 | head
echo "== programs using the new system:"; for d in /proc/[0-9]*; do for l in "$d/root" "$d/cwd" "$d/exe"; do case "$(readlink "$l" 2>/dev/null)" in /mnt|/mnt/*) echo "${d#/proc/} $(cat "$d/comm" 2>/dev/null) $(readlink "$l")"; break;; esac; done; done
for p in $(zpool list -H -o name 2>/dev/null); do zpool export -f "$p"; done
[ -z "$(zpool list -H -o name 2>/dev/null)" ]`
