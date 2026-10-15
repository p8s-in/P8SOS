package installer

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
)

// Answers are everything the installer asks. The menus fill them in; for an
// unattended install they come from a JSON file (p8s-install --answers FILE):
//
//	{"disks": ["sdb", "sdc"], "layout": "mirror", "hostname": "p8s1",
//	 "user": "admin", "password": "...", "timezone": "Europe/Berlin",
//	 "network": {"interface": "auto", "mode": "dhcp"}, "erase": true}
type Answers struct {
	Disks    []string `json:"disks"`              // kernel names (sdb), /dev paths or /dev/disk/by-id paths
	Layout   Layout   `json:"layout"`             // single, mirror, raid10, raidz1, raidz2, raidz3, ext4
	Hostname string   `json:"hostname"`           // p8s-node, or a full name: p8s1.example.com
	Timezone string   `json:"timezone,omitempty"` // Etc/UTC
	Keyboard string   `json:"keyboard,omitempty"` // us, de, fr, gb, ...
	User     string   `json:"user"`               // the admin account (gets sudo; root login stays off)
	Password string   `json:"password"`           // at least 8 characters
	Network  Network  `json:"network"`
	Encrypt  bool     `json:"encrypt_vm_storage"` // VM disks encrypted at rest (key on the system disk)
	// EncryptSystem: the whole ZFS pool (system and VM disks) is encrypted with
	// Passphrase, asked at every start (ZFS layouts only).
	EncryptSystem bool   `json:"encrypt_system,omitempty"`
	Passphrase    string `json:"passphrase,omitempty"`
	RootGB        int    `json:"root_gb,omitempty"` // ext4 only: system partition size (default 32)
	Erase         bool   `json:"erase"`             // must be true: "yes, wipe the disks above"
}

// Network is the server's network setup.
type Network struct {
	Interface    string   `json:"interface,omitempty"` // "auto" (default) or a port name: enp3s0
	Mode         string   `json:"mode,omitempty"`      // dhcp (default) or static
	Address      string   `json:"address,omitempty"`   // static: 192.168.1.10/24
	Gateway      string   `json:"gateway,omitempty"`   // static: 192.168.1.1
	DNS          []string `json:"dns,omitempty"`       // static: default 1.1.1.1, 8.8.8.8
	WifiSSID     string   `json:"wifi_ssid,omitempty"`
	WifiPassword string   `json:"wifi_password,omitempty"`
}

// ReadAnswers loads an answers file.
func ReadAnswers(path string) (Answers, error) {
	var a Answers
	b, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

var (
	reLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reUser  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reKeys  = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]+)?$`)
	reZone  = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+){0,2}$`)
)

// Reserved account names the admin user can't take.
var reservedUsers = map[string]bool{"root": true, "daemon": true, "bin": true, "sys": true, "sync": true,
	"games": true, "man": true, "lp": true, "mail": true, "news": true, "uucp": true, "proxy": true,
	"www-data": true, "backup": true, "list": true, "irc": true, "nobody": true, "systemd-network": true,
	"messagebus": true, "sshd": true, "libvirt-qemu": true, "libvirt-dnsmasq": true, "dnsmasq": true,
	"tss": true, "polkitd": true, "_apt": true, "p8s": true}

// ErrNotErase is returned when the answers don't say the disks may be wiped.
var ErrNotErase = errors.New(`the answers must say "erase": true (every disk listed is wiped)`)

// DefaultHostname is a name no other P8S server on the network has yet, like
// p8s-3fa2: servers that all keep the same default name can't be told apart in
// a cluster (and can't join one).
func DefaultHostname() string {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "p8s-node"
	}
	return fmt.Sprintf("p8s-%x", b)
}

