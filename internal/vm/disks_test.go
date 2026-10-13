package vm

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

type attachHV struct {
	*fakeHV
	attached []string
}

func (a *attachHV) AttachDisk(_ context.Context, name string, x []byte) error {
	a.attached = append(a.attached, string(x))
	return nil
}

func TestDataDisks(t *testing.T) {
	ctx := context.Background()
	svc, hv, disks, _ := newSvc()
	ahv := &attachHV{fakeHV: hv}
	svc.HV = ahv
	if _, err := svc.Create(ctx, CreateRequest{Name: "db1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	// running virtio VM: plugged in at once
	v, note, err := svc.AddDisk(ctx, "db1", 50)
	if err != nil || len(v.Disks) != 1 || v.Disks[0] != (Disk{1, 50, "vdb"}) || !strings.Contains(note, "vdb") {
		t.Fatalf("add: %v %q %+v", err, note, v.Disks)
	}
	if len(ahv.attached) != 1 || !strings.Contains(ahv.attached[0], "p8s-db1-disk1") {
		t.Fatalf("attach: %v", ahv.attached)
	}
	v, _, _ = svc.AddDisk(ctx, "db1", 20)
	if len(v.Disks) != 2 || v.Disks[1].Device != "vdc" || v.ExtraGB() != 70 {
		t.Fatalf("second: %+v", v.Disks)
	}
	if _, _, err := svc.AddDisk(ctx, "db1", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("size 0: %v", err)
	}
	// grow / remove need it stopped
	if _, err := svc.ResizeDisk(ctx, "db1", 1, 80); !errors.Is(err, ErrConflict) {
		t.Fatalf("grow running: %v", err)
	}
	if _, err := svc.RemoveDisk(ctx, "db1", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("remove running: %v", err)
	}
	_, _ = svc.Stop(ctx, "db1", true)
	if _, err := svc.ResizeDisk(ctx, "db1", 1, 30); !errors.Is(err, ErrInvalid) {
		t.Fatalf("shrink: %v", err)
	}
	if v, err = svc.ResizeDisk(ctx, "db1", 1, 80); err != nil || v.Disks[0].SizeGB != 80 || disks.extra["p8s-db1"][1] != 80 {
		t.Fatalf("grow: %v %+v", err, v.Disks)
	}
	if _, err := svc.ResizeDisk(ctx, "db1", 5, 80); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing disk: %v", err)
	}
	// disk 1 removed: disk 2 keeps its place (vdc); the next new disk takes slot 1
	if v, err = svc.RemoveDisk(ctx, "db1", 1); err != nil || len(v.Disks) != 1 || v.Disks[0].Index != 2 || v.Disks[0].Device != "vdc" {
		t.Fatalf("remove: %v %+v", err, v.Disks)
	}
	if _, ok := disks.extra["p8s-db1"][1]; ok {
		t.Fatal("disk 1's data should be gone")
	}
	v, note, _ = svc.AddDisk(ctx, "db1", 5)
	if v.Disks[0].Index != 1 || v.Disks[1].Index != 2 || strings.Contains(note, "restart") {
		t.Fatalf("re-add: %+v %q", v.Disks, note)
	}
	// the most is 8
	for i := 0; i < 6; i++ {
		if _, _, err := svc.AddDisk(ctx, "db1", 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.AddDisk(ctx, "db1", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("ninth: %v", err)
	}
	// deleting the VM takes its disks
	if err := svc.Delete(ctx, "db1"); err != nil || len(disks.extra["p8s-db1"]) != 0 {
		t.Fatalf("delete: %v %v", err, disks.extra)
	}
}

func TestDataDiskSATANeedsRestart(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	svc.HV = &attachHV{fakeHV: hv}
	if _, err := svc.Create(ctx, CreateRequest{Name: "w1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	if err := svc.redefine(ctx, "p8s-w1", func(sp *virt.Spec) { sp.DiskBus = "sata" }); err != nil {
		t.Fatal(err)
	}
	v, note, err := svc.AddDisk(ctx, "w1", 10)
	if err != nil || v.Disks[0].Device != "sdf" || !strings.Contains(note, "SATA") {
		t.Fatalf("sata: %v %q %+v", err, note, v.Disks)
	}
}

func TestBackupWithDataDisks(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newSvc()
	r := withBackups(t, svc)
	if _, err := svc.Create(ctx, CreateRequest{Name: "db2", Template: "debian-13", VCPUs: 1, MemoryMiB: 1024, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	_, _, _ = svc.AddDisk(ctx, "db2", 40)
	j, _ := svc.StartBackup(ctx, "db2")
	if done := waitJob(t, svc, j); done.State != jobs.Done || !strings.Contains(done.Message, "2 disks") {
		t.Fatalf("backup: %+v", done)
	}
	l, _ := svc.ListBackups("db2")
	if len(l) != 1 || len(l[0].Extra) != 1 || l[0].Extra[0].Index != 1 || l[0].Extra[0].SizeGB != 40 || l[0].Extra[0].Bytes == 0 {
		t.Fatalf("meta %+v", l)
	}
	b := l[0]
	// a disk added after the backup stays when restoring in place
	_, _ = svc.Stop(ctx, "db2", true)
	_, _, _ = svc.AddDisk(ctx, "db2", 7)
	j, _ = svc.StartRestore(ctx, "db2", b.ID, "")
	done := waitJob(t, svc, j)
	if done.State != jobs.Done || !strings.Contains(done.Message, "disk 2 is newer") {
		t.Fatalf("restore: %+v", done)
	}
	if !strings.Contains(strings.Join(r.Calls, "\n"), "zfs rename tank/vms/p8s-db2-disk1-restore tank/vms/p8s-db2-disk1") {
		t.Fatalf("calls %v", r.Calls)
	}
	if v, _ := svc.Get(ctx, "db2"); len(v.Disks) != 2 {
		t.Fatalf("after restore %+v", v.Disks)
	}
	// as a copy: both disks
	j, _ = svc.StartRestore(ctx, "db2", b.ID, "db2-copy")
	if done := waitJob(t, svc, j); done.State != jobs.Done {
		t.Fatalf("copy: %+v", done)
	}
	if v, err := svc.Get(ctx, "db2-copy"); err != nil || len(v.Disks) != 1 || v.Disks[0].SizeGB != 40 {
		t.Fatalf("copy %v %+v", err, v.Disks)
	}
	// deleting the backup removes the data disk file too
	files := svc.Backups.ExtraFiles(b)
	if err := svc.DeleteBackup("db2", b.ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s left", f)
		}
	}
}
