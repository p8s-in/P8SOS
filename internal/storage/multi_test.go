package storage

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

// fakeStreams: "zfs send … DS@SNAP" prints "data of DS" (big for disk 1, to
// cross frame boundaries); "zfs receive … DS" saves what it reads to dir.
func fakeStreams(t *testing.T, dir string) *[]string {
	var calls []string
	StreamCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		c := name + " " + strings.Join(args, " ")
		calls = append(calls, c)
		last := args[len(args)-1]
		if args[0] == "send" {
			ds, _, _ := strings.Cut(last, "@")
			if strings.HasSuffix(ds, "-disk1") {
				return exec.CommandContext(ctx, "sh", "-c", "head -c 3000000 /dev/zero | tr '\\0' 'x'")
			}
			return exec.CommandContext(ctx, "printf", "data of "+ds)
		}
		return exec.CommandContext(ctx, "sh", "-c", "cat > "+filepath.Join(dir, strings.ReplaceAll(last, "/", "_")))
	}
	t.Cleanup(func() { StreamCmd = exec.CommandContext })
	return &calls
}

func TestMoveDisks(t *testing.T) {
	dir := t.TempDir()
	calls := fakeStreams(t, dir)
	r := &testfake.Runner{Out: map[string]string{}}
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { return true }}
	ctx := context.Background()
	var buf bytes.Buffer
	if err := z.SendDisks(ctx, "p8s-a", []int{1, 3}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.Calls, "\n"), "zfs snapshot tank/vms/p8s-a-disk0@p8s-move tank/vms/p8s-a-disk1@p8s-move tank/vms/p8s-a-disk3@p8s-move") {
		t.Fatalf("not one snapshot for all disks:\n%s", strings.Join(r.Calls, "\n"))
	}
	root, extra, err := z.RecvDisks(ctx, "p8s-b", []int{1, 3}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if root != "/dev/zvol/tank/vms/p8s-b-disk0" || extra[3] != "/dev/zvol/tank/vms/p8s-b-disk3" {
		t.Fatal(root, extra)
	}
	b0, _ := os.ReadFile(filepath.Join(dir, "tank_vms_p8s-b-disk0"))
	b1, _ := os.ReadFile(filepath.Join(dir, "tank_vms_p8s-b-disk1"))
	b3, _ := os.ReadFile(filepath.Join(dir, "tank_vms_p8s-b-disk3"))
	if string(b0) != "data of tank/vms/p8s-a-disk0" || len(b1) != 3000000 || string(b3) != "data of tank/vms/p8s-a-disk3" {
		t.Fatalf("streams mixed up: %q %d %q", b0, len(b1), b3)
	}
	if buf.Len() != 0 {
		t.Fatal("stream not read to its end")
	}
	_ = calls
}

func TestReplicaDisks(t *testing.T) {
	dir := t.TempDir()
	calls := fakeStreams(t, dir)
	r := &testfake.Runner{Out: map[string]string{
		"zfs list -H -t snapshot -o name -s createtxg -d 1 tank/vms/p8s-a-disk0": "tank/vms/p8s-a-disk0@p8s-rep-1\n",
		"zfs list -H -t snapshot -o name -s createtxg -d 1 tank/vms/p8s-a-disk1": "tank/vms/p8s-a-disk1@p8s-rep-1\n",
		"zfs list -H -o name -t snapshot tank/vms/p8s-a-disk0@p8s-rep-1":         "x",
		"zfs list -H -o name -t snapshot tank/vms/p8s-a-disk1@p8s-rep-1":         "x",
	}, Fail: map[string]string{
		"zfs list -H -o name -t snapshot tank/vms/p8s-a-disk2@p8s-rep-1": "does not exist",
	}}
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { return true }}
	ctx := context.Background()
	var snap string
	var bases []string
	var buf bytes.Buffer
	err := z.SendReplicaDisks(ctx, "p8s-a", []int{1, 2}, "p8s-rep-1", func(s string, b []string) error { snap, bases = s, b; return nil }, &buf)
	if err != nil {
		t.Fatal(err)
	}
	// disk 2 is new since the last copy: a full copy of it
	if strings.Join(bases, ",") != "p8s-rep-1,p8s-rep-1," {
		t.Fatalf("bases %q", bases)
	}
	if (*calls)[0] != "zfs send -c -i @p8s-rep-1 tank/vms/p8s-a-disk0@"+snap || (*calls)[2] != "zfs send -c tank/vms/p8s-a-disk2@"+snap {
		t.Fatalf("%v", *calls)
	}
	r.Out["zfs list -H -o name -t volume -d 1 tank/vms/replica"] = "tank/vms/replica/p8s-a-disk0\ntank/vms/replica/p8s-a-disk1\ntank/vms/replica/p8s-a-disk5\n"
	r.Calls = nil
	if err := z.RecvReplicaDisks(ctx, "p8s-a", []int{1, 2}, bases, &buf); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(r.Calls, "\n")
	for _, want := range []string{"zfs destroy -r tank/vms/replica/p8s-a-disk5", "zfs rollback -r tank/vms/replica/p8s-a-disk0@p8s-rep-1",
		"zfs rollback -r tank/vms/replica/p8s-a-disk1@p8s-rep-1", "zfs destroy -r tank/vms/replica/p8s-a-disk2"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tank_vms_replica_p8s-a-disk2")); string(b) != "data of tank/vms/p8s-a-disk2" {
		t.Fatalf("disk2 copy %q", b)
	}
	// promote: every disk's copy becomes the disk
	r.Calls = nil
	r.Out["zfs list -H -o name -t volume -d 1 tank/vms/replica"] = "tank/vms/replica/p8s-a-disk0\ntank/vms/replica/p8s-a-disk1\ntank/vms/replica/p8s-a-disk2\n"
	if _, err := z.PromoteReplica(ctx, "p8s-a"); err != nil {
		t.Fatal(err)
	}
	if all = strings.Join(r.Calls, "\n"); !strings.Contains(all, "zfs rename tank/vms/replica/p8s-a-disk2 tank/vms/p8s-a-disk2") {
		t.Fatalf("%s", all)
	}
}
