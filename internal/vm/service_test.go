package vm

import (
	"context"
	"errors"
	"sync"
	"testing"

	"p8sos/internal/cloudinit"
	"p8sos/internal/config"
	"p8sos/internal/virt"
)

type fakeHV struct {
	mu        sync.Mutex
	doms      map[string][]byte
	state     map[string]virt.State
	failStart bool
}

func newHV() *fakeHV { return &fakeHV{doms: map[string][]byte{}, state: map[string]virt.State{}} }

func (h *fakeHV) Define(_ context.Context, x []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, err := virt.ParseDomainXML(x)
	if err != nil {
		return err
	}
	h.doms[s.Name], h.state[s.Name] = x, virt.Stopped
	return nil
}
func (h *fakeHV) set(n string, st virt.State) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.doms[n]; !ok {
		return virt.ErrNoDomain
	}
	h.state[n] = st
	return nil
}
func (h *fakeHV) Start(_ context.Context, n string) error {
	if h.failStart {
		return errors.New("qemu: could not open disk")
	}
	return h.set(n, virt.Running)
}
func (h *fakeHV) Shutdown(_ context.Context, n string) error { return h.set(n, virt.Stopping) }
func (h *fakeHV) Destroy(_ context.Context, n string) error  { return h.set(n, virt.Stopped) }
func (h *fakeHV) Undefine(_ context.Context, n string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.doms[n]; !ok {
		return virt.ErrNoDomain
	}
	delete(h.doms, n)
	delete(h.state, n)
	return nil
}
func (h *fakeHV) State(_ context.Context, n string) (virt.State, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.state[n]
	if !ok {
		return virt.Unknown, virt.ErrNoDomain
	}
	return st, nil
}
func (h *fakeHV) XML(_ context.Context, n string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	x, ok := h.doms[n]
	if !ok {
		return nil, virt.ErrNoDomain
	}
	return x, nil
}
func (h *fakeHV) List(context.Context) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{"handmade-vm"} // must be ignored (no prefix)
	for n := range h.doms {
		out = append(out, n)
	}
	return out, nil
}

type fakeDisks struct{ live map[string]bool }

func (d *fakeDisks) CloneDisk(_ context.Context, _, vm string, _ int) (string, error) {
	d.live[vm] = true
	return "/dev/zvol/tank/vms/" + vm + "-disk0", nil
}
func (d *fakeDisks) DestroyDisk(_ context.Context, vm string) error { delete(d.live, vm); return nil }
func (d *fakeDisks) SnapshotExists(context.Context, string) bool    { return true }

type fakeSeeds struct{ live map[string]bool }

func (s *fakeSeeds) Build(_ context.Context, vm string, _ cloudinit.Spec) (string, error) {
	s.live[vm] = true
	return "/var/lib/p8s/seeds/" + vm + "-seed.iso", nil
}
func (s *fakeSeeds) Remove(vm string) error { delete(s.live, vm); return nil }

func newSvc() (*Service, *fakeHV, *fakeDisks, *fakeSeeds) {
	hv, d, s := newHV(), &fakeDisks{live: map[string]bool{}}, &fakeSeeds{live: map[string]bool{}}
	return &Service{
		HV: hv, Disks: d, Seeds: s, Bridge: "vmbr0",
		Templates: []config.Template{{Name: "debian-13", Snapshot: "tank/t/debian-13@base", MinDiskGB: 4}},
		Limits:    Limits{MaxVCPUs: 8, MaxMemoryMiB: 16384, MaxDiskGB: 500},
	}, hv, d, s
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, _, disks, seeds := newSvc()

	v, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 20,
		SSHKeys: []string{"ssh-ed25519 AAAA test"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "web1" || v.State != virt.Running || v.VCPUs != 2 || v.MemoryMiB != 2048 || v.DiskGB != 20 || v.Template != "debian-13" {
		t.Fatalf("created %+v", v)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	list, _ := svc.List(ctx)
	if len(list) != 1 || list[0].Name != "web1" {
		t.Fatalf("list %+v", list)
	}
	if v, _ = svc.Stop(ctx, "web1", true); v.State != virt.Stopped {
		t.Fatalf("stop: %s", v.State)
	}
	if v, _ = svc.Start(ctx, "web1"); v.State != virt.Running {
		t.Fatalf("start: %s", v.State)
	}
	if err := svc.Delete(ctx, "web1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, "web1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if len(disks.live) != 0 || len(seeds.live) != 0 {
		t.Fatalf("leaked disks=%v seeds=%v", disks.live, seeds.live)
	}
}

func TestCreateDefaults(t *testing.T) {
	svc, _, _, _ := newSvc()
	v, err := svc.Create(context.Background(), CreateRequest{Name: "db1", Template: "debian-13"})
	if err != nil {
		t.Fatal(err)
	}
	if v.VCPUs != 1 || v.MemoryMiB != 1024 || v.DiskGB != 4 {
		t.Fatalf("defaults %+v", v)
	}
}

func TestCreateRollsBackOnStartFailure(t *testing.T) {
	svc, hv, disks, seeds := newSvc()
	hv.failStart = true
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "bad1", Template: "debian-13"}); err == nil {
		t.Fatal("expected error")
	}
	if len(hv.doms) != 0 || len(disks.live) != 0 || len(seeds.live) != 0 {
		t.Fatalf("rollback incomplete: doms=%d disks=%v seeds=%v", len(hv.doms), disks.live, seeds.live)
	}
}

func TestValidation(t *testing.T) {
	svc, _, _, _ := newSvc()
	for _, r := range []CreateRequest{
		{Name: "Bad_Name", Template: "debian-13"},
		{Name: "../etc", Template: "debian-13"},
		{Name: "ok1", Template: "nope"},
		{Name: "ok1", Template: "debian-13", VCPUs: 99},
		{Name: "ok1", Template: "debian-13", DiskGB: 2},
		{Name: "ok1", Template: "debian-13", SSHKeys: []string{"not a key"}},
		{Name: "ok1", Template: "debian-13", UserData: "#!/bin/sh"},
	} {
		if _, err := svc.Create(context.Background(), r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", r, err)
		}
	}
}

func TestOpsOnMissingVM(t *testing.T) {
	svc, _, _, _ := newSvc()
	ctx := context.Background()
	if _, err := svc.Start(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
	if _, err := svc.Stop(ctx, "ghost", false); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
	if err := svc.Delete(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
}
