// Command p8s-install is the P8S OS installer. The installer ISO starts it on
// the first screen; it asks a few questions in text menus (whiptail), then puts
// P8S OS on the disks chosen, with the system on ZFS (one disk, mirror, RAID 10
// or RAID-Z) or ext4.
//
//	p8s-install                       menus (the ISO runs this)
//	p8s-install --answers FILE        no questions: everything from a JSON file
//	p8s-install --answers FILE --dry-run   print what would be done, change nothing
//	p8s-install --list                the disks and network ports it sees
//
// The ISO's boot option p8s.answers=PATH (a file on the USB stick, like
// p8s.answers=/answers.json) also installs without questions.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"p8sos/internal/installer"
)

const zoneDir = "/usr/share/zoneinfo"

func main() {
	answers := flag.String("answers", "", "install without questions, from this JSON file")
	dry := flag.Bool("dry-run", false, "print the plan and every command; change nothing")
	list := flag.Bool("list", false, "list the disks and network ports, then exit")
	target := flag.String("target", "/mnt", "where the new system is mounted while installing")
	squash := flag.String("squashfs", "", "system image to copy (default: the live medium's)")
	logPath := flag.String("log", "/var/log/p8s-install.log", "install log")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	m := &machine{}
	if err := m.scan(ctx); err != nil {
		fail(err)
	}
	if *list {
		m.print(os.Stdout)
		return
	}
	if *answers == "" {
		*answers = cmdlineAnswers()
	}
	if *answers != "" {
		a, err := installer.ReadAnswers(*answers)
		if err != nil {
			fail(err)
		}
		p, err := installer.NewPlan(a, m.disks, m.nics, m.uefi, zoneOK)
		if err != nil {
			fail(err)
		}
		m.configure(p, *target, *squash, *logPath)
		fmt.Println(p.Summary())
		if *dry {
			printSteps(p)
			return
		}
		if err := zfsReady(ctx); err != nil {
			fail(err)
		}
		fmt.Println("\nInstalling in 10 seconds (Ctrl+C stops it) ...")
		time.Sleep(10 * time.Second)
		if err := install(ctx, p, nil); err != nil {
			fail(fmt.Errorf("%w\nlog: %s", err, *logPath))
		}
		fmt.Println("P8S OS is installed. Restarting in 10 seconds; remove the USB stick.")
		time.Sleep(10 * time.Second)
		_ = exec.Command("systemctl", "reboot").Run()
		return
	}
	if *dry {
		fail(errors.New("--dry-run needs --answers"))
	}
	ui := &menus{m: m, target: *target, squash: *squash, log: *logPath}
	if err := zfsReady(ctx); err != nil {
		msg(err.Error())
	}
	ui.run(ctx)
}

// zfsReady loads ZFS. It fails on a computer with Secure Boot on (the ZFS
// module is built on the spot, so it isn't signed by Microsoft's key).
func zfsReady(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "modprobe", "zfs").CombinedOutput(); err != nil {
		why := strings.TrimSpace(string(out))
		if sb, _ := exec.CommandContext(ctx, "mokutil", "--sb-state").Output(); strings.Contains(string(sb), "enabled") ||
			strings.Contains(why, "Key was rejected") || strings.Contains(why, "Operation not permitted") {
			return errors.New("ZFS can't start because Secure Boot is on.\n\nRestart, open the firmware setup (F2 or Del), " +
				"turn Secure Boot off, save, and boot this USB stick again.")
		}
		return fmt.Errorf("ZFS can't start: %s\n\nThe installer needs it for every layout.", why)
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "p8s-install:", err)
	os.Exit(1)
}

func zoneOK(z string) bool {
	fi, err := os.Stat(filepath.Join(zoneDir, z))
	return err == nil && fi.Mode().IsRegular()
}

// cmdlineAnswers reads p8s.answers=PATH from the kernel command line; a
// relative or plain /path is looked up on the installer medium.
func cmdlineAnswers() string {
	b, _ := os.ReadFile("/proc/cmdline")
	for _, f := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(f, "p8s.answers="); ok && v != "" {
			if p := filepath.Join(installer.LiveMount, v); fileExists(p) {
				return p
			}
			return v
		}
	}
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// machine is what the installer found on this computer.
type machine struct {
	disks []installer.Disk
	nics  []installer.NIC
	uefi  bool
}

func (m *machine) scan(ctx context.Context) error {
	_ = exec.CommandContext(ctx, "udevadm", "settle").Run()
	out, err := exec.CommandContext(ctx, "lsblk", installer.LsblkArgs...).Output()
	if err != nil {
		return fmt.Errorf("lsblk: %w", err)
	}
	m.disks, err = installer.ParseLsblk(out, installer.ByIDLinks("/dev/disk/by-id"))
	if err != nil {
		return err
	}
	m.nics = installer.ReadNICs("/sys/class/net")
	m.uefi = fileExists("/sys/firmware/efi")
	return nil
}

