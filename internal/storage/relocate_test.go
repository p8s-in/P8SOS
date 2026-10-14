package storage

import (
	"context"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestRelocateZvolToPool(t *testing.T) {
	r := &testfake.Runner{Out: map[string]string{"zfs get -H -p -o value volsize": "21474836480\n", "zfs list": ""}}
	z := &ZFS{Run: r, Dataset: "tank/p8s/vms", Exists: func(string) bool { return true },
		Pools: func() map[string]string { return map[string]string{"nas": "nas1/p8s-vms"} }}
	devs, err := z.Relocate(context.Background(), "p8s-web1", "nas", []int{0, 1}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if devs[0] != "/dev/zvol/nas1/p8s-vms/p8s-web1-disk0" || devs[1] != "/dev/zvol/nas1/p8s-vms/p8s-web1-disk1" {
		t.Fatalf("devs %v", devs)
	}
	all := strings.Join(r.Calls, "\n")
	for _, want := range []string{
		"zfs create -p -s -V 21474836480 -o volmode=dev nas1/p8s-vms/p8s-web1-disk0",
		"qemu-img convert -n --target-is-zero -W -f raw -O raw /dev/zvol/tank/p8s/vms/p8s-web1-disk0 /dev/zvol/nas1/p8s-vms/p8s-web1-disk0",
		"zfs destroy -r tank/p8s/vms/p8s-web1-disk1",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if z.StorageOf("p8s-web1") != "nas" {
		t.Fatal("placement")
	}
	// a failed copy puts it all back
	r2 := &testfake.Runner{Out: r.Out, Fail: map[string]string{"qemu-img": "read error"}}
	z2 := &ZFS{Run: r2, Dataset: "tank/p8s/vms", Exists: func(string) bool { return true },
		Pools: func() map[string]string { return map[string]string{"nas": "nas1/p8s-vms"} }}
	if _, err := z2.Relocate(context.Background(), "p8s-db", "nas", []int{0}, func(string) {}); err == nil {
		t.Fatal("copy failure")
	}
	if z2.StorageOf("p8s-db") != "local" || !strings.Contains(strings.Join(r2.Calls, "\n"), "zfs destroy -r nas1/p8s-vms/p8s-db-disk0") {
		t.Fatalf("not undone: %v", r2.Calls)
	}
}
