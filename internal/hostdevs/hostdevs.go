// Package hostdevs lists the server's PCI and USB devices that can be given
// to a VM (passthrough), from /sys, with lspci names when lspci is there.
package hostdevs

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"p8sos/internal/execx"
)

// PCI is one PCI function.
type PCI struct {
	Addr   string `json:"addr"`             // 0000:01:00.0
	Class  string `json:"class"`            // 0300 (VGA), 0200 (Ethernet), 0c03 (USB controller), 0108 (NVMe)
	Vendor string `json:"vendor"`           // 10de
	Device string `json:"device"`           // 2484
	Name   string `json:"name"`             // "NVIDIA Corporation GA104 [GeForce RTX 3070]"
	Kind   string `json:"kind"`             // graphics, audio, network, storage, usb, other
	Driver string `json:"driver,omitempty"` // host driver now (vfio-pci while a VM has it)
	Group  int    `json:"group"`            // IOMMU group; -1 = IOMMU off
	// Others are the other devices in its IOMMU group: they go to the VM
	// together (all of them must be given, except PCI bridges).
	Others []string `json:"others,omitempty"`
	// Warn says why giving it away may hurt the server (its screen, its
	// network or disks); Bridge devices can't be given at all.
	Warn   string `json:"warn,omitempty"`
	Bridge bool   `json:"bridge,omitempty"`
}

// USB is one USB device (hubs left out).
type USB struct {
	ID   string `json:"id"` // vendor:product, 046d:c52b
	Name string `json:"name"`
	Port string `json:"port"` // e.g. 1-2 (where it is plugged in now)
}

// Info is what the dashboard shows.
type Info struct {
	IOMMU bool   `json:"iommu"` // the CPU's IOMMU is on (needed for PCI)
	CPU   string `json:"cpu"`   // intel / amd (for the IOMMU setting)
	PCI   []PCI  `json:"pci"`
	USB   []USB  `json:"usb"`
}

// Lister reads the devices. Root is "" for "/" (tests point it elsewhere).
type Lister struct {
	Root string
	Run  execx.Runner // lspci for names; nil = names from ids only
}

