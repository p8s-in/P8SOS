package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/jobs"
	"p8sos/internal/testfake"
)

func withBackups(t *testing.T, svc *Service) *testfake.Runner {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "zfs")
	os.WriteFile(script, []byte("#!/bin/sh\ncase \"$1\" in send) echo \"stream $2\";; receive) cat >/dev/null;; esac\n"), 0o755)
	r := &testfake.Runner{}
	n := 0
	svc.Backups = &backup.Store{Dir: filepath.Join(dir, "backups"), Dataset: "tank/vms", Run: r, Compressor: "gzip",
		Now: func() time.Time { n++; return time.Date(2026, 10, 12, 2, 30, n, 0, time.UTC) },
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if name == "zfs" {
				name = script
			}
			return exec.CommandContext(ctx, name, args...)
		}}
	svc.Jobs = &jobs.Manager{}
	return r
}

func waitJob(t *testing.T, svc *Service, j Job) Job {
	t.Helper()
	svc.Jobs.Wait()
	got, _ := svc.Jobs.Get(j.ID)
	return got
}

func TestBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	svc, hv, disks, _ := newSvc()
	r := withBackups(t, svc)
	if _, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartBackup(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backup of missing VM: %v", err)
	}
	// Running VM: frozen and thawed; temporary snapshot removed; not shown as a user snapshot.
	j, err := svc.StartBackup(ctx, "web1")
	if err != nil {
		t.Fatal(err)
	}
	if done := waitJob(t, svc, j); done.State != jobs.Done || !strings.Contains(done.Message, "saved") {
		t.Fatalf("backup job: %+v", done)
	}
	if hv.frozen != 1 || hv.thawed != 1 {
		t.Fatalf("freeze %d thaw %d", hv.frozen, hv.thawed)
	}
	if l, _ := svc.Snapshots(ctx, "web1"); len(l) != 0 || len(disks.snaps["p8s-web1"]) != 0 {
		t.Fatalf("backup snapshot left: %+v %+v", l, disks.snaps)
	}
	list, _ := svc.ListBackups("web1")
	if len(list) != 1 || !list[0].Consistent || list[0].VCPUs != 2 || list[0].MemoryMiB != 2048 || list[0].Template != "debian-13" {
		t.Fatalf("backups: %+v", list)
	}
	b := list[0]

	// Restore over a running VM is refused; stopped is fine.
	if _, err := svc.StartRestore(ctx, "web1", b.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("restore over running VM: %v", err)
	}
	_, _ = svc.Stop(ctx, "web1", true)
	j, err = svc.StartRestore(ctx, "web1", b.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if done := waitJob(t, svc, j); done.State != jobs.Done {
		t.Fatalf("in-place restore: %+v", done)
	}
	if !strings.Contains(strings.Join(r.Calls, "\n"), "zfs rename tank/vms/p8s-web1-disk0-restore tank/vms/p8s-web1-disk0") {
		t.Fatalf("calls %v", r.Calls)
	}

	// Restore as a copy: new VM with the backup's size, running, autostart on.
	if _, err := svc.StartRestore(ctx, "web1", b.ID, "web1"); err != nil {
		t.Fatalf("same name means in place: %v", err)
	}
	svc.Jobs.Wait()
	if _, err := svc.StartRestore(ctx, "web1", b.ID, "Bad Name"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	j, err = svc.StartRestore(ctx, "web1", b.ID, "web1-copy")
	if err != nil {
		t.Fatal(err)
	}
	if done := waitJob(t, svc, j); done.State != jobs.Done {
		t.Fatalf("restore as new: %+v", done)
	}
	cp, err := svc.Get(ctx, "web1-copy")
	if err != nil || cp.State != "running" || cp.VCPUs != 2 || cp.MemoryMiB != 2048 || !cp.Autostart || cp.Template != "debian-13" {
		t.Fatalf("copy: %v %+v", err, cp)
	}
	if _, err := svc.StartRestore(ctx, "web1", b.ID, "web1-copy"); !errors.Is(err, ErrExists) {
		t.Fatalf("restore onto existing other VM: %v", err)
	}
	if _, err := svc.StartRestore(ctx, "web1", "20990101-000000", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing backup: %v", err)
	}

	// Deleting the VM keeps its backups; restoring by its name recreates it.
	if err := svc.Delete(ctx, "web1"); err != nil {
		t.Fatal(err)
	}
	if l, _ := svc.ListBackups(""); len(l) != 1 {
		t.Fatalf("backups after VM delete: %+v", l)
	}
	j, _ = svc.StartRestore(ctx, "web1", b.ID, "")
	if done := waitJob(t, svc, j); done.State != jobs.Done {
		t.Fatalf("recreate deleted VM: %+v", done)
	}
	if v, err := svc.Get(ctx, "web1"); err != nil || v.State != "running" {
		t.Fatalf("recreated: %v %+v", err, v)
	}
	if err := svc.DeleteBackup("web1", b.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteBackup("web1", b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestBackupSchedule(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newSvc()
	withBackups(t, svc)
	svc.ScheduleFile = filepath.Join(t.TempDir(), "schedule.json")
	_, _ = svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13"})
	_, _ = svc.Create(ctx, CreateRequest{Name: "web2", Template: "debian-13"})
	if err := svc.SetSchedule("web1", Schedule{Daily: true, Keep: 2}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetSchedule("web2", Schedule{Daily: true, Keep: 0}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("keep 0: %v", err)
	}
	if sch, _ := svc.GetSchedule("web1"); !sch.Daily || sch.Keep != 2 {
		t.Fatalf("schedule %+v", sch)
	}
	for i := 0; i < 3; i++ { // three nights
		if got := svc.RunScheduledBackups(ctx); len(got) != 1 || got[0] != "web1" {
			t.Fatalf("night %d started %v", i, got)
		}
		svc.Jobs.Wait()
	}
	if l, _ := svc.ListBackups("web1"); len(l) != 2 {
		t.Fatalf("keep=2 but have %d", len(l))
	}
	if l, _ := svc.ListBackups("web2"); len(l) != 0 {
		t.Fatalf("web2 has no schedule: %+v", l)
	}
	// Deleting the VM drops its schedule (but not its backups).
	_ = svc.Delete(ctx, "web1")
	if sch, _ := svc.GetSchedule("web1"); sch.Daily {
		t.Fatal("schedule survived VM delete")
	}
	if got := svc.RunScheduledBackups(ctx); len(got) != 0 {
		t.Fatalf("started %v after delete", got)
	}
}

func TestBackupsOff(t *testing.T) {
	svc, _, _, _ := newSvc()
	if _, err := svc.StartBackup(context.Background(), "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestBackupScheduleOtherGuests(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newSvc()
	withBackups(t, svc)
	svc.ScheduleFile = filepath.Join(t.TempDir(), "schedule.json")
	if err := svc.SetSchedule("ct:web2", Schedule{Daily: true, Keep: 3}); err != nil {
		t.Fatal(err)
	}
	if got := svc.RunScheduledBackups(ctx); len(got) != 0 {
		t.Fatalf("no hook but started %v", got)
	}
	var called []string
	svc.OtherBackup = func(_ context.Context, key string, keep int) error {
		called = append(called, fmt.Sprintf("%s/%d", key, keep))
		return nil
	}
	if got := svc.RunScheduledBackups(ctx); len(got) != 1 || len(called) != 1 || called[0] != "ct:web2/3" {
		t.Fatalf("started %v called %v", got, called)
	}
}
