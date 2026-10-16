package ippool

import (
	"errors"
	"path/filepath"
	"testing"
)

// fakeLeader is the master's Store reached directly.
type fakeLeader struct {
	m    *Store
	file []byte
	on   bool
	down bool
}

func (f *fakeLeader) Following() bool              { return f.on }
func (f *fakeLeader) MasterPools() ([]byte, error) { return f.file, nil }
func (f *fakeLeader) Apply(op Op) (Result, error) {
	if f.down {
		return Result{}, errors.New("master can't be reached")
	}
	r := f.m.Do(op)
	f.file = r.File
	return r, nil
}

func TestFollowMaster(t *testing.T) {
	master := &Store{Path: filepath.Join(t.TempDir(), "m.json")}
	if _, err := master.Add(Pool{Name: "dc1", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := master.Allocate("dc1", "m-vm", ""); err != nil { // .10
		t.Fatal(err)
	}
	// the worker had its own pool and a VM on it before it joined
	worker := &Store{Path: filepath.Join(t.TempDir(), "w.json")}
	if _, err := worker.Add(Pool{Name: "old", Bridge: "br0", CIDR: "198.51.100.0/29", Gateway: "198.51.100.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Allocate("old", "w-old", ""); err != nil {
		t.Fatal(err)
	}
	f := &fakeLeader{m: master, on: true}
	f.file = master.Do(Op{Kind: "noop"}).File // what the book pushes
	worker.Leader = f

	// a new VM on the worker: the master hands out the address
	l, err := worker.Allocate("dc1", "w-vm", "")
	if err != nil || l.IP != "203.0.113.11" {
		t.Fatalf("%+v %v", l, err)
	}
	if ml, ok := master.Lookup("w-vm"); !ok || ml.IP != l.IP {
		t.Fatal("not on the master")
	}
	if wl, ok := worker.Lookup("w-vm"); !ok || wl.IP != l.IP {
		t.Fatal("worker doesn't see it")
	}
	// the old lease still works, on the worker only
	if ol, ok := worker.Lookup("w-old"); !ok || ol.Pool != "old" {
		t.Fatal("old lease lost")
	}
	if _, ok := master.Lookup("w-old"); ok {
		t.Fatal("old lease went to the master")
	}
	if all := worker.All(); len(all) != 3 {
		t.Fatalf("%v", all)
	}
	vs, _ := worker.List()
	if len(vs) != 2 {
		t.Fatalf("%+v", vs)
	}
	// changes go to the master
	if l, err := worker.AddExtra("w-vm", ""); err != nil || len(l.Extra) != 1 {
		t.Fatalf("%+v %v", l, err)
	}
	if _, nl, err := worker.Change("w-vm", "", "203.0.113.14"); err != nil || nl.IP != "203.0.113.14" {
		t.Fatalf("%+v %v", nl, err)
	}
	if _, err := worker.Allocate("dc1", "m-vm", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("error kind lost: %v", err)
	}
	if _, err := worker.Allocate("nope", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// managing the master's pools only on the master; the old one stays the worker's
	if _, err := worker.Add(Pool{Name: "dc2", Bridge: "br0", CIDR: "192.0.2.0/29", Gateway: "192.0.2.1"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := worker.Delete("dc1"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := worker.Update("old", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	// master down: new addresses can't be handed out; reads still work
	f.down = true
	if _, err := worker.Allocate("dc1", "w-2", ""); err == nil {
		t.Fatal("allocated without the master")
	}
	if _, ok := worker.Lookup("w-vm"); !ok {
		t.Fatal("read while the master is down")
	}
	f.down = false
	if err := worker.Release("w-vm"); err != nil {
		t.Fatal(err)
	}
	if _, ok := master.Lookup("w-vm"); ok {
		t.Fatal("not released on the master")
	}
	if err := worker.Release("w-old"); err != nil {
		t.Fatal(err)
	}
	if _, ok := worker.Lookup("w-old"); ok {
		t.Fatal("old lease not released")
	}
	// a master's Store refuses to be driven by another follower
	if r := worker.Do(Op{Kind: OpRelease, VM: "x"}); r.ErrKind != "invalid" {
		t.Fatalf("%+v", r)
	}
	// not following any more (it left): its own file again
	f.on = false
	if _, ok := worker.Lookup("m-vm"); ok {
		t.Fatal("still the master's view")
	}
}

func TestAdoptOnLeave(t *testing.T) {
	master := &Store{Path: filepath.Join(t.TempDir(), "m.json")}
	master.Add(Pool{Name: "dc1", Bridge: "br0", CIDR: "203.0.113.8/29", Gateway: "203.0.113.9"})
	master.Allocate("dc1", "m-vm", "")
	worker := &Store{Path: filepath.Join(t.TempDir(), "w.json")}
	f := &fakeLeader{m: master, on: true}
	f.file = master.Do(Op{}).File
	worker.Leader = f
	l, _ := worker.Allocate("dc1", "w-vm", "")
	worker.AddExtra("w-vm", "")
	n, err := worker.Adopt(func(v string) bool { return v == "w-vm" })
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	f.on = false // it left
	got, ok := worker.Lookup("w-vm")
	if !ok || got.IP != l.IP || len(got.Extra) != 1 || got.Bridge != "br0" {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := worker.Lookup("m-vm"); ok {
		t.Fatal("the master's VM came along")
	}
}