func (l *Lister) p(parts ...string) string {
	r := l.Root
	if r == "" {
		r = "/"
	}
	return filepath.Join(append([]string{r}, parts...)...)
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// List returns the devices.
func (l *Lister) List(ctx context.Context) Info {
	in := Info{PCI: []PCI{}, USB: []USB{}, CPU: l.cpu()}
	groups, _ := os.ReadDir(l.p("sys/kernel/iommu_groups"))
	in.IOMMU = len(groups) > 0
	names := l.lspci(ctx)
	ents, _ := os.ReadDir(l.p("sys/bus/pci/devices"))
	byGroup := map[int][]string{}
	for _, e := range ents {
		d := l.p("sys/bus/pci/devices", e.Name())
		class := strings.TrimPrefix(read(filepath.Join(d, "class")), "0x")
		if len(class) >= 4 {
			class = class[:4]
		}
		p := PCI{Addr: e.Name(), Class: class, Group: -1,
			Vendor: strings.TrimPrefix(read(filepath.Join(d, "vendor")), "0x"),
			Device: strings.TrimPrefix(read(filepath.Join(d, "device")), "0x")}
		if t, err := os.Readlink(filepath.Join(d, "driver")); err == nil {
			p.Driver = filepath.Base(t)
		}
		if t, err := os.Readlink(filepath.Join(d, "iommu_group")); err == nil {
			var g int
			if _, err := sscanInt(filepath.Base(t), &g); err == nil {
				p.Group = g
				byGroup[g] = append(byGroup[g], p.Addr)
			}
		}
		p.Name = names[p.Addr]
		if p.Name == "" {
			p.Name = "device " + p.Vendor + ":" + p.Device
		}
		p.Kind = kindOf(class)
		p.Bridge = strings.HasPrefix(class, "06")
		switch {
		case p.Bridge:
			p.Warn = "a bridge inside the server; it can't be given to a VM"
		case read(filepath.Join(d, "boot_vga")) == "1":
			p.Warn = "the server's own screen runs on it; after giving it away the server's monitor goes dark (the dashboard keeps working)"
		case hasEntries(filepath.Join(d, "net")) && p.Driver != "vfio-pci":
			p.Warn = "the server uses this network card; if it is the one to your network, the server goes offline"
		case strings.HasPrefix(class, "01") && p.Driver != "vfio-pci":
			p.Warn = "a disk controller of the server; make sure no server disk or pool is on it"
		}
		in.PCI = append(in.PCI, p)
	}
	for i := range in.PCI {
		p := &in.PCI[i]
		if p.Group < 0 {
			continue
		}
		for _, o := range byGroup[p.Group] {
			if o != p.Addr && !l.isBridge(o) {
				p.Others = append(p.Others, o)
			}
		}
	}
	sort.Slice(in.PCI, func(i, j int) bool { return in.PCI[i].Addr < in.PCI[j].Addr })
	in.USB = l.usb()
	return in
}

// Find returns one PCI device (ok=false if the server has none at addr).
func (l *Lister) Find(ctx context.Context, addr string) (PCI, Info, bool) {
	in := l.List(ctx)
	for _, p := range in.PCI {
		if p.Addr == addr {
			return p, in, true
		}
	}
	return PCI{}, in, false
}

func (l *Lister) isBridge(addr string) bool {
	return strings.HasPrefix(strings.TrimPrefix(read(l.p("sys/bus/pci/devices", addr, "class")), "0x"), "06")
}

func (l *Lister) usb() []USB {
	out := []USB{}
	ents, _ := os.ReadDir(l.p("sys/bus/usb/devices"))
	seen := map[string]bool{}
	for _, e := range ents {
		d := l.p("sys/bus/usb/devices", e.Name())
		v, pr := read(filepath.Join(d, "idVendor")), read(filepath.Join(d, "idProduct"))
		if v == "" || pr == "" || strings.Contains(e.Name(), ":") {
			continue // interfaces, not devices
		}
		if read(filepath.Join(d, "bDeviceClass")) == "09" || strings.HasPrefix(e.Name(), "usb") {
			continue // hubs and the root hubs
		}
		id := v + ":" + pr
		if seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(read(filepath.Join(d, "manufacturer")) + " " + read(filepath.Join(d, "product")))
		if name == "" {
			name = "USB device " + id
		}
		out = append(out, USB{ID: id, Name: name, Port: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func (l *Lister) cpu() string {
	f, err := os.Open(l.p("proc/cpuinfo"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "vendor_id"); ok {
			switch {
			case strings.Contains(v, "AMD"):
				return "amd"
			case strings.Contains(v, "Intel"):
				return "intel"
			}
		}
	}
	return ""
}

// lspci -Dmm: 0000:01:00.0 "VGA compatible controller" "NVIDIA Corporation" "GA104 [GeForce RTX 3070]" ...
func (l *Lister) lspci(ctx context.Context) map[string]string {
	m := map[string]string{}
	if l.Run == nil {
		return m
	}
	out, err := l.Run.Run(ctx, "lspci", "-Dmm")
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(out), "\n") {
		addr, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		f := quoted(rest)
		if len(f) >= 3 {
			m[addr] = strings.TrimSpace(f[1] + " " + f[2])
		}
	}
	return m
}

func quoted(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '"')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i+1:], '"')
		if j < 0 {
			return out
		}
		out = append(out, s[i+1:i+1+j])
		s = s[i+j+2:]
	}
}

func kindOf(class string) string {
	switch {
	case strings.HasPrefix(class, "03"):
		return "graphics"
	case strings.HasPrefix(class, "0403"):
		return "audio"
	case strings.HasPrefix(class, "02"):
		return "network"
	case strings.HasPrefix(class, "01"):
		return "storage"
	case strings.HasPrefix(class, "0c03"):
		return "usb"
	}
	return "other"
}

func hasEntries(dir string) bool {
	e, err := os.ReadDir(dir)
	return err == nil && len(e) > 0
}

func sscanInt(s string, v *int) (int, error) {
	n := 0
	if s == "" {
		return 0, os.ErrInvalid
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(c-'0')
	}
	*v = n
	return 1, nil
}