// Check validates and fills in defaults. zoneOK reports whether a time zone
// exists (nil: only the format is checked).
func (a *Answers) Check(zoneOK func(string) bool) error {
	a.Hostname = strings.ToLower(strings.TrimSpace(a.Hostname))
	if a.Hostname == "" {
		a.Hostname = DefaultHostname()
	}
	if len(a.Hostname) > 253 {
		return fmt.Errorf("hostname: too long")
	}
	for _, l := range strings.Split(a.Hostname, ".") {
		if !reLabel.MatchString(l) {
			return fmt.Errorf("hostname %q: use letters, digits and '-' (like p8s-node or p8s1.example.com)", a.Hostname)
		}
	}
	if a.Layout == "" {
		a.Layout = Single
		if len(a.Disks) > 1 {
			a.Layout = Mirror
		}
	}
	if len(a.Disks) == 0 {
		return fmt.Errorf("no disk chosen")
	}
	if err := a.Layout.Check(len(a.Disks)); err != nil {
		return err
	}
	if a.Timezone == "" {
		a.Timezone = "Etc/UTC"
	}
	if !reZone.MatchString(a.Timezone) || strings.Contains(a.Timezone, "..") || (zoneOK != nil && !zoneOK(a.Timezone)) {
		return fmt.Errorf("time zone %q not found (like Etc/UTC, Europe/Berlin, Asia/Kolkata)", a.Timezone)
	}
	if a.Keyboard == "" {
		a.Keyboard = "us"
	}
	if !reKeys.MatchString(a.Keyboard) {
		return fmt.Errorf("keyboard %q: use a layout code like us, de, fr, gb", a.Keyboard)
	}
	a.User = strings.TrimSpace(a.User)
	if !reUser.MatchString(a.User) {
		return fmt.Errorf("user name %q: lower-case letters, digits, '-' and '_', starting with a letter", a.User)
	}
	if reservedUsers[a.User] {
		return fmt.Errorf("user name %q is taken by the system; pick another (like admin or your name)", a.User)
	}
	if len(a.Password) < 8 {
		return fmt.Errorf("password: at least 8 characters")
	}
	if strings.ContainsAny(a.Password, "\n\r") {
		return fmt.Errorf("password: no line breaks")
	}
	if a.EncryptSystem {
		if a.Layout == Ext4 {
			return fmt.Errorf("encrypting the whole system needs a ZFS layout (not ext4); with ext4 encrypt the VM disks only")
		}
		if len(a.Passphrase) < 8 || len(a.Passphrase) > 512 {
			return fmt.Errorf("system passphrase: at least 8 characters")
		}
		if strings.ContainsAny(a.Passphrase, "\n\r") {
			return fmt.Errorf("system passphrase: no line breaks")
		}
		a.Encrypt = false // the VM disks are inside the encrypted pool already
	} else {
		a.Passphrase = ""
	}
	if a.Layout == Ext4 {
		if a.RootGB == 0 {
			a.RootGB = 32
		}
		if a.RootGB < 16 || a.RootGB > 4096 {
			return fmt.Errorf("system partition: 16 to 4096 GB")
		}
	} else {
		a.RootGB = 0
	}
	return a.Network.check()
}

func (n *Network) check() error {
	if n.Interface == "" {
		n.Interface = "auto"
	}
	if n.Mode == "" {
		n.Mode = "dhcp"
	}
	if n.Interface != "auto" && !regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`).MatchString(n.Interface) {
		return fmt.Errorf("network port %q: not a port name", n.Interface)
	}
	if n.WifiSSID != "" && (len(n.WifiSSID) > 32 || strings.ContainsAny(n.WifiSSID, "\n\r\"")) {
		return fmt.Errorf("Wi-Fi name: up to 32 characters, no quotes")
	}
	if n.WifiPassword != "" && (len(n.WifiPassword) < 8 || len(n.WifiPassword) > 63 || strings.ContainsAny(n.WifiPassword, "\n\r\"")) {
		return fmt.Errorf("Wi-Fi password: 8 to 63 characters, no quotes")
	}
	switch n.Mode {
	case "dhcp":
		n.Address, n.Gateway, n.DNS = "", "", nil
		return nil
	case "static":
	default:
		return fmt.Errorf("network mode %q: use dhcp or static", n.Mode)
	}
	p, err := netip.ParsePrefix(strings.TrimSpace(n.Address))
	if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 30 {
		return fmt.Errorf("address %q: write it with the network size, like 192.168.1.10/24", n.Address)
	}
	if p.Addr() == p.Masked().Addr() {
		return fmt.Errorf("address %s is the network itself; use a host address", p)
	}
	n.Address = p.String()
	gw, err := netip.ParseAddr(strings.TrimSpace(n.Gateway))
	if err != nil || !gw.Is4() {
		return fmt.Errorf("gateway %q: the router's address, like 192.168.1.1", n.Gateway)
	}
	if !p.Masked().Contains(gw) {
		return fmt.Errorf("gateway %s is not in %s", gw, p.Masked())
	}
	if gw == p.Addr() {
		return fmt.Errorf("gateway can't be the server's own address")
	}
	n.Gateway = gw.String()
	if len(n.DNS) == 0 {
		n.DNS = []string{"1.1.1.1", "8.8.8.8"}
	}
	for i, d := range n.DNS {
		a, err := netip.ParseAddr(strings.TrimSpace(d))
		if err != nil {
			return fmt.Errorf("DNS server %q: an IP address, like 1.1.1.1", d)
		}
		n.DNS[i] = a.String()
	}
	return nil
}

// Short is the host name without the domain.
func (a Answers) Short() string { return strings.SplitN(a.Hostname, ".", 2)[0] }
