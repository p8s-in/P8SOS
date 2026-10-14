package tasks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"p8sos/internal/execx"
)

// Node gathers what the dashboard's Server page shows.
type Node struct {
	Tasks     *Manager
	Run       execx.Runner
	Pool      string          // ZFS pool, e.g. "tank"
	DataDir   string          // /var/lib/p8s
	Templates func() []string // installed template names
	// CTTemplates lists installed container templates; nil = containers off.
	CTTemplates func() []string
	LAN         func() bool // home-network bridge is up
	LANBridge   string      // e.g. br0
	SysNet      string      // /sys/class/net (tests)
	ProcBonding string      // /proc/net/bonding (tests)

	imgMu   sync.Mutex
	imgAt   time.Time
	imgList []CTImage
}

// CTImage is a container image on the image server (images.linuxcontainers.org).
type CTImage struct {
	ID        string `json:"id"`    // DIST/RELEASE, e.g. kali/current
	Label     string `json:"label"` // e.g. "Kali current"
	Installed bool   `json:"installed"`
}

// CTImages lists every container image the image server offers (cached for an hour).
func (n *Node) CTImages(ctx context.Context) ([]CTImage, error) {
	n.imgMu.Lock()
	defer n.imgMu.Unlock()
	if n.imgList == nil || time.Since(n.imgAt) > time.Hour {
		b, err := n.Run.Run(ctx, "/bin/bash", filepath.Join(n.Tasks.Scripts, "make-ct-template.sh"), "--available")
		if err != nil {
			return nil, err
		}
		var l []CTImage
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) >= 2 && reImage.MatchString(f[0]) {
				l = append(l, CTImage{ID: f[0], Label: strings.Join(f[1:], " ")})
			}
		}
		if len(l) == 0 {
			return nil, errors.New("the image server listed no images")
		}
		n.imgList, n.imgAt = l, time.Now()
	}
	have := map[string]bool{}
	if n.CTTemplates != nil {
		for _, t := range n.CTTemplates() {
			have[t] = true
		}
	}
	out := make([]CTImage, len(n.imgList))
	for i, im := range n.imgList {
		im.Installed = have[CTImageName(im.ID)]
		out[i] = im
	}
	return out, nil
}

// ctShort is make-ct-template.sh's short list: DIST/RELEASE → its name there.
var ctShort = map[string]string{
	"debian/trixie": "debian-13", "debian/bookworm": "debian-12", "ubuntu/noble": "ubuntu-24.04", "ubuntu/jammy": "ubuntu-22.04",
	"alpine/3.24": "alpine-3.24", "rockylinux/9": "rocky-9", "almalinux/9": "alma-9", "fedora/44": "fedora-44", "archlinux/current": "archlinux",
}

// CTImageName is the template name make-ct-template.sh --image gives DIST/RELEASE.
func CTImageName(id string) string {
	if n, ok := ctShort[id]; ok {
		return n
	}
	s := strings.ToLower(strings.Replace(id, "/", "-", 1))
	s = strings.ReplaceAll(s, "_", "-")
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
	}
	s = b.String()
	if len(s) > 31 {
		s = s[:31]
	}
	return s
}

// Port is a physical network port.
type Port struct {
	Name    string `json:"name"`
	Carrier bool   `json:"carrier"`          // cable plugged in, link up
	Speed   int    `json:"speed,omitempty"`  // Mbit/s
	In      string `json:"in,omitempty"`     // bridge or bond it is part of
	Active  bool   `json:"active,omitempty"` // bond: carries the traffic now
	Wifi    bool   `json:"wifi,omitempty"`
}

// Network is the Server page's network part.
type Network struct {
	Bridge   string `json:"bridge,omitempty"`    // home-network bridge
	Uplink   string `json:"uplink,omitempty"`    // what the bridge sits on (a port or bond0)
	BondMode string `json:"bond_mode,omitempty"` // when the uplink is a bond
	Ports    []Port `json:"ports"`
	IPv6NAT  string `json:"ipv6_nat,omitempty"` // the private network's IPv6 range when on
	HostIPv6 bool   `json:"host_ipv6"`          // the server has IPv6 internet
}

// Storage is the VM pool.
type Storage struct {
	Pool       string `json:"pool"`
	Health     string `json:"health"`
	Size       int64  `json:"size"`
	Used       int64  `json:"used"`
	Free       int64  `json:"free"`
	File       bool   `json:"file"`        // pool lives in a file on the system disk (can grow)
	SystemFree int64  `json:"system_free"` // free space on the system disk
	CanGrow    int64  `json:"can_grow"`    // about how much "Grow" would add
	Error      string `json:"error,omitempty"`
}

// CatalogEntry is an OS image that can be added as a template.
type CatalogEntry struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
}

// OSUpdates is the last Debian update check (written by os-upgrade.sh).
type OSUpdates struct {
	Checked        string `json:"checked,omitempty"`
	Upgradable     int    `json:"upgradable"`
	Security       int    `json:"security"`
	RebootRequired bool   `json:"reboot_required"`
	Kernel         string `json:"kernel,omitempty"`
}

// Overview is the Server page.
type Overview struct {
	Storage    Storage        `json:"storage"`
	Catalog    []CatalogEntry `json:"catalog"`
	CTCatalog  []CatalogEntry `json:"ct_catalog"` // empty when containers are off
	Containers bool           `json:"containers"` // LXC installed
	LAN        bool           `json:"lan"`
	Network    Network        `json:"network"`
	OS         OSUpdates      `json:"os"`
	Running    *Task          `json:"running,omitempty"`
	Tasks      []Task         `json:"tasks"`
	Hostname   string         `json:"hostname"`
}

