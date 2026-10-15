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

func TestMoveContainer(t *testing.T) {
	ctx := context.Background()
	a, runA := newSvc(t)
	if _, err := a.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", MemoryMiB: 1024, DiskGB: 10, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	if why, _ := a.MoveCheck(ctx, "web"); why == "" {
		t.Fatal("running container may move")
	}
	runA.Out["lxc-info -n web -sH"] = "STOPPED\n"
	dir := t.TempDir()
	a.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "printf", "DISK-STREAM")
	}
	var buf bytes.Buffer
	if err := a.Export(ctx, "web", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(runA.Calls, "\n"), "zfs snapshot tank/p8s/ct/web@p8s-move") {
		t.Fatalf("%v", runA.Calls)
	}
	b, runB := newSvc(t)
	var recv []string
	b.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		recv = append(recv, strings.Join(args, " "))
		return exec.CommandContext(ctx, "sh", "-c", "cat > "+filepath.Join(dir, "disk"))
	}
	c, note, err := b.Import(ctx, &buf)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "disk"))
	if string(got) != "DISK-STREAM" || c.Owner != "alice" || c.MemoryMiB != 1024 || note != "" {
		t.Fatalf("%q %+v %q", got, c, note)
	}
	if !strings.Contains(recv[0], "receive -o mountpoint=") || !strings.Contains(recv[0], "refquota=10G tank/p8s/ct/web") {
		t.Fatalf("%v", recv)
	}
	cfg, _ := os.ReadFile(filepath.Join(b.LXCDir, "web", "config"))
	if !strings.Contains(string(cfg), "lxc.net.0.link = virbr0") {
		t.Fatalf("%s", cfg)
	}
	if _, _, err := b.Import(ctx, strings.NewReader("{\"name\":\"web\",\"vcpus\":1,\"memory_mib\":512,\"disk_gb\":8}\n")); !errors.Is(err, vm.ErrExists) {
		t.Fatalf("twice: %v", err)
	}
	_ = runB
}

type fakeVNets struct{}

func (fakeVNets) BridgeOf(_ context.Context, name string) (string, error) {
	if name == "office" {
		return "vxb10000", nil
	}
	return "", errors.New("no such virtual network")
}
func (fakeVNets) NameOf(string) string { return "" }
func (fakeVNets) Names() []string      { return []string{"office"} }

func TestContainerOnVNet(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t)
	if _, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", Network: "vnet:office"}); err == nil {
		t.Fatal("vnet without virtual networks")
	}
	s.VNets = fakeVNets{}
	c, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", Network: "vnet:office"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(s.LXCDir, "web", "config"))
	if c.Network != "vnet:office" || !strings.Contains(string(cfg), "lxc.net.0.link = vxb10000") || !strings.Contains(string(cfg), "lxc.net.0.mtu = 1230") {
		t.Fatalf("%+v\n%s", c, cfg)
	}
}

func TestMoveRunningContainer(t *testing.T) {
	ctx := context.Background()
	a, runA := newSvc(t)
	if _, err := a.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", MemoryMiB: 1024, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	var sent []string
	a.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		sent = append(sent, strings.Join(args, " "))
		return exec.CommandContext(ctx, "printf", "ROUND")
	}
	// the final round needs it stopped; the first works while it runs
	var buf bytes.Buffer
	if err := a.ExportPhase(ctx, "web", PhaseFinal, &buf); !errors.Is(err, vm.ErrConflict) {
		t.Fatalf("final while running: %v", err)
	}
	buf.Reset()
	if err := a.ExportPhase(ctx, "web", PhasePre, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent[0], "send -c tank/p8s/ct/web@p8s-move-pre") {
		t.Fatalf("%v", sent)
	}
	b, runB := newSvc(t)
	dir := t.TempDir()
	var recv []string
	b.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		recv = append(recv, strings.Join(args, " "))
		return exec.CommandContext(ctx, "sh", "-c", "cat >> "+filepath.Join(dir, "disk"))
	}
	runB.Fail = map[string]string{"nsenter --mount=/proc/1/ns/mnt -- zfs destroy -r tank/p8s/ct/web": "dataset does not exist"}
	if _, _, err := b.Import(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if b.Exists("web") || !strings.Contains(recv[0], "receive -u -o mountpoint=") {
		t.Fatalf("first round made a container / %v", recv)
	}
	// shut down; the last changes
	runA.Out["lxc-info -n web -sH"] = "STOPPED\n"
	buf.Reset()
	if err := a.ExportPhase(ctx, "web", PhaseFinal, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent[1], "send -c -i tank/p8s/ct/web@p8s-move-pre tank/p8s/ct/web@p8s-move") {
		t.Fatalf("%v", sent)
	}
	if calls := strings.Join(runA.Calls, "\n"); !strings.Contains(calls, "zfs destroy tank/p8s/ct/web@p8s-move-pre") {
		t.Fatal("pre snapshot kept on the old server")
	}
	c, _, err := b.Import(ctx, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "disk")); string(got) != "ROUNDROUND" || c.MemoryMiB != 1024 || !b.Exists("web") {
		t.Fatalf("%q %+v", got, c)
	}
	if !strings.Contains(recv[1], "receive -u -F tank/p8s/ct/web") {
		t.Fatalf("%v", recv)
	}
	calls := strings.Join(runB.Calls, "\n")
	if !strings.Contains(calls, "zfs destroy tank/p8s/ct/web@p8s-move-pre") || !strings.Contains(calls, "zfs mount tank/p8s/ct/web") {
		t.Fatal(calls)
	}
	// a move that stopped halfway: its disk is removed, never a real container
	if err := b.DropMove(ctx, "web"); !errors.Is(err, vm.ErrConflict) {
		t.Fatalf("drop of a real container: %v", err)
	}
}
