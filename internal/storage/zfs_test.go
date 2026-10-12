package storage

import (
	"context"
	"strings"
	"testing"
	"time"

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
