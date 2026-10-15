package ct

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/vm"
)

func TestContainerReplica(t *testing.T) {
	ctx := context.Background()
	a, runA := newSvc(t)
	if _, err := a.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", MemoryMiB: 1024, DiskGB: 10, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	if why, err := a.HACheck(ctx, "web"); why != "" || err != nil {
		t.Fatalf("%q %v", why, err)
	}
	var sent []string
	a.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		sent = append(sent, strings.Join(args, " "))
		return exec.CommandContext(ctx, "printf", "DELTA")
	}
	// first copy: full (the other server has nothing), works while it runs
	var buf bytes.Buffer
	if err := a.SendReplica(ctx, "web", nil, "srv-a", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(runA.Calls, "\n"), "zfs snapshot tank/p8s/ct/web@p8s-rep-") ||
		strings.Contains(sent[0], " -i ") || !strings.Contains(sent[0], "send -c tank/p8s/ct/web@p8s-rep-") {
		t.Fatalf("%v %v", sent, runA.Calls)
	}
	head, _, _ := strings.Cut(buf.String(), "\n")
	if !strings.Contains(head, `"owner":"alice"`) || strings.Contains(head, `"base"`) {
		t.Fatal(head)
	}

	// the other server keeps it, unmounted
	b, runB := newSvc(t)
	dir := t.TempDir()
	var recv []string
	b.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		recv = append(recv, strings.Join(args, " "))
		return exec.CommandContext(ctx, "sh", "-c", "cat > "+filepath.Join(dir, "disk"))
	}
	runB.Fail = map[string]string{"nsenter --mount=/proc/1/ns/mnt -- zfs destroy -r tank/p8s/ct-replica/web": "dataset does not exist"}
	rep, err := b.RecvReplica(ctx, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "disk")); string(got) != "DELTA" {
		t.Fatalf("%q", got)
	}
	if rep.Kind != "ct" || rep.Name != "web" || rep.From != "srv-a" || rep.MemoryMiB != 1024 {
		t.Fatalf("%+v", rep)
	}
	if !strings.Contains(recv[0], "receive -u -o canmount=off tank/p8s/ct-replica/web") || strings.Contains(recv[0], "-F") {
		t.Fatalf("%v", recv)
	}
	if b.Exists("web") {
		t.Fatal("a copy isn't a container")
	}
	snap := strings.Split(strings.Split(head, `"snap":"`)[1], `"`)[0]
	runB.Out["zfs list -H -t snapshot -o name -s creation -d 1 tank/p8s/ct-replica/web"] = "tank/p8s/ct-replica/web@" + snap + "\n"
	if l, _ := b.ReplicaSnaps(ctx, "web"); len(l) != 1 || l[0] != snap {
		t.Fatalf("%v", l)
	}
	if l := b.Replicas(ctx); len(l) != 1 || l[0].Name != "web" || l[0].At == "" {
		t.Fatalf("%+v", l)
	}

	// next copy: only the change since the snapshot both have
	runA.Out["zfs list -H -t snapshot -o name -s creation -d 1 tank/p8s/ct/web"] = "tank/p8s/ct/web@p8s-rep-1\ntank/p8s/ct/web@" + snap + "\n"
	buf.Reset()
	if err := a.SendReplica(ctx, "web", []string{snap}, "srv-a", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent[1], "send -c -i tank/p8s/ct/web@"+snap+" tank/p8s/ct/web@p8s-rep-") {
		t.Fatalf("%v", sent)
	}
	if calls := strings.Join(runA.Calls, "\n"); !strings.Contains(calls, "zfs destroy tank/p8s/ct/web@p8s-rep-1") ||
		strings.Contains(calls, "zfs destroy tank/p8s/ct/web@"+snap) {
		t.Fatalf("old copy points: %v", runA.Calls)
	}
	if _, err := b.RecvReplica(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recv[1], "receive -u -o canmount=off -F tank/p8s/ct-replica/web") {
		t.Fatalf("%v", recv)
	}
	// copy points are not the user's snapshots
	runA.Out["zfs list -H -p -t snapshot"] = "tank/p8s/ct/web@mine\t100\ntank/p8s/ct/web@" + snap + "\t200\n"
	if l, _ := a.Snapshots(ctx, "web"); len(l) != 1 || l[0].Name != "mine" {
		t.Fatalf("%+v", l)
	}

	// the server with web dies: the copy becomes the container (stopped, no autostart)
	runB.Fail["zfs list -H -o name tank/p8s/ct/web"] = "dataset does not exist"
	c, note, err := b.PromoteReplica(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if c.Owner != "alice" || c.Autostart || c.MemoryMiB != 1024 || !strings.Contains(note, "from the copy of") {
		t.Fatalf("%+v %q", c, note)
	}
	calls := strings.Join(runB.Calls, "\n")
	for _, want := range []string{"zfs rename tank/p8s/ct-replica/web tank/p8s/ct/web", "canmount=on refquota=10G tank/p8s/ct/web", "zfs mount tank/p8s/ct/web"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("no %q in %s", want, calls)
		}
	}
	if !b.Exists("web") || len(b.Replicas(ctx)) != 0 {
		t.Fatal("promote")
	}
	if _, _, err := b.PromoteReplica(ctx, "web"); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("twice: %v", err)
	}

	// it comes back: its old container is switched off and becomes the copy
	runA.Out["lxc-info -n web -sH"] = "RUNNING\n"
	if got := a.Fence(ctx, []string{"web", "other"}); len(got) != 1 || got[0] != "web" {
		t.Fatalf("fence %v", got)
	}
	runA.Out["lxc-info -n web -sH"] = "STOPPED\n"
	if err := a.RetireCopy(ctx, "web", "srv-b"); err != nil {
		t.Fatal(err)
	}
	if a.Exists("web") {
		t.Fatal("still a container")
	}
	if calls := strings.Join(runA.Calls, "\n"); !strings.Contains(calls, "zfs rename tank/p8s/ct/web tank/p8s/ct-replica/web") {
		t.Fatal(calls)
	}
	if l := a.Replicas(ctx); len(l) != 1 || l[0].From != "srv-b" {
		t.Fatalf("%+v", l)
	}
	if err := a.RetireCopy(ctx, "web", "srv-b"); err != nil { // nothing left to retire
		t.Fatal(err)
	}
	if err := a.DropReplica(ctx, "web"); err != nil || len(a.Replicas(ctx)) != 0 {
		t.Fatal(err)
	}

	// public addresses stay with their server
	if err := b.put("pub", &meta{VCPUs: 1, MemoryMiB: 512, DiskGB: 8, Network: vm.NetPublic}); err != nil {
		t.Fatal(err)
	}
	if why, _ := b.HACheck(ctx, "pub"); !strings.Contains(why, "public IP") {
		t.Fatal(why)
	}
	if err := b.SendReplica(ctx, "pub", nil, "x", &buf); !errors.Is(err, vm.ErrConflict) {
		t.Fatal(err)
	}
}
