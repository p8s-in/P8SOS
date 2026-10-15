// Package sdn is P8S OS's software-defined networking (like Proxmox SDN's
// VXLAN zone): virtual networks ("vnets") that span every server of the
// cluster. VMs on the same vnet are on one switch wherever they run, so they
// keep talking to each other when they move between servers.
//
// Each vnet is a VXLAN (UDP 4789) carried inside the cluster's WireGuard
// tunnel: on every server a VXLAN device vxl<VNI> plugged into a bridge
// vxb<VNI>, with the other servers' tunnel addresses as flood targets (no
// multicast needed). Frames are 1230 bytes (the tunnel's 1280 minus 50), so
// VM cards on a vnet get MTU 1230.
//
// A vnet may have a subnet: then the master (or a server on its own) is its
// gateway — an address on the bridge, DHCP and DNS (dnsmasq) and NAT to the
// internet. Without a subnet the vnet is a plain switch (static addresses).
package sdn

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ErrInvalid is a bad setting.
var ErrInvalid = errors.New("invalid")

const (
	// MTU of VM cards on a vnet (the tunnel's 1280 minus VXLAN's 50).
	MTU = 1230
	// MaxVNets keeps it to a sane number of devices per server.
	MaxVNets = 64
	// Prefix is how VMs name a vnet as their network ("vnet:office").
	Prefix = "vnet:"
	// Port is the VXLAN UDP port (inside the tunnel).
	Port = 4789
)

// VNet is one virtual network.
type VNet struct {
	Name    string `json:"name"`             // office, db-net …
	VNI     int    `json:"vni"`              // VXLAN id (picked when 0)
	Subnet  string `json:"subnet,omitempty"` // 10.60.0.0/24: the master is gateway + DHCP + NAT
	NAT     *bool  `json:"nat,omitempty"`    // with a subnet: reach the internet (default yes)
	Comment string `json:"comment,omitempty"`
}

// Config is every vnet.
type Config struct {
	VNets []VNet `json:"vnets"`
}

var reName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

// Bridge is the bridge VMs of a vnet plug into.
func Bridge(vni int) string { return fmt.Sprintf("vxb%d", vni) }

// Dev is the VXLAN device of a vnet.
func Dev(vni int) string { return fmt.Sprintf("vxl%d", vni) }

// VNIOf reads a vnet bridge's VNI (0: not a vnet bridge).
func VNIOf(bridge string) int {
	if !strings.HasPrefix(bridge, "vxb") {
		return 0
	}
	n := 0
	for _, c := range bridge[3:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1<<24 {
			return 0
		}
	}
	return n
}

// NATOn says whether a vnet with a subnet reaches the internet.
func (v VNet) NATOn() bool { return v.Subnet != "" && (v.NAT == nil || *v.NAT) }

// Plan is a subnet's addresses: the gateway and the DHCP range.
type Plan struct {
	Prefix  netip.Prefix
	Gateway netip.Addr
	First   netip.Addr
	Last    netip.Addr
}

