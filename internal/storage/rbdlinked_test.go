package storage

import (
	"context"
	"strings"
	"testing"
)

func TestCephLinkedClones(t *testing.T) {
	f := &cephFake{devs: map[string]bool{}, out: map[string]string{
		"rbd ls --pool p8s-vms":                                           "p8s-db1-disk0\np8s-db1-disk1\n",
		"rbd status --format json":                                        `{"watchers":[]}`,
		"rbd snap ls --format json":                                       "[]",
		"rbd snap ls --format json p8s-vms/p8s-db1-disk0":                 `[{"name":"p8s-link-db2-1"},{"name":"mine"}]`,
		"rbd children --format json p8s-vms/p8s-db1-disk0@p8s-link-db2-1": `[{"pool":"p8s-vms","pool_namespace":"","image":"p8s-db2-disk0"}]`,
		"rbd info --format json p8s-vms/p8s-db2-disk0":                    `{"size":1,"parent":{"pool":"p8s-vms","image":"p8s-db1-disk0","snapshot":"p8s-link-db2-1"}}`,
		"rbd info --format json p8s-vms/p8s-db2-disk1":                    `{"size":1,"parent":{"pool":"p8s-vms","image":"p8s-db1-disk1","snapshot":"p8s-link-db2-1"}}`,
		"rbd info --format json p8s-vms/p8s-db1-disk0":                    `{"size":1}`,
	}}
	z := cephZFS(t, f)
	ctx := context.Background()
	_ = z.Place("p8s-db2", CephStorage)
	if err := z.LinkDisks(ctx, "p8s-db1", "p8s-link-db2-1", "p8s-db2", []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{
		"rbd snap protect p8s-vms/p8s-db1-disk0@p8s-link-db2-1",
		"rbd clone p8s-vms/p8s-db1-disk0@p8s-link-db2-1 p8s-vms/p8s-db2-disk0",
		"rbd clone p8s-vms/p8s-db1-disk1@p8s-link-db2-1 p8s-vms/p8s-db2-disk1",
		"rbd device map --options ms_mode=secure p8s-vms/p8s-db2-disk1",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in\n%s", want, all)
		}
	}
	if l := z.LinkedClones(ctx, "p8s-db1"); len(l) != 1 || l[0] != "p8s-db2" {
		t.Fatalf("clones %v", l)
	}
	if got := z.LinkedTo(ctx, "p8s-db2"); got != "p8s-db1" {
		t.Fatalf("linked to %q", got)
	}
	if got := z.LinkedTo(ctx, "p8s-db1"); got != "" {
		t.Fatalf("source linked to %q", got)
	}
	// make it independent: flatten, then the source's snapshot goes
	f.out["rbd ls --pool p8s-vms"] = "p8s-db1-disk0\np8s-db1-disk1\np8s-db2-disk0\np8s-db2-disk1\n"
	if err := z.Unlink(ctx, "p8s-db2", nil); err != nil {
		t.Fatal(err)
	}
	all = f.all()
	for _, want := range []string{
		"rbd flatten --no-progress p8s-vms/p8s-db2-disk0",
		"rbd flatten --no-progress p8s-vms/p8s-db2-disk1",
		"rbd snap unprotect p8s-vms/p8s-db1-disk0@p8s-link-db2-1",
		"rbd snap rm --no-progress p8s-vms/p8s-db1-disk1@p8s-link-db2-1",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in\n%s", want, all)
		}
	}
	// deleting a linked clone drops the source's snapshot too
	f.calls = nil
	if err := z.DestroyDisk(ctx, "p8s-db2"); err != nil {
		t.Fatal(err)
	}
	if all := f.all(); !strings.Contains(all, "rbd rm --no-progress p8s-vms/p8s-db2-disk0") || !strings.Contains(all, "rbd snap unprotect p8s-vms/p8s-db1-disk0@p8s-link-db2-1") {
		t.Fatalf("destroy:\n%s", all)
	}
	// a snapshot still used by another clone isn't removed
	f.calls = nil
	f.fail = map[string]string{"rbd snap unprotect": "Device or resource busy: child images"}
	z.cephDropOrigins(ctx, []string{"p8s-vms/p8s-db1-disk0@p8s-link-db3-1"})
	if strings.Contains(f.all(), "snap rm") {
		t.Fatal("removed a snapshot in use")
	}
}
