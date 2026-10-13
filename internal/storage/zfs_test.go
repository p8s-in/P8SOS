package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"p8sos/internal/execx"
	"p8sos/internal/testfake"
)

func TestCloneDisk(t *testing.T) {
	r := &testfake.Runner{}
	z := &ZFS{Run: r, Dataset: "tank/p8s/vms", Exists: func(string) bool { return true }}
	dev, err := z.CloneDisk(context.Background(), "tank/p8s/templates/debian-13@base", "p8s-web1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if dev != "/dev/zvol/tank/p8s/vms/p8s-web1-disk0" {
		t.Fatalf("dev = %s", dev)
	}
	want := []string{
		"zfs clone -p tank/p8s/templates/debian-13@base tank/p8s/vms/p8s-web1-disk0",
		"zfs set volsize=20G tank/p8s/vms/p8s-web1-disk0",
	}
	for i, w := range want {
		if r.Calls[i] != w {
			t.Errorf("call %d = %q, want %q", i, r.Calls[i], w)
		}
	}
}

func TestCloneDiskResizeFailureCleansUp(t *testing.T) {
	r := &testfake.Runner{Fail: map[string]string{"zfs set": "volsize too small"}}
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { return true }}
	if _, err := z.CloneDisk(context.Background(), "tank/t@base", "p8s-a1", 1); err == nil {
		t.Fatal("expected error")
	}
	if last := r.Calls[len(r.Calls)-1]; !strings.HasPrefix(last, "zfs destroy -r tank/vms/p8s-a1-disk0") {
		t.Fatalf("expected cleanup, last call %q", last)
	}
}

func TestDestroyMissingDiskIsOK(t *testing.T) {
	r := &testfake.Runner{Fail: map[string]string{"zfs destroy": "dataset does not exist"}}
	z := &ZFS{Run: r, Dataset: "tank/vms"}
	if err := z.DestroyDisk(context.Background(), "p8s-gone"); err != nil {
		t.Fatal(err)
	}
}

func TestCloneDiskWaitsForDevice(t *testing.T) {
	r := &testfake.Runner{}
	n := 0
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { n++; return n > 2 }}
	if _, err := z.CloneDisk(context.Background(), "tank/t@base", "p8s-a1", 4); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("checked device %d times, want 3", n)
	}
}

func TestCloneDiskDeviceNeverAppears(t *testing.T) {
	r := &testfake.Runner{}
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { return false }, Wait: 50 * time.Millisecond}
	if _, err := z.CloneDisk(context.Background(), "tank/t@base", "p8s-a1", 4); err == nil {
		t.Fatal("expected error")
	}
	if last := r.Calls[len(r.Calls)-1]; !strings.HasPrefix(last, "zfs destroy -r tank/vms/p8s-a1-disk0") {
		t.Fatalf("expected cleanup, last call %q", last)
	}
}

func TestSnapshots(t *testing.T) {
	r := &testfake.Runner{Out: map[string]string{
		"zfs list -H -p -t snapshot":     "tank/vms/p8s-a1-disk0@first\t1791700000\t4096\ntank/vms/p8s-a1-disk0@second\t1791700600\t1048576\n",
		"zfs get -H -p -o value volsize": "21474836480\n",
	}}
	z := &ZFS{Run: r, Dataset: "tank/vms"}
	ctx := context.Background()
	snaps, err := z.Snapshots(ctx, "p8s-a1")
	if err != nil || len(snaps) != 2 || snaps[0].Name != "first" || snaps[1].UsedBytes != 1048576 ||
		snaps[0].CreatedAt != "2026-10-11T06:26:40Z" {
		t.Fatalf("snapshots %v %+v", err, snaps)
	}
	if gb, err := z.DiskSizeGB(ctx, "p8s-a1"); err != nil || gb != 20 {
		t.Fatalf("size %d %v", gb, err)
	}
	_ = z.Snapshot(ctx, "p8s-a1", "third")
	_ = z.Rollback(ctx, "p8s-a1", "first", true)
	_ = z.Rollback(ctx, "p8s-a1", "second", false)
	_ = z.DestroySnapshot(ctx, "p8s-a1", "first")
	_ = z.ResizeDisk(ctx, "p8s-a1", 30)
	want := []string{
		"zfs snapshot tank/vms/p8s-a1-disk0@third",
		"zfs rollback -r tank/vms/p8s-a1-disk0@first",
		"zfs rollback tank/vms/p8s-a1-disk0@second",
		"zfs destroy tank/vms/p8s-a1-disk0@first",
		"zfs set volsize=30G tank/vms/p8s-a1-disk0",
	}
	var got []string
	for _, c := range r.Calls[2:] {
		if !strings.HasPrefix(c, "zfs list -H -t snapshot -o name ") { // replica snapshots looked up before a rollback
			got = append(got, c)
		}
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call %d = %q, want %q", i, got[i], w)
		}
	}
	if len(ParseSnapshots("")) != 0 {
		t.Fatal("empty output should give no snapshots")
	}
}

// busyRunner fails "zfs destroy" with "dataset is busy" the first n times.
type busyRunner struct {
	testfake.Runner
	n int
}

func (b *busyRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "zfs" && args[0] == "destroy" && b.n > 0 {
		b.n--
		return nil, &execx.Error{Cmd: name, Args: args, Stderr: "cannot destroy 'tank/vms/p8s-a1-disk0': dataset is busy", Err: context.Canceled}
	}
	return b.Runner.Run(ctx, name, args...)
}

func TestDestroyDiskRetriesWhileBusy(t *testing.T) {
	r := &busyRunner{n: 3}
	z := &ZFS{Run: r, Dataset: "tank/vms", BusyRetry: time.Millisecond}
	if err := z.DestroyDisk(context.Background(), "p8s-a1"); err != nil {
		t.Fatalf("should succeed after the device is released: %v", err)
	}
	r = &busyRunner{n: 100}
	z.Run = r
	if err := z.DestroyDisk(context.Background(), "p8s-a1"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("should give up eventually: %v", err)
	}
}
