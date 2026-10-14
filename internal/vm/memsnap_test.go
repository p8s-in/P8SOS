package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"p8sos/internal/virt"
)

// stateHV adds virsh save/restore to the fake hypervisor.
type stateHV struct {
	*fakeHV
	saved    []string
	restored []string
	failSave bool
	disks    *fakeDisks
	snapsAt  int // number of disk snapshots when the VM was saved
}

func (h *stateHV) SaveState(_ context.Context, name, file string) error {
	if h.failSave {
		return errors.New("virsh save: no space")
	}
	h.saved = append(h.saved, file)
	h.snapsAt = len(h.disks.snaps[name])
	if err := os.WriteFile(file, []byte("RAM"), 0o600); err != nil {
		return err
	}
	return h.set(name, virt.Stopped) // like virsh save: the domain stops
}

func (h *stateHV) RestoreState(_ context.Context, file string) error {
	if _, err := os.Stat(file); err != nil {
		return err
	}
	h.restored = append(h.restored, file)
	return h.set("p8s-"+filepath.Base(filepath.Dir(file))[4:], virt.Running)
}

func newStateSvc(t *testing.T) (*Service, *stateHV, *fakeDisks) {
	svc, hv, d, _ := newSvc()
	sh := &stateHV{fakeHV: hv, disks: d}
	svc.HV = sh
	svc.StateDir = t.TempDir()
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "web1", Template: "debian-13", DiskGB: 10, MemoryMiB: 1024}); err != nil {
		t.Fatal(err)
	}
	return svc, sh, d
}

func TestSnapshotWithRAM(t *testing.T) {
	ctx := context.Background()
	svc, hv, d := newStateSvc(t)
	freeBytes = func(string) (int64, error) { return 100 << 30, nil }

	sn, err := svc.CreateSnapshotRAM(ctx, "web1", "with-ram")
	if err != nil || !sn.RAM || sn.RAMBytes != 3 {
		t.Fatalf("snapshot: %v %+v", err, sn)
	}
	// memory saved first, disk snapshot after (same instant), then it runs on
	if len(hv.saved) != 1 || hv.snapsAt != 0 || len(d.snaps["p8s-web1"]) != 1 || len(hv.restored) != 1 {
		t.Fatalf("order: saved=%v snapsAt=%d snaps=%v restored=%v", hv.saved, hv.snapsAt, d.snaps, hv.restored)
	}
	if st, _ := hv.State(ctx, "p8s-web1"); st != virt.Running {
		t.Fatalf("VM should run on, is %s", st)
	}
	if _, err := os.Stat(svc.stateFile("p8s-web1", "with-ram")); err != nil {
		t.Fatalf("state file kept: %v", err)
	}
	// a normal snapshot next to it
	if _, err := svc.CreateSnapshot(ctx, "web1", "plain"); err != nil {
		t.Fatal(err)
	}
	list, _ := svc.Snapshots(ctx, "web1")
	if len(list) != 2 || !list[0].RAM || list[1].RAM {
		t.Fatalf("list %+v", list)
	}

	// running VM + RAM snapshot: rollback without stopping, resumes from the memory
	hv.restored = nil
	v, err := svc.Rollback(ctx, "web1", "with-ram", true)
	if err != nil || v.State != virt.Running || len(hv.restored) != 1 {
		t.Fatalf("rollback: %v %+v restored=%v", err, v, hv.restored)
	}
	// a plain snapshot still needs the VM stopped
	if _, err := svc.CreateSnapshot(ctx, "web1", "plain2"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rollback(ctx, "web1", "plain2", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("plain rollback while running: %v", err)
	}

	// delete removes the memory too
	if err := svc.DeleteSnapshot(ctx, "web1", "with-ram"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc.stateFile("p8s-web1", "with-ram")); !os.IsNotExist(err) {
		t.Fatalf("state file should be gone: %v", err)
	}
	// stopped VM: nothing to save
	_, _ = svc.Stop(ctx, "web1", true)
	if _, err := svc.CreateSnapshotRAM(ctx, "web1", "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopped: %v", err)
	}
	// deleting the VM removes its memory folder
	_, _ = svc.Start(ctx, "web1")
	if _, err := svc.CreateSnapshotRAM(ctx, "web1", "again"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "web1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc.stateDir("p8s-web1")); !os.IsNotExist(err) {
		t.Fatalf("state dir should be gone: %v", err)
	}
}

func TestSnapshotWithRAMNoRoomOrSaveFails(t *testing.T) {
	ctx := context.Background()
	svc, hv, d := newStateSvc(t)
	freeBytes = func(string) (int64, error) { return 100 << 20, nil } // 100 MB free < 1 GB RAM
	defer func() { freeBytes = func(string) (int64, error) { return 100 << 30, nil } }()
	if _, err := svc.CreateSnapshotRAM(ctx, "web1", "x"); !errors.Is(err, ErrConflict) || len(hv.saved) != 0 {
		t.Fatalf("no room: %v", err)
	}
	freeBytes = func(string) (int64, error) { return 100 << 30, nil }
	hv.failSave = true
	if _, err := svc.CreateSnapshotRAM(ctx, "web1", "x"); err == nil || len(d.snaps["p8s-web1"]) != 0 {
		t.Fatalf("failed save must not leave a snapshot: %v %v", err, d.snaps)
	}
	if st, _ := hv.State(ctx, "p8s-web1"); st != virt.Running {
		t.Fatalf("VM must keep running after a failed save: %s", st)
	}
	// no StateDir: refused cleanly
	svc.StateDir = ""
	if _, err := svc.CreateSnapshotRAM(ctx, "web1", "y"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("off: %v", err)
	}
}