const gib = 1 << 30

func (n *Node) storage(ctx context.Context) Storage {
	s := Storage{Pool: n.Pool}
	out, err := n.Run.Run(ctx, "zpool", "list", "-Hp", "-o", "name,size,alloc,free,health", n.Pool)
	if err != nil {
		s.Error = "the VM storage pool could not be read"
		return s
	}
	f := strings.Fields(string(out))
	if len(f) >= 5 {
		s.Size, _ = strconv.ParseInt(f[1], 10, 64)
		s.Used, _ = strconv.ParseInt(f[2], 10, 64)
		s.Free, _ = strconv.ParseInt(f[3], 10, 64)
		s.Health = f[4]
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(n.DataDir, &fs) == nil {
		s.SystemFree = int64(fs.Bavail) * fs.Bsize
		img := filepath.Join(n.DataDir, n.Pool+".img")
		if st, err := os.Stat(img); err == nil {
			s.File = true
			sys := st.Sys().(*syscall.Stat_t)
			used := sys.Blocks * 512
			total := int64(fs.Blocks) * fs.Bsize
			reserve := total / 10
			if reserve < 20*gib {
				reserve = 20 * gib
			}
			target := (used + s.SystemFree - reserve) / gib * gib
			if target > st.Size() {
				s.CanGrow = target - st.Size()
			}
		}
	}
	return s
}

func (n *Node) catalog(ctx context.Context, script string, installed func() []string) []CatalogEntry {
	have := map[string]bool{}
	if installed != nil {
		for _, t := range installed() {
			have[t] = true
		}
	}
	out := []CatalogEntry{}
	b, err := n.Run.Run(ctx, "/bin/bash", filepath.Join(n.Tasks.Scripts, script), "--list")
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && reArg.MatchString(f[0]) {
			out = append(out, CatalogEntry{Name: f[0], Label: strings.Join(f[1:], " "), Installed: have[f[0]]})
		}
	}
	return out
}

// Overview collects everything for the Server page.
func (n *Node) Overview(ctx context.Context) Overview {
	o := Overview{Storage: n.storage(ctx), Catalog: n.catalog(ctx, "make-template.sh", n.Templates), Tasks: n.Tasks.List(ctx),
		CTCatalog: []CatalogEntry{}}
	if n.CTTemplates != nil {
		o.Containers = true
		o.CTCatalog = n.catalog(ctx, "make-ct-template.sh", n.CTTemplates)
	}
	if n.LAN != nil {
		o.LAN = n.LAN()
	}
	o.Network = n.network()
	if b, err := os.ReadFile(filepath.Join(n.DataDir, "os-updates.json")); err == nil {
		_ = json.Unmarshal(b, &o.OS)
	}
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		o.OS.RebootRequired = true
	}
	for i := range o.Tasks {
		if o.Tasks[i].State == "running" {
			t := o.Tasks[i]
			o.Running = &t
			break
		}
	}
	if len(o.Tasks) > 15 {
		o.Tasks = o.Tasks[:15]
	}
	o.Hostname, _ = os.Hostname()
	return o
}

func (n *Node) sysNet() string {
	if n.SysNet != "" {
		return n.SysNet
	}
	return "/sys/class/net"
}

func readStr(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }

// network reads the ports, the bridge's uplink and the bond from sysfs.
func (n *Node) network() Network {
	net := Network{Bridge: n.LANBridge, Ports: []Port{}}
	sys := n.sysNet()
	if n.LANBridge != "" {
		if ents, err := os.ReadDir(filepath.Join(sys, n.LANBridge, "brif")); err == nil {
			for _, e := range ents {
				nm := e.Name()
				if !strings.HasPrefix(nm, "vnet") && !strings.HasPrefix(nm, "tap") && !strings.HasPrefix(nm, "veth") && !strings.HasPrefix(nm, "vc") {
					net.Uplink = nm
					break
				}
			}
		}
	}
	active := ""
	if net.Uplink != "" {
		pb := n.ProcBonding
		if pb == "" {
			pb = "/proc/net/bonding"
		}
		if b, err := os.ReadFile(filepath.Join(pb, net.Uplink)); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(l, "Bonding Mode: "); ok {
					net.BondMode = strings.TrimSpace(v)
				}
				if v, ok := strings.CutPrefix(l, "Currently Active Slave: "); ok {
					active = strings.TrimSpace(v)
				}
			}
		}
	}
	ents, _ := os.ReadDir(sys)
	for _, e := range ents {
		nm := e.Name()
		if _, err := os.Stat(filepath.Join(sys, nm, "device")); err != nil {
			continue // virtual
		}
		p := Port{Name: nm, Carrier: readStr(filepath.Join(sys, nm, "carrier")) == "1", Active: nm == active}
		if s, err := strconv.Atoi(readStr(filepath.Join(sys, nm, "speed"))); err == nil && s > 0 {
			p.Speed = s
		}
		if _, err := os.Stat(filepath.Join(sys, nm, "wireless")); err == nil {
			p.Wifi = true
		}
		if l, err := os.Readlink(filepath.Join(sys, nm, "master")); err == nil {
			p.In = filepath.Base(l)
		}
		net.Ports = append(net.Ports, p)
	}
	if b, err := os.ReadFile(filepath.Join(n.DataDir, "ipv6-nat")); err == nil {
		net.IPv6NAT = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/net/ipv6_route"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			// default route: destination ::/0 with a next hop
			if len(f) >= 10 && f[0] == "00000000000000000000000000000000" && f[1] == "00" && f[4] != "00000000000000000000000000000000" && f[9] != "lo" {
				net.HostIPv6 = true
			}
		}
	}
	return net
}
