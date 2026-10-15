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
