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
		if !strings.HasPrefix(c, "zfs list -H -t snapshot -o name ") && // replica snapshots looked up before a rollback
			!strings.HasPrefix(c, "zfs list -H -o name -t volume") && // data disks
			!strings.HasPrefix(c, "zfs list -H -o name -t snapshot -d 1") { // kept backup snapshots
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

func TestExtraDisks(t *testing.T) {
	r := &testfake.Runner{Out: map[string]string{
		"zfs list -H -o name -t volume -d 1 tank/vms": "tank/vms/p8s-a1-disk0\ntank/vms/p8s-a1-disk2\ntank/vms/p8s-a1-disk1\n" +
			"tank/vms/p8s-a1-disk1-disk0\ntank/vms/p8s-a1-disk01\ntank/vms/p8s-b-disk1\n",
	}, Fail: map[string]string{"zfs list -H -o name -t snapshot tank/vms/p8s-a1-disk2@s1": "does not exist"}}
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { return true }}
	ctx := context.Background()
	if got := z.Extras(ctx, "p8s-a1"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("extras %v", got)
	}
	if dev, err := z.CreateExtra(ctx, "p8s-a1", 3, 50); err != nil || dev != "/dev/zvol/tank/vms/p8s-a1-disk3" {
		t.Fatalf("create %q %v", dev, err)
	}
	r.Calls = nil
	_ = z.Snapshot(ctx, "p8s-a1", "s1")
	_ = z.Rollback(ctx, "p8s-a1", "s1", false)
	_ = z.DestroyDisk(ctx, "p8s-a1")
	all := strings.Join(r.Calls, "\n") + "\n"
	for _, w := range []string{
		"zfs snapshot tank/vms/p8s-a1-disk0@s1 tank/vms/p8s-a1-disk1@s1 tank/vms/p8s-a1-disk2@s1",
		"zfs rollback tank/vms/p8s-a1-disk1@s1",
		"zfs destroy -r tank/vms/p8s-a1-disk1",
		"zfs destroy -r tank/vms/p8s-a1-disk2",
		"zfs destroy -r tank/vms/p8s-a1-disk0",
	} {
		if !strings.Contains(all, w+"\n") {
			t.Errorf("missing %q in\n%s", w, all)
		}
	}
	if strings.Contains(all, "rollback tank/vms/p8s-a1-disk2@s1") {
		t.Error("disk 2 has no such snapshot; it must be left alone")
	}
}

func TestPools(t *testing.T) {
	r := &testfake.Runner{Out: map[string]string{
		"zfs list -H -o name -t volume -d 1 p8s-san/vms": "p8s-san/vms/p8s-db-disk0\np8s-san/vms/p8s-db-disk1\n",
	}}
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { return true },
		Pools: func() map[string]string { return map[string]string{"san": "p8s-san/vms"} }}
	ctx := context.Background()
	if z.DevicePath("p8s-db") != "/dev/zvol/p8s-san/vms/p8s-db-disk0" || z.StorageOf("p8s-db") != "san" {
		t.Fatalf("found on san: %s %s", z.DevicePath("p8s-db"), z.StorageOf("p8s-db"))
	}
	if z.StorageOf("p8s-web") != "local" || z.DevicePath("p8s-web") != "/dev/zvol/tank/vms/p8s-web-disk0" {
		t.Fatal("others stay local")
	}
	if got := z.Extras(ctx, "p8s-db"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("extras on san %v", got)
	}
	if err := z.Place("p8s-new", "nope"); err == nil {
		t.Fatal("unknown storage accepted")
	}
	if err := z.Place("p8s-new", "san"); err != nil {
		t.Fatal(err)
	}
	if dev, err := z.CreateDisk(ctx, "p8s-new", 10); err != nil || dev != "/dev/zvol/p8s-san/vms/p8s-new-disk0" {
		t.Fatalf("create on san %q %v", dev, err)
	}
	if s := z.Storages(); len(s) != 2 || s[0] != "local" || s[1] != "san" {
		t.Fatalf("storages %v", s)
	}
	_ = z.DestroyDisk(ctx, "p8s-new")
	if !strings.Contains(strings.Join(r.Calls, "\n"), "zfs destroy -r p8s-san/vms/p8s-new-disk0") {
		t.Fatalf("calls %v", r.Calls)
	}
}
