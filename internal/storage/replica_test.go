package storage

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestReplicaCommands(t *testing.T) {
	var streams []string
	StreamCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		streams = append(streams, name+" "+strings.Join(args, " "))
		return exec.CommandContext(ctx, "true")
	}
	defer func() { StreamCmd = exec.CommandContext }()
	r := &testfake.Runner{Out: map[string]string{
		"zfs list -H -t snapshot -o name -s createtxg -d 1 tank/vms/p8s-a-disk0":         "tank/vms/p8s-a-disk0@mine\ntank/vms/p8s-a-disk0@p8s-rep-1\ntank/vms/p8s-a-disk0@p8s-rep-2\n",
		"zfs list -H -t snapshot -o name -s createtxg -d 1 tank/vms/replica/p8s-a-disk0": "tank/vms/replica/p8s-a-disk0@p8s-rep-1\ntank/vms/replica/p8s-a-disk0@p8s-rep-2\n",
	}}
	z := &ZFS{Run: r, Dataset: "tank/vms", Exists: func(string) bool { return true }}
	ctx := context.Background()
	var snap string
	var buf bytes.Buffer
	if err := z.SendReplica(ctx, "p8s-a", "p8s-rep-1", func(s string) error { snap = s; return nil }, &buf); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.Calls, "\n")
	if !strings.Contains(joined, "zfs destroy tank/vms/p8s-a-disk0@p8s-rep-2") || strings.Contains(joined, "destroy tank/vms/p8s-a-disk0@p8s-rep-1") ||
		strings.Contains(joined, "destroy tank/vms/p8s-a-disk0@mine") || !strings.Contains(joined, "zfs snapshot tank/vms/p8s-a-disk0@"+snap) {
		t.Fatalf("send calls:\n%s", joined)
	}
	if streams[0] != "zfs send -c -i @p8s-rep-1 tank/vms/p8s-a-disk0@"+snap {
		t.Fatalf("stream %q", streams[0])
	}
	r.Calls = nil
	if err := z.RecvReplica(ctx, "p8s-a", "p8s-rep-1", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(r.Calls, "\n")
	if !strings.Contains(joined, "zfs rollback -r tank/vms/replica/p8s-a-disk0@p8s-rep-1") || streams[1] != "zfs receive -u -o volmode=none -F tank/vms/replica/p8s-a-disk0" ||
		!strings.Contains(joined, "zfs destroy tank/vms/replica/p8s-a-disk0@p8s-rep-1") {
		t.Fatalf("recv calls:\n%s\n%v", joined, streams)
	}
	// user snapshots stay visible, ours are hidden
	r.Out["zfs list -H -p -t snapshot"] = "tank/vms/p8s-a-disk0@mine\t1\t2\ntank/vms/p8s-a-disk0@p8s-rep-2\t1\t2\n"
	if l, _ := z.Snapshots(ctx, "p8s-a"); len(l) != 1 || l[0].Name != "mine" {
		t.Fatalf("snapshots %+v", l)
	}
	r.Calls = nil
	if _, err := z.PromoteReplica(ctx, "p8s-a"); err != nil {
		t.Fatal(err)
	}
	if r.Calls[0] != "zfs rename tank/vms/replica/p8s-a-disk0 tank/vms/p8s-a-disk0" || r.Calls[1] != "zfs set volmode=dev tank/vms/p8s-a-disk0" {
		t.Fatalf("promote %v", r.Calls)
	}
	r.Calls = nil
	if err := z.RetireDisk(ctx, "p8s-a"); err != nil {
		t.Fatal(err)
	}
	if last := r.Calls[len(r.Calls)-1]; last != "zfs rename tank/vms/p8s-a-disk0 tank/vms/replica/p8s-a-disk0" {
		t.Fatalf("retire %v", r.Calls)
	}
}
