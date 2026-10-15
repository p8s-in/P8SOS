package ippool

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestPools(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "ippools.json")}
	bad := []Pool{
		{Name: "x", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9"},    // name too short
		{Name: "pub", Bridge: "br 0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9"}, // bridge
		{Name: "pub", Bridge: "br0", CIDR: "2001:db8::/64", Gateway: "2001:db8::1"},   // v6 not yet
		{Name: "pub", Bridge: "br0", CIDR: "10.0.0.0/16", Gateway: "10.0.0.1"},        // too big
		{Name: "pub", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "nope"},         // gateway
		{Name: "pub", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9", DNS: []string{"x"}},
		{Name: "pub", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9", Exclude: []string{"198.51.100.1"}},
	}
	for _, p := range bad {
		if _, err := s.Add(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %+v (%v)", p, err)
		}
	}
	v, err := s.Add(Pool{Name: "pub", Bridge: "br0", CIDR: "203.0.113.10/29", Gateway: "203.0.113.9", Exclude: []string{"203.0.113.10"}, Customers: true})
	if err != nil {
		t.Fatal(err)
	}
	// /29 = 8 addresses: minus network .8, broadcast .15, gateway .9, excluded .10 -> 4
	if v.CIDR != "203.0.113.8/29" || v.Size != 4 || v.Free != 4 || len(v.DNS) != 2 {
		t.Fatalf("pool %+v", v)
	}
	if _, err := s.Add(Pool{Name: "pub2", Bridge: "br0", CIDR: "203.0.113.12/30", Gateway: "203.0.113.13"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("overlapping pool accepted")
	}
	if _, err := s.Add(Pool{Name: "pub", Bridge: "br0", CIDR: "198.51.100.0/30", Gateway: "198.51.100.1"}); !errors.Is(err, ErrExists) {
		t.Fatal("duplicate name accepted")
	}

	got := []string{}
	for _, vm := range []string{"a", "b", "c", "d"} {
		l, err := s.Allocate("pub", vm, "")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, l.IP)
		if l.Prefix != 29 || l.Gateway != "203.0.113.9" || l.Bridge != "br0" || l.OnLink {
			t.Fatalf("lease %+v", l)
		}
	}
	if want := []string{"203.0.113.11", "203.0.113.12", "203.0.113.13", "203.0.113.14"}; !equal(got, want) {
		t.Fatalf("handed out %v, want %v", got, want)
	}
	if _, err := s.Allocate("pub", "e", ""); !errors.Is(err, ErrFull) {
		t.Fatal("full pool handed out an address")
	}
	if _, err := s.Allocate("pub", "a", ""); !errors.Is(err, ErrExists) {
		t.Fatal("second address for the same VM")
	}
	if l, ok := s.Lookup("c"); !ok || l.IP != "203.0.113.13" {
		t.Fatalf("lookup %+v", l)
	}
	if err := s.Release("b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("nobody"); err != nil {
		t.Fatal(err)
	}
	// a recreated VM gets its old address back when it's free
	if l, err := s.Allocate("pub", "b2", "203.0.113.12"); err != nil || l.IP != "203.0.113.12" {
		t.Fatalf("preferred address: %+v %v", l, err)
	}
	if err := s.Delete("pub"); !errors.Is(err, ErrInUse) {
		t.Fatal("deleted a pool with VMs")
	}
	if _, err := s.Update("pub", []string{"9.9.9.9"}, []string{"203.0.113.11"}, false); !errors.Is(err, ErrInUse) {
		t.Fatal("excluded an address in use")
	}
	if v, err := s.Update("pub", []string{"9.9.9.9"}, nil, false); err != nil || v.DNS[0] != "9.9.9.9" || v.Customers {
		t.Fatalf("update %+v %v", v, err)
	}
	// reload from disk
	s2 := &Store{Path: s.Path}
	if all := s2.All(); len(all) != 4 || all["d"].IP != "203.0.113.14" {
		t.Fatalf("reloaded %+v", all)
	}
	for _, vm := range []string{"a", "b2", "c", "d"} {
		_ = s2.Release(vm)
	}
	if err := s2.Delete("pub"); err != nil {
		t.Fatal(err)
	}

	// routed single address with the gateway outside (Hetzner-style /32)
	if _, err := s2.Add(Pool{Name: "one", Bridge: "vmbr1", CIDR: "198.51.100.7/32", Gateway: "172.31.1.1"}); err != nil {
		t.Fatal(err)
	}
	l, err := s2.Allocate("one", "x", "")
	if err != nil || l.IP != "198.51.100.7" || l.Prefix != 32 || !l.OnLink {
		t.Fatalf("/32 lease %+v %v", l, err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExtrasV6Change(t *testing.T) {
	s := &Store{}
	if _, err := s.Add(Pool{Name: "dc1", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9",
		CIDR6: "2001:db8:5::/64", Gateway6: "fe80::1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Pool{Name: "dc2", Bridge: "br0", CIDR: "198.51.100.0/29", Gateway: "198.51.100.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Pool{Name: "bad", Bridge: "br0", CIDR: "192.0.2.0/29", Gateway: "192.0.2.1", CIDR6: "2001:db8:9::/64", Gateway6: "2001:db8:1::1"}); err == nil {
		t.Fatal("gateway6 outside the network accepted")
	}
	l, err := s.Allocate("dc1", "web", "")
	if err != nil || l.IP != "203.0.113.10" || l.IP6 != "2001:db8:5::100" || l.Prefix6 != 64 || l.Gateway6 != "fe80::1" {
		t.Fatalf("%+v %v", l, err)
	}
	l2, _ := s.Allocate("dc1", "db", "")
	if l2.IP != "203.0.113.11" || l2.IP6 != "2001:db8:5::101" {
		t.Fatalf("%+v", l2)
	}
	// extras
	l, err = s.AddExtra("web", "")
	if err != nil || len(l.Extra) != 1 || l.Extra[0] != "203.0.113.12" {
		t.Fatalf("%+v %v", l, err)
	}
	l, _ = s.AddExtra("web", "203.0.113.14")
	if got := l.Addrs4(); len(got) != 3 || got[2] != "203.0.113.14" {
		t.Fatalf("%v", got)
	}
	if _, err := s.AddExtra("web", "203.0.113.11"); err == nil {
		t.Fatal("someone else's address given")
	}
	if v, _ := s.Get("dc1"); v.Free != 1 { // .10 .11 .12 .14 used of .10-.14
		t.Fatalf("free %d", v.Free)
	}
	// the next VM doesn't get an extra address
	if l3, _ := s.Allocate("dc1", "mail", ""); l3.IP != "203.0.113.13" {
		t.Fatalf("%+v", l3)
	}
	s.Release("mail")
	if _, err := s.RemoveExtra("web", "203.0.113.10"); err == nil {
		t.Fatal("main address removed as extra")
	}
	l, err = s.RemoveExtra("web", "203.0.113.12")
	if err != nil || len(l.Extra) != 1 {
		t.Fatalf("%+v %v", l, err)
	}
	// change in the same pool: extras and IPv6 stay
	old, nw, err := s.Change("web", "", "203.0.113.13")
	if err != nil || old.IP != "203.0.113.10" || nw.IP != "203.0.113.13" || nw.IP6 != "2001:db8:5::100" || len(nw.Extra) != 1 {
		t.Fatalf("%+v %+v %v", old, nw, err)
	}
	// promote the extra address
	_, nw, err = s.Change("web", "", "203.0.113.14")
	if err != nil || nw.IP != "203.0.113.14" || len(nw.Extra) != 0 {
		t.Fatalf("%+v %v", nw, err)
	}
	// to another pool (no IPv6 there): extras and IPv6 are given back
	s.AddExtra("web", "")
	_, nw, err = s.Change("web", "dc2", "")
	if err != nil || nw.Pool != "dc2" || nw.IP6 != "" || len(nw.Extra) != 0 {
		t.Fatalf("%+v %v", nw, err)
	}
	if v, _ := s.Get("dc1"); len(v.Used6) != 1 || len(v.Extra) != 0 {
		t.Fatalf("left behind %+v", v.Pool)
	}
	// IPv6 later: set on dc2, then AddV6
	if _, err := s.UpdateV6("dc2", nil, nil, false, &V6{CIDR6: "2001:db8:7::/120", Gateway6: "2001:db8:7::1"}); err != nil {
		t.Fatal(err)
	}
	l, err = s.AddV6("web")
	if err != nil || l.IP6 != "2001:db8:7::2" {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := s.UpdateV6("dc2", nil, nil, false, &V6{}); err == nil {
		t.Fatal("IPv6 network removed while in use")
	}
	s.Release("web")
	if v, _ := s.Get("dc2"); len(v.Used6)+len(v.Used)+len(v.Extra) != 0 {
		t.Fatalf("not released %+v", v.Pool)
	}
}