func (m *machine) print(w io.Writer) {
	mode := "BIOS (legacy)"
	if m.uefi {
		mode = "UEFI"
	}
	fmt.Fprintf(w, "Booted in %s mode.\n\nDisks:\n", mode)
	for _, d := range m.disks {
		fmt.Fprintf(w, "  %s\n      %s\n", d.Label(), d.State())
		if d.ByID != "" {
			fmt.Fprintf(w, "      %s\n", d.ByID)
		}
	}
	fmt.Fprintln(w, "\nNetwork ports:")
	for _, n := range m.nics {
		fmt.Fprintf(w, "  %s\n", n.Label())
	}
}

func (m *machine) configure(p *installer.Plan, target, squash, log string) {
	p.Target, p.Log = target, log
	if squash != "" {
		p.Squashfs = squash
	}
}

func printSteps(p *installer.Plan) {
	fmt.Println("\nSteps:")
	for i, s := range p.Steps() {
		fmt.Printf("%3d  %s\n", i+1, s)
	}
}

// install runs the plan, logging everything.
func install(ctx context.Context, p *installer.Plan, progress func(int, string)) error {
	f, err := os.OpenFile(p.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(f, "P8S OS installer — %s\n%s\n", time.Now().Format(time.RFC3339), p.Summary())
	if progress == nil {
		progress = func(pct int, title string) { fmt.Printf("[%3d%%] %s\n", pct, title) }
	}
	r := &installer.Runner{Plan: p, Log: f, Progress: progress}
	return r.Run(ctx)
}

// ---------------------------------------------------------------- menus

var errBack = errors.New("back")

type menus struct {
	m      *machine
	a      installer.Answers
	target string
	squash string
	log    string
}

const title = "P8S OS installer"

// wt runs whiptail and returns what it printed (the choice).
func wt(args ...string) (string, error) {
	var out strings.Builder
	c := exec.Command("whiptail", append([]string{"--title", title, "--backtitle", "P8S OS — hypervisor"}, args...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, &out
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return "", errBack
	}
	return strings.TrimSpace(out.String()), err
}

func msg(text string) { _, _ = wt("--msgbox", text, "20", "76") }

func (u *menus) run(ctx context.Context) {
	for {
		choice, err := wt("--nocancel", "--menu", "Welcome. P8S OS turns this computer into a server for virtual machines.\n\n"+
			"The installer erases the disks you choose. It asks first.", "16", "70", "4",
			"install", "Install P8S OS",
			"disks", "Show the disks and network ports",
			"shell", "Command line (for experts; type exit to come back)",
			"reboot", "Restart the computer")
		if err != nil {
			continue
		}
		switch choice {
		case "install":
			u.install(ctx)
		case "disks":
			var b strings.Builder
			u.m.print(&b)
			showText(b.String())
		case "shell":
			shell()
		case "reboot":
			_ = exec.Command("systemctl", "reboot").Run()
		}
	}
}

func shell() {
	c := exec.Command("bash", "-l")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Print("\033[H\033[2JCommand line. The installer comes back when you type exit.\n\n")
	_ = c.Run()
}

func showText(s string) {
	f, err := os.CreateTemp("", "p8s-*.txt")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	f.WriteString(s)
	f.Close()
	_, _ = wt("--scrolltext", "--textbox", f.Name(), "22", "78")
}

func (u *menus) install(ctx context.Context) {
	_ = u.m.scan(ctx) // a disk may have been plugged in since
	for {
		if err := u.ask(); err != nil {
			return // back to the welcome menu
		}
		p, err := installer.NewPlan(u.a, u.m.disks, u.m.nics, u.m.uefi, zoneOK)
		if err != nil {
			msg("Please fix this:\n\n" + err.Error())
			continue
		}
		u.m.configure(p, u.target, u.squash, u.log)
		if !u.confirm(p) {
			continue
		}
		u.progress(ctx, p)
		return
	}
}

// ask walks through the questions; the previous answers are the defaults.
func (u *menus) ask() error {
	a := &u.a
	// disks
	var items []string
	var blocked []string
	for _, d := range u.m.disks {
		if !d.Usable() {
			blocked = append(blocked, d.Name+": "+d.Why)
			continue
		}
		on := "OFF"
		for _, c := range a.Disks {
			if c == d.Name {
				on = "ON"
			}
		}
		items = append(items, d.Name, strings.TrimPrefix(d.Label(), d.Name)+"  ["+d.State()+"]", on)
	}
	if len(items) == 0 {
		msg("No disk can be used:\n\n" + strings.Join(blocked, "\n") + "\n\nThe installer needs a disk of at least 16 GB.")
		return errBack
	}
	text := "Choose the disk(s) for P8S OS with Space, then Enter. EVERYTHING ON THEM IS ERASED.\n" +
		"Disks that hold data say so. Pick two or more for a mirror or RAID."
	if len(blocked) > 0 {
		text += "\nNot offered: " + strings.Join(blocked, "; ")
	}
	out, err := wt(append([]string{"--separate-output", "--checklist", text, "22", "100", "10"}, items...)...)
	if err != nil {
		return err
	}
	a.Disks = strings.Fields(out)
	if len(a.Disks) == 0 {
		msg("Choose at least one disk (Space ticks it).")
		return u.ask()
	}
	// layout
	fits := installer.Fits(len(a.Disks))
	var li []string
	for _, l := range fits {
		li = append(li, string(l), l.Title())
	}
	def := string(fits[0])
	switch n := len(a.Disks); {
	case n == 2:
		def = string(installer.Mirror)
	case n == 3:
		def = string(installer.RAIDZ1)
	case n >= 4:
		def = string(installer.RAIDZ2)
	}
	if a.Layout != "" && a.Layout.Check(len(a.Disks)) == nil {
		def = string(a.Layout)
	}
	l, err := wt(append([]string{"--default-item", def, "--menu", fmt.Sprintf("How should the %d disk(s) be used?", len(a.Disks)), "16", "78", "7"}, li...)...)
	if err != nil {
		return err
	}
	a.Layout = installer.Layout(l)
	if a.Layout == installer.Ext4 {
		if a.RootGB == 0 {
			a.RootGB = 32
		}
		s, err := wt("--inputbox", "Size of the system partition in GB (the rest of the disk holds VMs):", "10", "70", strconv.Itoa(a.RootGB))
		if err != nil {
			return err
		}
		a.RootGB, _ = strconv.Atoi(s)
	}
	// name
	if a.Hostname == "" {
		a.Hostname = "p8s-node"
	}
	if a.Hostname, err = wt("--inputbox", "Server name (like p8s-node, or a full name like p8s1.example.com):", "10", "70", a.Hostname); err != nil {
		return err
	}
	if err := u.askNetwork(); err != nil {
		return err
	}
	if a.Timezone == "" {
		a.Timezone = "Etc/UTC"
	}
	if a.Timezone, err = wt("--inputbox", "Time zone (like Etc/UTC, Europe/Berlin, Asia/Kolkata, America/New_York):", "10", "70", a.Timezone); err != nil {
		return err
	}
	if a.Keyboard == "" {
		a.Keyboard = "us"
	}
	if a.Keyboard, err = wt("--inputbox", "Keyboard layout (us, gb, de, fr, es, it, in, ...):", "10", "70", a.Keyboard); err != nil {
		return err
	}
	// admin
	if a.User == "" {
		a.User = "admin"
	}
	if a.User, err = wt("--inputbox", "Admin user name. It gets sudo; logging in as root stays off.", "10", "70", a.User); err != nil {
		return err
	}
	for {
		p1, err := wt("--passwordbox", "Password for "+a.User+" (at least 8 characters):", "10", "70")
		if err != nil {
			return err
		}
		p2, err := wt("--passwordbox", "The same password again:", "10", "70")
		if err != nil {
			return err
		}
		if p1 != p2 {
			msg("The two passwords are different. Try again.")
			continue
		}
		a.Password = p1
		break
	}
	_, err = wt("--defaultno", "--yesno", "Encrypt the VM disks at rest?\n\nThe key is kept on the system disk, so the VM disks alone "+
		"(stolen, sent back, sold) can't be read. Back the key up after installing: Server page → Storage → Download the key.", "13", "74")
	a.Encrypt = err == nil
	a.Erase = false
	return nil
}

func (u *menus) askNetwork() error {
	n := &u.a.Network
	if len(u.m.nics) == 0 {
		msg("No network port found. Install anyway; set the network up later in /etc/network/interfaces.")
		n.Interface = "auto"
		return nil
	}
	if len(u.m.nics) == 1 {
		n.Interface = u.m.nics[0].Name
	} else {
		var items []string
		for _, c := range u.m.nics {
			items = append(items, c.Name, strings.TrimPrefix(c.Label(), c.Name+"  "))
		}
		def := u.m.nics[0].Name
		if n.Interface != "" && n.Interface != "auto" {
			def = n.Interface
		}
		v, err := wt(append([]string{"--default-item", def, "--menu", "Network port for the server (VMs use it too):", "16", "78", "6"}, items...)...)
		if err != nil {
			return err
		}
		n.Interface = v
	}
	for _, c := range u.m.nics {
		if c.Name == n.Interface && c.Wireless {
			var err error
			if n.WifiSSID, err = wt("--inputbox", "Wi-Fi name (a cable is better for a server):", "10", "70", n.WifiSSID); err != nil {
				return err
			}
			if n.WifiPassword, err = wt("--passwordbox", "Wi-Fi password (empty for an open network):", "10", "70"); err != nil {
				return err
			}
		}
	}
	def := "dhcp"
	if n.Mode == "static" {
		def = "static"
	}
	mode, err := wt("--default-item", def, "--menu", "Server address:", "12", "70", "2",
		"dhcp", "Automatic (DHCP) — fine to start; set a fixed one in the router",
		"static", "Fixed address (type it in)")
	if err != nil {
		return err
	}
	n.Mode = mode
	if mode == "static" {
		if n.Address, err = wt("--inputbox", "Address with network size (like 192.168.1.10/24):", "10", "70", n.Address); err != nil {
			return err
		}
		if n.Gateway, err = wt("--inputbox", "Gateway (your router, like 192.168.1.1):", "10", "70", n.Gateway); err != nil {
			return err
		}
		dns := strings.Join(n.DNS, " ")
		if dns == "" {
			dns = "1.1.1.1 8.8.8.8"
		}
		if dns, err = wt("--inputbox", "DNS servers (separated by spaces):", "10", "70", dns); err != nil {
			return err
		}
		n.DNS = strings.Fields(dns)
	}
	return nil
}

// confirm shows the summary and makes the person type ERASE.
func (u *menus) confirm(p *installer.Plan) bool {
	if _, err := wt("--yes-button", "Install", "--no-button", "Change", "--yesno", p.Summary(), "24", "100"); err != nil {
		return false
	}
	if data := p.HasData(); len(data) > 0 {
		var l []string
		for _, d := range data {
			l = append(l, d.Label()+"\n    "+d.State())
		}
		if _, err := wt("--defaultno", "--yes-button", "Erase them", "--no-button", "Change", "--yesno",
			"THESE DISKS ARE NOT EMPTY:\n\n"+strings.Join(l, "\n")+"\n\nEverything on them will be lost for good. Erase them?", "20", "100"); err != nil {
			return false
		}
	}
	var names []string
	for _, d := range p.Disks {
		names = append(names, d.Name)
	}
	v, err := wt("--inputbox", "Last check. Type ERASE (capital letters) to wipe "+strings.Join(names, ", ")+" and install:", "10", "74")
	if err != nil || v != "ERASE" {
		if err == nil {
			msg("Nothing was changed (you did not type ERASE).")
		}
		return false
	}
	p.A.Erase, u.a.Erase = true, true
	return true
}

// progress runs the install behind a progress bar.
func (u *menus) progress(ctx context.Context, p *installer.Plan) {
	g := exec.Command("whiptail", "--title", title, "--gauge", "Installing P8S OS ...", "8", "76", "0")
	g.Stdout, g.Stderr = os.Stdout, os.Stderr
	in, err := g.StdinPipe()
	if err == nil {
		err = g.Start()
	}
	w := bufio.NewWriter(in)
	upd := func(pct int, t string) {
		if err == nil {
			fmt.Fprintf(w, "XXX\n%d\n%s\nXXX\n", pct, t)
			w.Flush()
		}
	}
	ierr := install(ctx, p, upd)
	if in != nil {
		in.Close()
	}
	if err == nil {
		_ = g.Wait()
	}
	if ierr == nil {
		msg("P8S OS is installed.\n\nRemove the USB stick, then press Enter to restart.\n\n" +
			"On the first start it finishes setting up by itself (10-20 minutes, needs internet). Then log in as " + p.A.User + ".")
		_ = exec.Command("systemctl", "reboot").Run()
		return
	}
	for {
		c, e := wt("--nocancel", "--menu", "The install stopped:\n\n"+ierr.Error()+"\n\nThe log has the details.", "18", "78", "3",
			"log", "Show the log", "shell", "Command line", "menu", "Back to the start (fix it, then install again)")
		if e != nil {
			continue
		}
		switch c {
		case "log":
			b, _ := os.ReadFile(p.Log)
			s := string(b)
			if len(s) > 60000 {
				s = s[len(s)-60000:]
			}
			showText(s)
		case "shell":
			shell()
		case "menu":
			// the half-finished install is still mounted: clean up so the next try starts fresh
			_ = exec.Command("umount", "-Rl", p.Target).Run()
			_ = exec.Command("zpool", "export", "-a").Run()
			return
		}
	}
}
