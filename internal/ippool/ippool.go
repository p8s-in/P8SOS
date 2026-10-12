// Package ippool hands out public IPv4 addresses to VMs, the way a hosting
// provider does: the admin enters a block of addresses from the datacenter
// (e.g. 203.0.113.8/29 with gateway 203.0.113.9) and the bridge that block
// lives on; each new VM gets one free address, configured inside the VM by
// cloud-init (static, no DHCP). Pools and who has which address are kept in
// one JSON file.
package ippool

import (
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

// Pool is one block of addresses.
type Pool struct {
	Name    string   `json:"name"`
	Bridge  string   `json:"bridge"`  // host bridge VMs join, e.g. br0 (on the uplink) or vmbr1 (routed)
	CIDR    string   `json:"cidr"`    // the network, e.g. 203.0.113.8/29
	Gateway string   `json:"gateway"` // e.g. 203.0.113.9; may lie outside CIDR (routed /32 setups)
	DNS     []string `json:"dns"`
	Exclude []string `json:"exclude,omitempty"` // addresses not to hand out (used elsewhere)
	// Customers may pick this pool (admins always can).
	Customers bool `json:"customers"`
	// Used: address -> VM name.
	Used map[string]string `json:"used"`
}

// Lease is one VM's address with what the VM needs to configure it.
type Lease struct {
	Pool    string   `json:"pool"`
	IP      string   `json:"ip"`
	Prefix  int      `json:"prefix"` // e.g. 29
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
	Bridge  string   `json:"bridge"`
	OnLink  bool     `json:"on_link"` // gateway is outside the network (needs an on-link route)
}

// View is a pool as the API shows it.
type View struct {
	Pool
	Size int `json:"size"` // addresses that can be handed out
	Free int `json:"free"`
}

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrFull     = errors.New("no free address")
	ErrInUse    = errors.New("in use")

	reName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)
	reBridge = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
)

// MaxSize keeps pools to something one host can sensibly use (/22).
const MaxSize = 1024

type Store struct {
	Path string // e.g. /var/lib/p8s/ippools.json; empty = memory only
	mu   sync.Mutex
	mem  []Pool
}