func addrAdd(a netip.Addr, n uint32) netip.Addr {
	b := a.As4()
	v := binary.BigEndian.Uint32(b[:]) + n
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// PlanOf works out the gateway (first address) and DHCP range (from the
// 10th address, or the 2nd in small subnets, to the last before broadcast).
func PlanOf(subnet string) (Plan, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil || !p.Addr().Is4() {
		return Plan{}, fmt.Errorf("%q isn't an IPv4 network like 10.60.0.0/24", subnet)
	}
	if p.Bits() < 16 || p.Bits() > 28 {
		return Plan{}, fmt.Errorf("%s: use a /16 … /28 network", subnet)
	}
	if p.Masked() != p {
		return Plan{}, fmt.Errorf("%s isn't the start of its network (did you mean %s?)", subnet, p.Masked())
	}
	if !p.Addr().IsPrivate() {
		return Plan{}, fmt.Errorf("%s isn't a private network (10.x, 172.16-31.x, 192.168.x)", subnet)
	}
	size := uint32(1) << (32 - p.Bits())
	pl := Plan{Prefix: p, Gateway: addrAdd(p.Addr(), 1), Last: addrAdd(p.Addr(), size-2)}
	if size >= 64 {
		pl.First = addrAdd(p.Addr(), 10)
	} else {
		pl.First = addrAdd(p.Addr(), 2)
	}
	return pl, nil
}

// Reserved networks a vnet must not overlap (the cluster tunnel, libvirt's NAT).
var Reserved = []string{"10.250.0.0/24", "192.168.122.0/24"}

// Validate checks and tidies the vnets (picking VNIs for new ones).
func Validate(c Config) (Config, error) {
	out := Config{VNets: []VNet{}}
	if len(c.VNets) > MaxVNets {
		return out, fmt.Errorf("%w: at most %d virtual networks", ErrInvalid, MaxVNets)
	}
	names, vnis := map[string]bool{}, map[int]bool{}
	var nets []netip.Prefix
	for _, r := range Reserved {
		nets = append(nets, netip.MustParsePrefix(r))
	}
	for _, v := range c.VNets {
		if v.VNI != 0 {
			vnis[v.VNI] = true
		}
	}
	next := 10000
	for _, v := range c.VNets {
		v.Name = strings.TrimSpace(v.Name)
		v.Subnet = strings.TrimSpace(v.Subnet)
		v.Comment = strings.TrimSpace(v.Comment)
		if !reName.MatchString(v.Name) {
			return out, fmt.Errorf("%w: name %q: lowercase letters, digits and -, starting with a letter (at most 20)", ErrInvalid, v.Name)
		}
		if names[v.Name] {
			return out, fmt.Errorf("%w: two networks called %s", ErrInvalid, v.Name)
		}
		names[v.Name] = true
		if len(v.Comment) > 200 || strings.ContainsAny(v.Comment, "\r\n") {
			return out, fmt.Errorf("%w: %s: comment too long", ErrInvalid, v.Name)
		}
		if v.VNI == 0 {
			for vnis[next] {
				next++
			}
			v.VNI = next
			vnis[next] = true
		}
		if v.VNI < 1 || v.VNI > 9999999 { // vxb + 7 digits stays within 15 characters
			return out, fmt.Errorf("%w: %s: VNI must be 1-9999999", ErrInvalid, v.Name)
		}
		if v.Subnet != "" {
			pl, err := PlanOf(v.Subnet)
			if err != nil {
				return out, fmt.Errorf("%w: %s: %v", ErrInvalid, v.Name, err)
			}
			for _, n := range nets {
				if n.Overlaps(pl.Prefix) {
					return out, fmt.Errorf("%w: %s: %s overlaps %s", ErrInvalid, v.Name, pl.Prefix, n)
				}
			}
			nets = append(nets, pl.Prefix)
			v.Subnet = pl.Prefix.String()
		} else {
			v.NAT = nil
		}
		out.VNets = append(out.VNets, v)
	}
	seen := map[int]string{}
	for _, v := range out.VNets {
		if o, ok := seen[v.VNI]; ok {
			return out, fmt.Errorf("%w: %s and %s have the same VNI %d", ErrInvalid, o, v.Name, v.VNI)
		}
		seen[v.VNI] = v.Name
	}
	sort.Slice(out.VNets, func(i, j int) bool { return out.VNets[i].Name < out.VNets[j].Name })
	return out, nil
}

// Find returns the vnet called name.
func (c Config) Find(name string) (VNet, bool) {
	for _, v := range c.VNets {
		if v.Name == name {
			return v, true
		}
	}
	return VNet{}, false
}

// Store keeps the vnets in a JSON file.
type Store struct {
	Path string
	mu   sync.Mutex
}

// Load reads them (none when there is no file).
func (s *Store) Load() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return loadFile(s.Path)
}

func loadFile(path string) Config {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &c) != nil {
		return Config{VNets: []VNet{}}
	}
	if v, err := Validate(c); err == nil {
		return v
	}
	return Config{VNets: []VNet{}}
}

// Save validates and writes them.
func (s *Store) Save(c Config) (Config, error) {
	v, err := Validate(c)
	if err != nil {
		return v, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return v, writeFile(s.Path, v)
}

func writeFile(path string, c Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Adopt takes the master's copy (from the cluster book) when it differs;
// it reports whether anything changed.
func (s *Store) Adopt(masterCopy string) (bool, error) {
	b, err := os.ReadFile(masterCopy)
	if err != nil {
		return false, nil
	}
	var c Config
	if json.Unmarshal(b, &c) != nil {
		return false, nil
	}
	v, err := Validate(c)
	if err != nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := loadFile(s.Path)
	a, _ := json.Marshal(cur)
	n, _ := json.Marshal(v)
	if string(a) == string(n) {
		return false, nil
	}
	return true, writeFile(s.Path, v)
}
