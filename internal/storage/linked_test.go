package storage

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestLinkedClones(t *testing.T) {
	ctx := context.Background()
	seen := map[string]int{}
	r := &testfake.Runner{Out: map[string]string{
		"zfs list -H -t snapshot -o name,clones -d 1 tank/vms/p8s-base-disk0": "tank/vms/p8s-base-disk0@p8s-link-c1-1\ttank/vms/p8s-c1-disk0\ntank/vms/p8s-base-disk0@mine\t-\n",
		"zfs get -H -o value origin tank/vms/p8s-c1-disk0":                    "tank/vms/p8s-base-disk0@p8s-link-c1-1\n",
	}}
	// a device is missing the first time it is looked for (new), there after
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(p string) bool { seen[p]++; return seen[p] > 1 }}
	if err := z.LinkDisks(ctx, "p8s-base", "p8s-link-c1-1", "p8s-c1", []int{0}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.Calls, "\n"), "zfs clone -o p8s:linked=p8s-base tank/vms/p8s-base-disk0@p8s-link-c1-1 tank/vms/p8s-c1-disk0") {
		t.Fatalf("calls %v", r.Calls)
	}
	if l := z.LinkedClones(ctx, "p8s-base"); len(l) != 1 || l[0] != "p8s-c1" {
		t.Fatalf("clones %v", l)
	}
	if s := z.LinkedTo(ctx, "p8s-c1"); s != "p8s-base" {
		t.Fatalf("linked to %q", s)
	}
	if s := z.LinkedTo(ctx, "p8s-base"); s != "" {
		t.Fatalf("base linked to %q", s)
	}
	// deleting the clone takes its source snapshot along
	r.Calls = nil
	if err := z.DestroyDisk(ctx, "p8s-c1"); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(r.Calls, "\n")
	if !strings.Contains(calls, "zfs destroy -r tank/vms/p8s-c1-disk0") || !strings.Contains(calls, "zfs destroy tank/vms/p8s-base-disk0@p8s-link-c1-1") {
		t.Fatalf("calls\n%s", calls)
	}
	// unlink: copy, swap, drop the link snapshot
	var streams []string
	StreamCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		streams = append(streams, name+" "+strings.Join(args, " "))
		if args[0] == "send" {
			return exec.CommandContext(ctx, "printf", "DATA")
		}
		return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null")
	}
	t.Cleanup(func() { StreamCmd = exec.CommandContext })
	r.Calls = nil
	if err := z.Unlink(ctx, "p8s-c1", nil); err != nil {
		t.Fatal(err)
	}
	calls = strings.Join(r.Calls, "\n")
	for _, want := range []string{"zfs snapshot tank/vms/p8s-c1-disk0@p8s-unlink", "zfs destroy -r tank/vms/p8s-c1-disk0",
		"zfs rename tank/vms/p8s-c1-disk0-unlink tank/vms/p8s-c1-disk0", "zfs destroy tank/vms/p8s-base-disk0@p8s-link-c1-1"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("no %q in\n%s", want, calls)
		}
	}
	if len(streams) != 2 || streams[0] != "zfs send tank/vms/p8s-c1-disk0@p8s-unlink" {
		t.Fatalf("streams %v", streams)
	}
	if err := z.Unlink(ctx, "p8s-base", nil); err == nil {
		t.Fatal("unlinked a VM that isn't a clone")
	}
}