func (s *Store) load() ([]Pool, error) {
	if s.Path == "" {
		return clone(s.mem), nil
	}
	var l []Pool
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return []Pool{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	for i := range l {
		if l[i].Used == nil {
			l[i].Used = map[string]string{}
		}
	}
	return l, nil
}

func clone(l []Pool) []Pool {
	out := make([]Pool, len(l))
	for i, p := range l {
		p.Used = func() map[string]string {
			m := map[string]string{}
			for k, v := range p.Used {
				m[k] = v
			}
			return m
		}()
		p.DNS = append([]string(nil), p.DNS...)
		p.Exclude = append([]string(nil), p.Exclude...)
		out[i] = p
	}
	return out
}

func (s *Store) save(l []Pool) error {
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	if s.Path == "" {
		s.mem = clone(l)
		return nil
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o750); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// hosts lists the addresses a pool can hand out, in order.
func hosts(p Pool) ([]netip.Addr, error) {
	pfx, err := netip.ParsePrefix(p.CIDR)
	if err != nil || !pfx.Addr().Is4() {
		return nil, fmt.Errorf("%w: network %q must be IPv4 like 203.0.113.8/29", ErrInvalid, p.CIDR)
	}
	pfx = pfx.Masked()
	if pfx.Bits() < 32-10 {
		return nil, fmt.Errorf("%w: network %s is too big (at most /22)", ErrInvalid, pfx)
	}
	skip := map[netip.Addr]bool{}
	if gw, err := netip.ParseAddr(p.Gateway); err == nil {
		skip[gw] = true
	}
	for _, e := range p.Exclude {
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("%w: excluded address %q", ErrInvalid, e)
		}
		skip[a] = true
	}
	var out []netip.Addr
	first, last := pfx.Addr(), lastAddr(pfx)
	for a := first; pfx.Contains(a); a = a.Next() {
		// network and broadcast addresses aren't usable on /30 and bigger
		if pfx.Bits() <= 30 && (a == first || a == last) {
			continue
		}
		if !skip[a] {
			out = append(out, a)
		}
		if a == last {
			break
		}
	}
	return out, nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	n |= (1 << (32 - p.Bits())) - 1
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// Validate checks a new pool and tidies it.
func Validate(p Pool) (Pool, error) {
	p.Name = strings.ToLower(strings.TrimSpace(p.Name))
	if !reName.MatchString(p.Name) {
		return p, fmt.Errorf("%w: pool name: 2-32 lowercase letters, digits, hyphens", ErrInvalid)
	}
	if !reBridge.MatchString(p.Bridge) {
		return p, fmt.Errorf("%w: bridge %q", ErrInvalid, p.Bridge)
	}
	pfx, err := netip.ParsePrefix(strings.TrimSpace(p.CIDR))
	if err != nil || !pfx.Addr().Is4() {
		return p, fmt.Errorf("%w: network %q must be IPv4 like 203.0.113.8/29", ErrInvalid, p.CIDR)
	}
	p.CIDR = pfx.Masked().String()
	gw, err := netip.ParseAddr(strings.TrimSpace(p.Gateway))
	if err != nil || !gw.Is4() {
		return p, fmt.Errorf("%w: gateway %q must be an IPv4 address", ErrInvalid, p.Gateway)
	}
	p.Gateway = gw.String()
	if len(p.DNS) == 0 {
		p.DNS = []string{"1.1.1.1", "8.8.8.8"}
	}
	var dns []string
	for _, d := range p.DNS {
		a, err := netip.ParseAddr(strings.TrimSpace(d))
		if err != nil {
			return p, fmt.Errorf("%w: DNS server %q", ErrInvalid, d)
		}
		dns = append(dns, a.String())
	}
	p.DNS = dns
	var ex []string
	for _, e := range p.Exclude {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil || !pfx.Contains(a) {
			return p, fmt.Errorf("%w: excluded address %q is not in %s", ErrInvalid, e, p.CIDR)
		}
		ex = append(ex, a.String())
	}
	p.Exclude = ex
	h, err := hosts(p)
	if err != nil {
		return p, err
	}
	if len(h) == 0 {
		return p, fmt.Errorf("%w: no usable addresses left in %s", ErrInvalid, p.CIDR)
	}
	if p.Used == nil {
		p.Used = map[string]string{}
	}
	return p, nil
}

func view(p Pool) View {
	h, _ := hosts(p)
	free := 0
	for _, a := range h {
		if _, used := p.Used[a.String()]; !used {
			free++
		}
	}
	return View{Pool: p, Size: len(h), Free: free}
}

// List returns all pools with their free counts.
func (s *Store) List() ([]View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	out := []View{}
	for _, p := range l {
		out = append(out, view(p))
	}
	return out, err
}

// Get returns one pool.
func (s *Store) Get(name string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return View{}, err
	}
	for _, p := range l {
		if p.Name == name {
			return view(p), nil
		}
	}
	return View{}, fmt.Errorf("%w: ip pool %q", ErrNotFound, name)
}

// Add creates a pool. Its network may not overlap another pool's.
func (s *Store) Add(p Pool) (View, error) {
	p, err := Validate(p)
	if err != nil {
		return View{}, err
	}
	p.Used = map[string]string{}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return View{}, err
	}
	np := netip.MustParsePrefix(p.CIDR)
	for _, o := range l {
		if o.Name == p.Name {
			return View{}, fmt.Errorf("%w: ip pool %q", ErrExists, p.Name)
		}
		if op, err := netip.ParsePrefix(o.CIDR); err == nil && op.Overlaps(np) {
			return View{}, fmt.Errorf("%w: %s overlaps pool %s (%s)", ErrInvalid, p.CIDR, o.Name, o.CIDR)
		}
	}
	if err := s.save(append(l, p)); err != nil {
		return View{}, err
	}
	return view(p), nil
}

