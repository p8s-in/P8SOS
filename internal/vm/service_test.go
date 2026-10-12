package vm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/cloudinit"
	"p8sos/internal/config"
	"p8sos/internal/virt"
)

type fakeHV struct {
	mu          sync.Mutex
	doms        map[string][]byte
	state       map[string]virt.State
	failStart   bool
	noAgent     bool
	pw          string
	agentWarmup int
	frozen      int
	thawed      int
	auto        map[string]bool
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
func (h *fakeHV) Reboot(_ context.Context, n string) error { return h.set(n, virt.Running) }
func (h *fakeHV) SetAutostart(_ context.Context, n string, on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.doms[n]; !ok {
		return virt.ErrNoDomain
	}
	if h.auto == nil {
		h.auto = map[string]bool{}
	}
	h.auto[n] = on
	return nil
}
func (h *fakeHV) Autostart(_ context.Context, n string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.auto[n], nil
}
func (h *fakeHV) FSFreeze(_ context.Context, n string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.noAgent {
		return errors.New("guest agent is not responding")
	}
	h.frozen++
	return nil
}
func (h *fakeHV) FSThaw(_ context.Context, n string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.thawed++
	return nil
}
func (h *fakeHV) Screenshot(_ context.Context, n string) ([]byte, error) {
	return append([]byte("P6\n1 1\n255\n"), 1, 2, 3), nil
}
func (h *fakeHV) Ports(_ context.Context, n string) ([]string, error)  { return []string{"vnet7"}, nil }
func (h *fakeHV) SetLiveMetadata(_ context.Context, n, x string) error { return nil }
func (h *fakeHV) VNCAddr(_ context.Context, n string) (string, error)  { return "127.0.0.1:5900", nil }
func (h *fakeHV) SetUserPassword(_ context.Context, n, user, pw string) error {
	if h.noAgent {
		return errors.New("guest agent is not connected")
	}
	if h.agentWarmup > 0 {
		h.agentWarmup--
		return errors.New("error: Guest agent is not responding: QEMU guest agent is not connected")
	}
	if user == "nobody-here" {
		return errors.New("error: internal error: unable to execute QEMU agent command 'guest-set-user-password': chpasswd: user does not exist")
	}
	h.pw = user + ":" + pw
	return nil
}
func (h *fakeHV) Addresses(_ context.Context, n string) ([]string, error) {
	return []string{"192.168.122.50"}, nil
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

type fakeDisks struct {
	live  map[string]bool
	snaps map[string][]Snapshot // vm -> oldest first
	size  map[string]int
}

func (d *fakeDisks) Snapshot(_ context.Context, vm, name string) error {
	if d.snaps == nil {
		d.snaps = map[string][]Snapshot{}
	}
	d.snaps[vm] = append(d.snaps[vm], Snapshot{Name: name, CreatedAt: "2026-10-12T10:00:00Z"})
	return nil
}
func (d *fakeDisks) Snapshots(_ context.Context, vm string) ([]Snapshot, error) {
	return append([]Snapshot{}, d.snaps[vm]...), nil
}
func (d *fakeDisks) Rollback(_ context.Context, vm, name string, destroyNewer bool) error {
	l := d.snaps[vm]
	for i, s := range l {
		if s.Name == name {
			if i != len(l)-1 && !destroyNewer {
				return errors.New("cannot rollback: more recent snapshots exist")
			}
			d.snaps[vm] = l[:i+1]
			return nil
		}
	}
	return errors.New("no such snapshot")
}
func (d *fakeDisks) DestroySnapshot(_ context.Context, vm, name string) error {
	l := d.snaps[vm]
	for i, s := range l {
		if s.Name == name {
			d.snaps[vm] = append(l[:i:i], l[i+1:]...)
			return nil
		}
	}
	return errors.New("no such snapshot")
}
func (d *fakeDisks) ResizeDisk(_ context.Context, vm string, gb int) error {
	if d.size == nil {
		d.size = map[string]int{}
	}
	d.size[vm] = gb
	return nil
}
func (d *fakeDisks) CreateDisk(_ context.Context, vm string, gb int) (string, error) {
	if d.live[vm] {
		return "", errors.New("zfs create: dataset already exists")
	}
	d.live[vm] = true
	_ = d.ResizeDisk(context.Background(), vm, gb)
	return "/dev/zvol/tank/vms/" + vm + "-disk0", nil
}
func (d *fakeDisks) DiskSizeGB(_ context.Context, vm string) (int, error) { return d.size[vm], nil }

func (d *fakeDisks) CloneDisk(_ context.Context, _, vm string, sizeGB int) (string, error) {
	if d.live[vm] {
		return "", errors.New("zfs clone: dataset already exists") // like real ZFS
	}
	d.live[vm] = true
	_ = d.ResizeDisk(context.Background(), vm, sizeGB)
	return "/dev/zvol/tank/vms/" + vm + "-disk0", nil
}
func (d *fakeDisks) DestroyDisk(_ context.Context, vm string) error {
	delete(d.live, vm)
	delete(d.snaps, vm)
	return nil
}
func (d *fakeDisks) SnapshotExists(context.Context, string) bool { return true }

type fakeSeeds struct {
	live map[string]bool
	last cloudinit.Spec
}

func (s *fakeSeeds) Build(_ context.Context, vm string, sp cloudinit.Spec) (string, error) {
	s.live[vm] = true
	s.last = sp
	return "/var/lib/p8s/seeds/" + vm + "-seed.iso", nil
}
func (s *fakeSeeds) Remove(vm string) error { delete(s.live, vm); return nil }

func newSvc() (*Service, *fakeHV, *fakeDisks, *fakeSeeds) {
	hv, d, s := newHV(), &fakeDisks{live: map[string]bool{}}, &fakeSeeds{live: map[string]bool{}}
	return &Service{
		HV: hv, Disks: d, Seeds: s, Bridge: "vmbr0",
		Templates:  []config.Template{{Name: "debian-13", Snapshot: "tank/t/debian-13@base", MinDiskGB: 4}},
		Limits:     Limits{MaxVCPUs: 8, MaxMemoryMiB: 16384, MaxDiskGB: 500},
		AgentRetry: time.Millisecond, AgentWait: 50 * time.Millisecond,
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
	if !v.Autostart || v.IP != "192.168.122.50" {
		t.Fatalf("autostart/ip: %+v", v)
	}
	if v, err = svc.Reboot(ctx, "web1"); err != nil || v.State != virt.Running {
		t.Fatalf("reboot: %v %+v", err, v)
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
	if v.IP != "" {
		t.Fatalf("stopped VM should have no IP, got %q", v.IP)
	}
	if _, err := svc.Reboot(ctx, "web1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reboot stopped VM: want ErrConflict, got %v", err)
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

func TestCreateClearsLeftoversFromInterruptedCreate(t *testing.T) {
	svc, _, disks, seeds := newSvc()
	disks.live["p8s-web1"] = true // disk left behind by a crash, but no domain
	seeds.live["p8s-web1"] = true
	v, err := svc.Create(context.Background(), CreateRequest{Name: "web1", Template: "debian-13"})
	if err != nil {
		t.Fatalf("create after crash leftovers: %v", err)
	}
	if v.State != virt.Running {
		t.Fatalf("state %s", v.State)
	}
}

func TestAutostartOptOut(t *testing.T) {
	svc, _, _, _ := newSvc()
	off := false
	v, err := svc.Create(context.Background(), CreateRequest{Name: "tmp1", Template: "debian-13", Autostart: &off})
	if err != nil || v.Autostart {
		t.Fatalf("err=%v autostart=%v", err, v.Autostart)
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

type keySeeds struct {
	fakeSeeds
	got []string
}

func (k *keySeeds) Build(ctx context.Context, vm string, s cloudinit.Spec) (string, error) {
	k.got = s.SSHKeys
	return k.fakeSeeds.Build(ctx, vm, s)
}

func TestDefaultSSHKeysUsedWhenNoneGiven(t *testing.T) {
	svc, _, _, _ := newSvc()
	ks := &keySeeds{fakeSeeds: fakeSeeds{live: map[string]bool{}}}
	svc.Seeds, svc.SSHKeys = ks, []string{"ssh-ed25519 HOSTKEY"}
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "ui1", Template: "debian-13"}); err != nil {
		t.Fatal(err)
	}
	if len(ks.got) != 1 || ks.got[0] != "ssh-ed25519 HOSTKEY" {
		t.Fatalf("default keys not applied: %v", ks.got)
	}
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "ui2", Template: "debian-13", SSHKeys: []string{"ssh-rsa MINE"}}); err != nil {
		t.Fatal(err)
	}
	if ks.got[0] != "ssh-rsa MINE" {
		t.Fatalf("explicit keys should win: %v", ks.got)
	}
}

func TestNetworks(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()

	// No LAN bridge configured: only NAT, and asking for LAN is a clear 400.
	if got := svc.Networks(); len(got) != 1 || got[0] != NetNAT {
		t.Fatalf("networks without lan: %v", got)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "a1", Template: "debian-13", Network: "lan"}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "lan-setup") {
		t.Fatalf("lan without bridge: %v", err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "a1", Template: "debian-13", Network: "wifi"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad network: %v", err)
	}

	// Configured but the bridge is gone from the host: still not offered.
	svc.LANBridge = "br0"
	up := false
	svc.BridgeUp = func(n string) bool { return n == "br0" && up }
	if svc.LANAvailable() {
		t.Fatal("lan offered while br0 is down")
	}
	up = true
	if got := svc.Networks(); len(got) != 2 || got[1] != NetLAN {
		t.Fatalf("networks with lan: %v", got)
	}

	nat, err := svc.Create(ctx, CreateRequest{Name: "nat1", Template: "debian-13"})
	if err != nil || nat.Network != NetNAT {
		t.Fatalf("default network: %v %+v", err, nat)
	}
	lan, err := svc.Create(ctx, CreateRequest{Name: "lan1", Template: "debian-13", Network: "lan"})
	if err != nil || lan.Network != NetLAN {
		t.Fatalf("lan create: %v %+v", err, lan)
	}
	spec, _ := virt.ParseDomainXML(hv.doms["p8s-lan1"])
	if spec.Bridge != "br0" {
		t.Fatalf("lan VM attached to %q, want br0", spec.Bridge)
	}
	spec, _ = virt.ParseDomainXML(hv.doms["p8s-nat1"])
	if spec.Bridge != "vmbr0" {
		t.Fatalf("nat VM attached to %q, want vmbr0", spec.Bridge)
	}
}

func TestSnapshots(t *testing.T) {
	ctx := context.Background()
	svc, hv, disks, _ := newSvc()
	if _, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	// Running VM: filesystems frozen and thawed around the snapshot.
	sn, err := svc.CreateSnapshot(ctx, "web1", "before-upgrade")
	if err != nil || sn.Name != "before-upgrade" || hv.frozen != 1 || hv.thawed != 1 {
		t.Fatalf("snapshot: %v %+v frozen=%d thawed=%d", err, sn, hv.frozen, hv.thawed)
	}
	// No guest agent: still works (crash-consistent), nothing to thaw.
	hv.noAgent = true
	auto, err := svc.CreateSnapshot(ctx, "web1", "")
	if err != nil || !strings.HasPrefix(auto.Name, "snap-") || hv.thawed != 1 {
		t.Fatalf("auto-named snapshot: %v %+v thawed=%d", err, auto, hv.thawed)
	}
	if _, err := svc.CreateSnapshot(ctx, "web1", "before-upgrade"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := svc.CreateSnapshot(ctx, "web1", "Bad Name"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := svc.CreateSnapshot(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing vm: %v", err)
	}
	list, _ := svc.Snapshots(ctx, "web1")
	if len(list) != 2 {
		t.Fatalf("list %+v", list)
	}

	// Rollback needs a stopped VM, and confirmation when newer snapshots exist.
	if _, err := svc.Rollback(ctx, "web1", "before-upgrade", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("rollback running: %v", err)
	}
	_, _ = svc.Stop(ctx, "web1", true)
	if _, err := svc.Rollback(ctx, "web1", "before-upgrade", false); !errors.Is(err, ErrConflict) ||
		!strings.Contains(err.Error(), auto.Name) {
		t.Fatalf("rollback past newer snapshot: %v", err)
	}
	if _, err := svc.Rollback(ctx, "web1", "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback missing: %v", err)
	}
	disks.size["p8s-web1"] = 8 // the snapshot was taken when the disk was smaller
	v, err := svc.Rollback(ctx, "web1", "before-upgrade", true)
	if err != nil || v.DiskGB != 8 {
		t.Fatalf("rollback: %v %+v", err, v)
	}
	if list, _ = svc.Snapshots(ctx, "web1"); len(list) != 1 {
		t.Fatalf("newer snapshot should be gone: %+v", list)
	}

	if err := svc.DeleteSnapshot(ctx, "web1", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	if err := svc.DeleteSnapshot(ctx, "web1", "before-upgrade"); err != nil {
		t.Fatal(err)
	}
	if list, _ = svc.Snapshots(ctx, "web1"); len(list) != 0 {
		t.Fatalf("after delete: %+v", list)
	}
}

func TestSnapshotLimit(t *testing.T) {
	ctx := context.Background()
	svc, _, disks, _ := newSvc()
	_, _ = svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13"})
	for i := 0; i < MaxSnapshots; i++ {
		_ = disks.Snapshot(ctx, "p8s-web1", "s"+strings.Repeat("x", i))
	}
	if _, err := svc.CreateSnapshot(ctx, "web1", "one-more"); !errors.Is(err, ErrConflict) {
		t.Fatalf("over limit: %v", err)
	}
}

func TestResize(t *testing.T) {
	ctx := context.Background()
	svc, hv, disks, _ := newSvc()
	if _, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", VCPUs: 1, MemoryMiB: 1024, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	_ = hv.SetAutostart(ctx, "p8s-web1", true)
	if _, err := svc.Resize(ctx, "web1", ResizeRequest{VCPUs: 2}); !errors.Is(err, ErrConflict) {
		t.Fatalf("resize running: %v", err)
	}
	_, _ = svc.Stop(ctx, "web1", true)
	for _, bad := range []ResizeRequest{{VCPUs: 99}, {MemoryMiB: 100}, {DiskGB: 5}, {DiskGB: 9999}} {
		if _, err := svc.Resize(ctx, "web1", bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("resize %+v: %v", bad, err)
		}
	}
	v, err := svc.Resize(ctx, "web1", ResizeRequest{VCPUs: 4, MemoryMiB: 4096, DiskGB: 30})
	if err != nil || v.VCPUs != 4 || v.MemoryMiB != 4096 || v.DiskGB != 30 || disks.size["p8s-web1"] != 30 {
		t.Fatalf("resize: %v %+v size=%d", err, v, disks.size["p8s-web1"])
	}
	// Only memory: the rest stays, and the VM keeps its identity.
	v2, err := svc.Resize(ctx, "web1", ResizeRequest{MemoryMiB: 2048})
	if err != nil || v2.VCPUs != 4 || v2.DiskGB != 30 || v2.MemoryMiB != 2048 || v2.UUID != v.UUID || v2.MAC != v.MAC || !v2.Autostart {
		t.Fatalf("partial resize: %v %+v", err, v2)
	}
	if v, _ = svc.Start(ctx, "web1"); v.State != virt.Running {
		t.Fatalf("start after resize: %s", v.State)
	}
}

func TestConsoleAndPassword(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	_, _ = svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13"})
	if a, err := svc.ConsoleAddr(ctx, "web1"); err != nil || a != "127.0.0.1:5900" {
		t.Fatalf("console: %q %v", a, err)
	}
	if err := svc.SetPassword(ctx, "web1", "", "s3cret-pass"); err != nil || hv.pw != "debian:s3cret-pass" {
		t.Fatalf("password: %v %q", err, hv.pw)
	}
	for _, bad := range [][2]string{{"debian", "short"}, {"Root!", "longenough"}, {"debian", "two\nlines!"}} {
		if err := svc.SetPassword(ctx, "web1", bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	hv.noAgent = true
	if err := svc.SetPassword(ctx, "web1", "debian", "longenough"); !errors.Is(err, ErrConflict) {
		t.Fatalf("no agent: %v", err)
	}
	_, _ = svc.Stop(ctx, "web1", true)
	if _, err := svc.ConsoleAddr(ctx, "web1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("console of stopped VM: %v", err)
	}
	if _, err := svc.ConsoleAddr(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("console of missing VM: %v", err)
	}
}

func TestRootPassword(t *testing.T) {
	ctx := context.Background()
	svc, _, _, seeds := newSvc()
	v, err := svc.Create(ctx, CreateRequest{Name: "vps1", Template: "debian-13", RootPassword: "Sup3r-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seeds.last.RootPasswordHash, "$6$") || !seeds.last.RootSSHPassword {
		t.Fatalf("seed spec %+v", seeds.last)
	}
	if b, _ := json.Marshal(v); strings.Contains(string(b), "Sup3r") || strings.Contains(string(b), "$6$") {
		t.Fatalf("password leaked in VM object: %s", b)
	}
	off := false
	if _, err := svc.Create(ctx, CreateRequest{Name: "vps2", Template: "debian-13", RootPassword: "Sup3r-secret", SSHPasswordAuth: &off}); err != nil || seeds.last.RootSSHPassword {
		t.Fatalf("ssh password opt-out: %v %+v", err, seeds.last)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "vps3", Template: "debian-13"}); err != nil || seeds.last.RootPasswordHash != "" {
		t.Fatalf("no password: %v %+v", err, seeds.last)
	}
	on := true
	for _, bad := range []CreateRequest{
		{Name: "bad1", Template: "debian-13", RootPassword: "short"},
		{Name: "bad2", Template: "debian-13", RootPassword: "two\nlines-here"},
		{Name: "bad3", Template: "debian-13", RootPassword: "longenough", UserData: "#cloud-config\n"},
		{Name: "bad4", Template: "debian-13", SSHPasswordAuth: &on},
	} {
		if _, err := svc.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want invalid, got %v", bad.Name, err)
		}
	}
}

func TestLoginUserFromTemplate(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newSvc()
	svc.Templates = append(svc.Templates, config.Template{Name: "ubuntu-24.04", Snapshot: "tank/t/ubuntu@base", MinDiskGB: 4, DefaultUser: "ubuntu"})
	u, err := svc.Create(ctx, CreateRequest{Name: "u1", Template: "ubuntu-24.04"})
	if err != nil || u.User != "ubuntu" || u.Template != "ubuntu-24.04" {
		t.Fatalf("ubuntu VM: %v %+v", err, u)
	}
	d, _ := svc.Create(ctx, CreateRequest{Name: "d1", Template: "debian-13"})
	if d.User != "debian" {
		t.Fatalf("old-style template without default_user should mean debian: %+v", d)
	}
}

func TestSetPasswordWaitsForAgent(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	svc.AgentRetry = time.Millisecond
	_, _ = svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13"})
	hv.agentWarmup = 3 // just rebooted: agent connects after a few tries
	if err := svc.SetPassword(ctx, "web1", "root", "longenough"); err != nil || hv.pw != "root:longenough" {
		t.Fatalf("should succeed once the agent is up: %v", err)
	}
	// A real failure is reported at once with the guest's own reason.
	err := svc.SetPassword(ctx, "web1", "nobody-here", "longenough")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "user does not exist") {
		t.Fatalf("got %v", err)
	}
}
