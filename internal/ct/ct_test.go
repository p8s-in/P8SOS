package ct

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/testfake"
	"p8sos/internal/vm"
)

func newSvc(t *testing.T) (*Service, *testfake.Runner) {
	dir := t.TempDir()
	run := &testfake.Runner{Out: map[string]string{
		"zfs list -H -t snapshot -o name,p8s:label": "tank/p8s/ct-templates/debian-13@base\tDebian 13 (trixie)\ntank/p8s/ct-templates/alpine-3.22@base\t-\ntank/p8s/ct-templates/half@other\t-\n",
		"lxc-info -n web -sH":                       "RUNNING\n",
		"lxc-info -n web -iH":                       "fd42::1\n10.0.3.15\n",
		"zfs get -Hp -o value used":                 "123456\n",
	}}
	var chowns []string
	s := &Service{Run: run, Pool: "tank", MountDir: filepath.Join(dir, "ct"), LXCDir: filepath.Join(dir, "lxc"),
		MetaFile: filepath.Join(dir, "containers.json"), Bridge: "virbr0", Limits: Limits{MaxVCPUs: 8, MaxMemoryMiB: 16384, MaxDiskGB: 200},
		Chown: func(p string, u, g int) error { chowns = append(chowns, p); return nil },
		Now:   func() time.Time { return time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC) }}
	root := filepath.Join(s.MountDir, "web") // what "zfs clone" would mount
	_ = os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "etc/shadow"), []byte("root:*:19000:0:99999:7:::\nbin:*:19000:0:99999:7:::\n"), 0o640)
	_ = os.WriteFile(filepath.Join(root, "etc/hosts"), []byte("127.0.0.1 localhost\n127.0.1.1 LXC_NAME\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "etc/hostname"), []byte("LXC_NAME\n"), 0o644)
	return s, run
}

func last(run *testfake.Runner, prefix string) string {
	for i := len(run.Calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(run.Calls[i], prefix) {
			return run.Calls[i]
		}
	}
	return ""
}

func TestTemplates(t *testing.T) {
	s, _ := newSvc(t)
	l := s.Templates(context.Background())
	if len(l) != 2 || l[0].Name != "alpine-3.22" || l[0].Label != "alpine-3.22" || l[1].Label != "Debian 13 (trixie)" {
		t.Fatalf("%+v", l)
	}
}

func TestCreateAndManage(t *testing.T) {
	s, run := newSvc(t)
	ctx := context.Background()
	for _, bad := range []CreateRequest{
		{Name: "Web", Template: "debian-13"},
		{Name: "web", Template: "nope"},
		{Name: "web", Template: "debian-13", VCPUs: 99},
		{Name: "web", Template: "debian-13", RootPassword: "short"},
		{Name: "web", Template: "debian-13", Network: "lan"},
		{Name: "web", Template: "debian-13", SSHKeys: []string{"ssh-ed25519 AAA\nevil"}},
	} {
		if _, err := s.Create(ctx, bad); !errors.Is(err, vm.ErrInvalid) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	c, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13", VCPUs: 2, MemoryMiB: 1024, DiskGB: 10,
		RootPassword: "secret-pass-1", SSHKeys: []string{"ssh-ed25519 AAAAkey me"}, Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "running" || c.IP != "10.0.3.15" || c.Owner != "alice" || c.Kind != "ct" || c.DiskUsed != 123456 {
		t.Fatalf("%+v", c)
	}
	if l := last(run, "nsenter --mount=/proc/1/ns/mnt -- zfs clone"); !strings.Contains(l, "refquota=10G") || !strings.HasSuffix(l, "tank/p8s/ct-templates/debian-13@base tank/p8s/ct/web") {
		t.Fatalf("clone: %q", l)
	}
	if l := last(run, "systemd-run"); !strings.Contains(l, "--unit=p8s-ct-web") || !strings.HasSuffix(l, "lxc-start -F -n web") {
		t.Fatalf("start: %q", l)
	}
	conf, _ := os.ReadFile(filepath.Join(s.LXCDir, "web", "config"))
	for _, want := range []string{"lxc.idmap = u 0 100000 65536", "lxc.net.0.link = virbr0", "lxc.cgroup2.memory.max = 1024M",
		"lxc.cgroup2.cpu.max = 200000 100000", "lxc.rootfs.path = dir:" + filepath.Join(s.MountDir, "web")} {
		if !strings.Contains(string(conf), want) {
			t.Fatalf("config lacks %q:\n%s", want, conf)
		}
	}
	ud, _ := os.ReadFile(filepath.Join(s.MountDir, "web", "var/lib/cloud/seed/nocloud/user-data"))
	if !strings.Contains(string(ud), "ssh-ed25519 AAAAkey me") || !strings.Contains(string(ud), "$6$") || strings.Contains(string(ud), "secret-pass-1") {
		t.Fatalf("user-data:\n%s", ud)
	}
	root := filepath.Join(s.MountDir, "web")
	sh, _ := os.ReadFile(filepath.Join(root, "etc/shadow"))
	hn, _ := os.ReadFile(filepath.Join(root, "etc/hostname"))
	hosts, _ := os.ReadFile(filepath.Join(root, "etc/hosts"))
	ak, _ := os.ReadFile(filepath.Join(root, "root/.ssh/authorized_keys"))
	if !strings.HasPrefix(string(sh), "root:$6$") || !strings.Contains(string(sh), "\nbin:*:") || string(hn) != "web\n" ||
		!strings.Contains(string(hosts), "127.0.1.1 web") || string(ak) != "ssh-ed25519 AAAAkey me\n" {
		t.Fatalf("prepared files:\n%s\n%s\n%s\n%s", sh, hn, hosts, ak)
	}
	if st, _ := os.Stat(filepath.Join(root, "etc/shadow")); st.Mode().Perm() != 0o640 {
		t.Fatalf("shadow mode %v", st.Mode())
	}
	// first boot installs SSH (in the background)
	for i := 0; i < 100 && last(run, "lxc-attach") == ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if l := last(run, "lxc-attach -n web"); !strings.Contains(l, "ROOT_LOGIN=1") || !strings.Contains(l, "openssh") {
		t.Fatalf("ssh setup: %q", l)
	}
	if _, err := s.Create(ctx, CreateRequest{Name: "web", Template: "debian-13"}); !errors.Is(err, vm.ErrExists) {
		t.Fatalf("dup: %v", err)
	}

	// resize while running: cgroups live, quota on ZFS
	c, err = s.Resize(ctx, "web", ResizeRequest{VCPUs: 4, MemoryMiB: 2048, DiskGB: 20})
	if err != nil || c.VCPUs != 4 || c.DiskGB != 20 {
		t.Fatal(err, c)
	}
	if last(run, "lxc-cgroup -n web memory.max") != "lxc-cgroup -n web memory.max 2147483648" || !strings.HasSuffix(last(run, "nsenter --mount=/proc/1/ns/mnt -- zfs set"), "refquota=20G tank/p8s/ct/web") {
		t.Fatalf("resize calls %v", run.Calls[len(run.Calls)-6:])
	}

	// password via stdin
	var got string
	s.Stdin = func(_ context.Context, in, name string, args ...string) error {
		got = in + strings.Join(args, " ")
		return nil
	}
	if err := s.SetPassword(ctx, "web", "root", "new-pass-123"); err != nil || !strings.HasPrefix(got, "root:new-pass-123\n") {
		t.Fatal(err, got)
	}

	// snapshots: rollback needs it stopped
	if _, err := s.CreateSnapshot(ctx, "web", "before"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rollback(ctx, "web", "before", false); !errors.Is(err, vm.ErrConflict) {
		t.Fatalf("rollback while running: %v", err)
	}
	run.Out["lxc-info -n web -sH"] = "STOPPED\n"
	if _, err := s.Rollback(ctx, "web", "before", true); err != nil || last(run, "nsenter") != "nsenter --mount=/proc/1/ns/mnt -- zfs rollback -r tank/p8s/ct/web@before" {
		t.Fatal(err, last(run, "nsenter"))
	}
	run.Out["zfs list -H -p -t snapshot -o name,creation"] = "tank/p8s/ct/web@before\t1791800000\n"
	if l, _ := s.Snapshots(ctx, "web"); len(l) != 1 || l[0].Name != "before" {
		t.Fatalf("snaps %+v", l)
	}

	// owner and listing (reloaded from disk)
	if _, err := s.SetOwner(ctx, "web", "bob"); err != nil {
		t.Fatal(err)
	}
	s2 := &Service{Run: run, Pool: "tank", MountDir: s.MountDir, LXCDir: s.LXCDir, MetaFile: s.MetaFile, Bridge: "virbr0"}
	l, _ := s2.List(ctx)
	if len(l) != 1 || l[0].Owner != "bob" || l[0].State != "stopped" || l[0].IP != "" {
		t.Fatalf("list %+v", l)
	}
	if err := s2.Delete(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.LXCDir, "web")); err == nil || s2.Exists("web") {
		t.Fatal("not deleted")
	}
}

func TestSeedRefusesSymlinks(t *testing.T) {
	s, _ := newSvc(t)
	root := filepath.Join(s.MountDir, "web")
	_ = os.MkdirAll(filepath.Join(root, "var/lib"), 0o755)
	_ = os.Symlink("/etc", filepath.Join(root, "var/lib/cloud"))
	if err := s.seed("web", nil, ""); err == nil {
		t.Fatal("followed a symlink out of the container")
	}
}
