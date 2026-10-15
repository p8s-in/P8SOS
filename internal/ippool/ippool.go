// Package ippool hands out public IPv4 addresses to VMs, the way a hosting
// provider does: the admin enters a block of addresses from the datacenter
// (e.g. 203.0.113.8/29 with gateway 203.0.113.9) and the bridge that block
// lives on; each new VM gets one free address, configured inside the VM by
// cloud-init (static, no DHCP). Pools and who has which address are kept in
// one JSON file.
//
// A pool can also have an IPv6 network: every VM from it then gets one IPv6
// address too. A VM can have extra IPv4 addresses from its pool, and its
// address can be changed later (Change).
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
	// Used: address -> VM name (each VM's main address).
	Used map[string]string `json:"used"`
	// Extra: additional IPv4 addresses -> VM name.
	Extra map[string]string `json:"extra,omitempty"`
	// IPv6: the pool's IPv6 network (e.g. 2001:db8:5::/64) and gateway (an
	// address in it, or a link-local one like fe80::1); "" = IPv4 only.
	CIDR6    string            `json:"cidr6,omitempty"`
	Gateway6 string            `json:"gateway6,omitempty"`
	Used6    map[string]string `json:"used6,omitempty"` // IPv6 address -> VM name
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
	// Extra IPv4 addresses (same network and gateway).
	Extra []string `json:"extra,omitempty"`
	// IPv6 address, its network's prefix length and the gateway ("" = none).
	IP6      string `json:"ip6,omitempty"`
	Prefix6  int    `json:"prefix6,omitempty"`
	Gateway6 string `json:"gateway6,omitempty"`
}

// MaxExtra is how many extra IPv4 addresses one VM can have.
const MaxExtra = 8

// Addrs4 are all the lease's IPv4 addresses (main first).
func (l Lease) Addrs4() []string { return append([]string{l.IP}, l.Extra...) }

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
		fill(&l[i])
	}
	return l, nil
}

func fill(p *Pool) {
	if p.Used == nil {
		p.Used = map[string]string{}
	}
	if p.Extra == nil {
		p.Extra = map[string]string{}
	}
	if p.Used6 == nil {
		p.Used6 = map[string]string{}
	}
}

func copyMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func clone(l []Pool) []Pool {
	out := make([]Pool, len(l))
	for i, p := range l {
		p.Used, p.Extra, p.Used6 = copyMap(p.Used), copyMap(p.Extra), copyMap(p.Used6)
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
	p.CIDR6, p.Gateway6 = strings.TrimSpace(p.CIDR6), strings.TrimSpace(p.Gateway6)
	if p.CIDR6 != "" || p.Gateway6 != "" {
		p6, err := netip.ParsePrefix(p.CIDR6)
		if err != nil || !p6.Addr().Is6() || p6.Addr().Is4In6() || p6.Bits() > 120 || p6.Bits() < 32 {
			return p, fmt.Errorf("%w: IPv6 network %q must look like 2001:db8:5::/64", ErrInvalid, p.CIDR6)
		}
		p6 = p6.Masked()
		p.CIDR6 = p6.String()
		g6, err := netip.ParseAddr(p.Gateway6)
		if err != nil || !g6.Is6() || g6.Is4In6() || (!p6.Contains(g6) && !g6.IsLinkLocalUnicast()) {
			return p, fmt.Errorf("%w: IPv6 gateway %q must be in %s or link-local (fe80::…)", ErrInvalid, p.Gateway6, p.CIDR6)
		}
		p.Gateway6 = g6.String()
	}
	fill(&p)
	return p, nil
}

// hosts6First is the first IPv6 host number handed out (::100 and up; the low
// ones are usually the router's).
const hosts6First = 0x100

// nextFree6 picks a free IPv6 address in the pool's IPv6 network.
func nextFree6(p Pool) (string, error) {
	p6, err := netip.ParsePrefix(p.CIDR6)
	if err != nil {
		return "", fmt.Errorf("%w: pool %s has no IPv6 network", ErrInvalid, p.Name)
	}
	a := p6.Addr().Next() // not the network's ::0
	if p6.Bits() <= 112 { // big networks: from ::100 (the low ones are usually the router's)
		b := p6.Addr().As16()
		b[14], b[15] = byte(hosts6First>>8), byte(hosts6First&0xff)
		a = netip.AddrFrom16(b)
	}
	for i := 0; i < 4*MaxSize && p6.Contains(a); i, a = i+1, a.Next() {
		if _, used := p.Used6[a.String()]; used || a.String() == p.Gateway6 {
			continue
		}
		return a.String(), nil
	}
	return "", fmt.Errorf("%w for IPv6 in pool %s", ErrFull, p.Name)
}

// taken: an IPv4 address of the pool is someone's (main or extra).
func taken(p Pool, ip string) bool {
	_, a := p.Used[ip]
	_, b := p.Extra[ip]
	return a || b
}

func view(p Pool) View {
	h, _ := hosts(p)
	free := 0
	for _, a := range h {
		if !taken(p, a.String()) {
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
	p.Used, p.Extra, p.Used6 = map[string]string{}, map[string]string{}, map[string]string{}
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
	return s.UpdateV6(name, dns, exclude, customers, nil)
}

// V6 sets a pool's IPv6 network (both "" = none).
type V6 struct{ CIDR6, Gateway6 string }

// UpdateV6 is Update that can also set the IPv6 network (v6 != nil; only
// while no VM has an IPv6 address from it).
func (s *Store) UpdateV6(name string, dns, exclude []string, customers bool, v6 *V6) (View, error) {
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
		if v6 != nil && (v6.CIDR6 != p.CIDR6 || v6.Gateway6 != p.Gateway6) {
			if len(p.Used6) > 0 {
				return View{}, fmt.Errorf("%w: %d VM(s) have IPv6 addresses from %s", ErrInUse, len(p.Used6), p.CIDR6)
			}
			p.CIDR6, p.Gateway6 = v6.CIDR6, v6.Gateway6
		}
		np, err := Validate(p)
		if err != nil {
			return View{}, err
		}
		for _, e := range np.Exclude {
			if vm, used := np.Used[e]; used {
				return View{}, fmt.Errorf("%w: %s is used by %s", ErrInUse, e, vm)
			}
			if vm, used := np.Extra[e]; used {
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
	l := Lease{Pool: p.Name, IP: ip, Prefix: pfx.Bits(), Gateway: p.Gateway, DNS: p.DNS, Bridge: p.Bridge, OnLink: !pfx.Contains(gw)}
	vm := p.Used[ip]
	for x, v := range p.Extra {
		if v == vm {
			l.Extra = append(l.Extra, x)
		}
	}
	sort.Slice(l.Extra, func(i, j int) bool {
		a, _ := netip.ParseAddr(l.Extra[i])
		b, _ := netip.ParseAddr(l.Extra[j])
		return a.Less(b)
	})
	for x, v := range p.Used6 {
		if v == vm {
			p6 := netip.MustParsePrefix(p.CIDR6)
			l.IP6, l.Prefix6, l.Gateway6 = x, p6.Bits(), p.Gateway6
		}
	}
	return l
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
			if taken(p, a.String()) {
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
		if p.CIDR6 != "" {
			ip6, err := nextFree6(l[i])
			if err != nil {
				return Lease{}, err
			}
			l[i].Used6[ip6] = vm
		}
		if err := s.save(l); err != nil {
			return Lease{}, err
		}
		return lease(l[i], pick), nil
	}
	return Lease{}, fmt.Errorf("%w: ip pool %q", ErrNotFound, pool)
}

// find is the pool index and main address of vm's lease (-1 = none).
func find(l []Pool, vm string) (int, string) {
	for i, p := range l {
		for ip, v := range p.Used {
			if v == vm {
				return i, ip
			}
		}
	}
	return -1, ""
}

// AddExtra gives vm one more IPv4 address from the pool of its main one
// (want = that address if free, "" = the lowest free).
func (s *Store) AddExtra(vm, want string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return Lease{}, err
	}
	i, main := find(l, vm)
	if i < 0 {
		return Lease{}, fmt.Errorf("%w: %s has no public address", ErrNotFound, vm)
	}
	p := l[i]
	if n := len(lease(p, main).Extra); n >= MaxExtra {
		return Lease{}, fmt.Errorf("%w: %s has %d extra addresses already (at most %d)", ErrInvalid, vm, n, MaxExtra)
	}
	h, err := hosts(p)
	if err != nil {
		return Lease{}, err
	}
	pick := ""
	for _, a := range h {
		if taken(p, a.String()) {
			continue
		}
		if want == "" || a.String() == want {
			pick = a.String()
			break
		}
	}
	if pick == "" {
		if want != "" {
			return Lease{}, fmt.Errorf("%w: %s isn't a free address of pool %s", ErrInvalid, want, p.Name)
		}
		return Lease{}, fmt.Errorf("%w in pool %s", ErrFull, p.Name)
	}
	l[i].Extra[pick] = vm
	if err := s.save(l); err != nil {
		return Lease{}, err
	}
	return lease(l[i], main), nil
}

// RemoveExtra takes an extra address back from vm.
func (s *Store) RemoveExtra(vm, ip string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return Lease{}, err
	}
	i, main := find(l, vm)
	if i < 0 {
		return Lease{}, fmt.Errorf("%w: %s has no public address", ErrNotFound, vm)
	}
	if ip == main {
		return Lease{}, fmt.Errorf("%w: %s is %s's main address; change it instead", ErrInvalid, ip, vm)
	}
	if l[i].Extra[ip] != vm {
		return Lease{}, fmt.Errorf("%w: %s isn't an extra address of %s", ErrNotFound, ip, vm)
	}
	delete(l[i].Extra, ip)
	if err := s.save(l); err != nil {
		return Lease{}, err
	}
	return lease(l[i], main), nil
}

// AddV6 gives vm an IPv6 address (its pool must have an IPv6 network).
func (s *Store) AddV6(vm string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return Lease{}, err
	}
	i, main := find(l, vm)
	if i < 0 {
		return Lease{}, fmt.Errorf("%w: %s has no public address", ErrNotFound, vm)
	}
	if lease(l[i], main).IP6 != "" {
		return Lease{}, fmt.Errorf("%w: %s has an IPv6 address already", ErrExists, vm)
	}
	ip6, err := nextFree6(l[i])
	if err != nil {
		return Lease{}, err
	}
	l[i].Used6[ip6] = vm
	if err := s.save(l); err != nil {
		return Lease{}, err
	}
	return lease(l[i], main), nil
}

// Change gives vm a new main address: from pool (its own when ""), want if
// free ("" = the lowest free). Extra addresses stay when the pool stays (and
// are given back otherwise); the IPv6 address stays in the same pool and is
// renewed in another one that has IPv6. Returns the old and the new lease.
func (s *Store) Change(vm, pool, want string) (Lease, Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return Lease{}, Lease{}, err
	}
	i, main := find(l, vm)
	if i < 0 {
		return Lease{}, Lease{}, fmt.Errorf("%w: %s has no public address", ErrNotFound, vm)
	}
	old := lease(l[i], main)
	if pool == "" {
		pool = l[i].Name
	}
	j := -1
	for k := range l {
		if l[k].Name == pool {
			j = k
		}
	}
	if j < 0 {
		return Lease{}, Lease{}, fmt.Errorf("%w: ip pool %q", ErrNotFound, pool)
	}
	if want == main && i == j {
		return Lease{}, Lease{}, fmt.Errorf("%w: %s has %s already", ErrInvalid, vm, want)
	}
	h, err := hosts(l[j])
	if err != nil {
		return Lease{}, Lease{}, err
	}
	pick := ""
	for _, a := range h {
		ip := a.String()
		if taken(l[j], ip) && !(i == j && l[j].Extra[ip] == vm && ip == want) {
			continue
		}
		if want == "" || ip == want {
			pick = ip
			break
		}
	}
	if pick == "" {
		if want != "" {
			return Lease{}, Lease{}, fmt.Errorf("%w: %s isn't a free address of pool %s", ErrInvalid, want, pool)
		}
		return Lease{}, Lease{}, fmt.Errorf("%w in pool %s", ErrFull, pool)
	}
	delete(l[i].Used, main)
	delete(l[j].Extra, pick) // an extra address promoted to the main one
	l[j].Used[pick] = vm
	if i != j {
		for x, v := range l[i].Extra {
			if v == vm {
				delete(l[i].Extra, x)
			}
		}
		for x, v := range l[i].Used6 {
			if v == vm {
				delete(l[i].Used6, x)
			}
		}
		if l[j].CIDR6 != "" {
			ip6, err := nextFree6(l[j])
			if err != nil {
				return Lease{}, Lease{}, err
			}
			l[j].Used6[ip6] = vm
		}
	}
	if err := s.save(l); err != nil {
		return Lease{}, Lease{}, err
	}
	return old, lease(l[j], pick), nil
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
		for _, m := range []map[string]string{p.Used, p.Extra, p.Used6} {
			for ip, v := range m {
				if v == vm {
					delete(m, ip)
					changed = true
				}
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