// Update changes the parts of a pool that don't move addresses: DNS,
// excluded addresses (not ones in use), customers flag.
func (s *Store) Update(name string, dns, exclude []string, customers bool) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return View{}, err
	}
	for i, p := range l {
		if p.Name != name {
			continue
		}
		p.DNS, p.Exclude, p.Customers = dns, exclude, customers
		np, err := Validate(p)
		if err != nil {
			return View{}, err
		}
		for _, e := range np.Exclude {
			if vm, used := np.Used[e]; used {
				return View{}, fmt.Errorf("%w: %s is used by %s", ErrInUse, e, vm)
			}
		}
		l[i] = np
		if err := s.save(l); err != nil {
			return View{}, err
		}
		return view(np), nil
	}
	return View{}, fmt.Errorf("%w: ip pool %q", ErrNotFound, name)
}

// Delete removes an empty pool.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return err
	}
	for i, p := range l {
		if p.Name != name {
			continue
		}
		if len(p.Used) > 0 {
			var vms []string
			for _, v := range p.Used {
				vms = append(vms, v)
			}
			sort.Strings(vms)
			return fmt.Errorf("%w: pool %s still has VMs (%s)", ErrInUse, name, strings.Join(vms, ", "))
		}
		return s.save(append(l[:i], l[i+1:]...))
	}
	return fmt.Errorf("%w: ip pool %q", ErrNotFound, name)
}

func lease(p Pool, ip string) Lease {
	pfx := netip.MustParsePrefix(p.CIDR)
	gw, _ := netip.ParseAddr(p.Gateway)
	return Lease{Pool: p.Name, IP: ip, Prefix: pfx.Bits(), Gateway: p.Gateway, DNS: p.DNS, Bridge: p.Bridge, OnLink: !pfx.Contains(gw)}
}

// Allocate gives vm an address from the pool: prefer if it is free (a VM
// recreated from a backup gets its old address back), else the lowest free one.
func (s *Store) Allocate(pool, vm, prefer string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return Lease{}, err
	}
	for _, p := range l {
		for ip, v := range p.Used {
			if v == vm {
				return Lease{}, fmt.Errorf("%w: %s already has %s", ErrExists, vm, ip)
			}
		}
	}
	for i, p := range l {
		if p.Name != pool {
			continue
		}
		h, err := hosts(p)
		if err != nil {
			return Lease{}, err
		}
		pick := ""
		for _, a := range h {
			if _, used := p.Used[a.String()]; used {
				continue
			}
			if a.String() == prefer {
				pick = prefer
				break
			}
			if pick == "" {
				pick = a.String()
			}
		}
		if pick == "" {
			return Lease{}, fmt.Errorf("%w in pool %s (%d of %d used)", ErrFull, pool, len(p.Used), len(h))
		}
		l[i].Used[pick] = vm
		if err := s.save(l); err != nil {
			return Lease{}, err
		}
		return lease(p, pick), nil
	}
	return Lease{}, fmt.Errorf("%w: ip pool %q", ErrNotFound, pool)
}

// Release frees vm's address (no error if it had none).
func (s *Store) Release(vm string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return err
	}
	changed := false
	for _, p := range l {
		for ip, v := range p.Used {
			if v == vm {
				delete(p.Used, ip)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return s.save(l)
}

// Lookup returns vm's address, if it has one.
func (s *Store) Lookup(vm string) (Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return Lease{}, false
	}
	for _, p := range l {
		for ip, v := range p.Used {
			if v == vm {
				return lease(p, ip), true
			}
		}
	}
	return Lease{}, false
}

// All returns every VM's lease (VM name -> lease).
func (s *Store) All() map[string]Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Lease{}
	l, _ := s.load()
	for _, p := range l {
		for ip, v := range p.Used {
			out[v] = lease(p, ip)
		}
	}
	return out
}
