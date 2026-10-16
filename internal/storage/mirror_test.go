package storage

import (
	"context"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestMirrorTarget(t *testing.T) {
	ctx := context.Background()
	// disk0's copy exists (smaller: the disk grew), disk1's not, disk3's copy is left from a removed data disk
	r := &testfake.Runner{
		Out: map[string]string{
			"zfs get -H -p -o value volsize tank/vms/replica/p8s-db-disk0": "10737418240\n",
			"zfs list -H -o name -t volume -d 1 tank/vms/replica":          "tank/vms/replica/p8s-db-disk0\ntank/vms/replica/p8s-db-disk3\n",
		},
		Fail: map[string]string{"zfs get -H -p -o value volsize tank/vms/replica/p8s-db-disk1": "dataset does not exist"},
	}
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { return true }}
	disks := []MirrorDisk{{0, 21474836480}, {1, 5368709120}}
	paths, err := z.PrepareMirror(ctx, "p8s-db", disks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, " ") != "/dev/zvol/tank/vms/replica/p8s-db-disk0 /dev/zvol/tank/vms/replica/p8s-db-disk1" {
		t.Fatal(paths)
	}
	calls := strings.Join(r.Calls, "\n")
	for _, want := range []string{
		"zfs destroy -r tank/vms/replica/p8s-db-disk3",
		"zfs snapshot tank/vms/replica/p8s-db-disk0@p8s-mirror-good",
		"zfs set volsize=21474836480 tank/vms/replica/p8s-db-disk0",
		"zfs create -s -V 5368709120 -o volmode=dev tank/vms/replica/p8s-db-disk1",
		"zfs set volmode=dev tank/vms/replica/p8s-db-disk0",
	} {
		if !strings.Contains(calls, want) {
			t.Fatalf("no %q in\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "snapshot tank/vms/replica/p8s-db-disk1") {
		t.Fatal("snapshot of a copy that didn't exist")
	}
	a := strings.Join(MirrorExportArgs("10.250.0.3", 10812, disks, paths), " ")
	for _, want := range []string{"driver=host_device,node-name=d0,filename=/dev/zvol/tank/vms/replica/p8s-db-disk0,cache.direct=on",
		"addr.host=10.250.0.3,addr.port=10812", "type=nbd,id=e1,node-name=d1,name=disk1,writable=on"} {
		if !strings.Contains(a, want) {
			t.Fatalf("no %q in %s", want, a)
		}
	}
	if err := z.StartMirrorExport(ctx, "p8s-db", "10.250.0.3", 10812, disks, paths); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.Calls, "\n"), "systemd-run --unit=p8s-mirror-p8s-db") {
		t.Fatal("not served")
	}
	// a sync cut short: back to the last good state (disk3 has none)
	r.Fail["zfs list -H -o name -t snapshot tank/vms/replica/p8s-db-disk3@"] = "does not exist"
	rolled, err := z.MirrorRollback(ctx, "p8s-db")
	if err != nil || !rolled {
		t.Fatal(rolled, err)
	}
	calls = strings.Join(r.Calls, "\n")
	if !strings.Contains(calls, "zfs rollback -r tank/vms/replica/p8s-db-disk0@p8s-mirror-good") || strings.Contains(calls, "rollback -r tank/vms/replica/p8s-db-disk3") {
		t.Fatal(calls)
	}
}
