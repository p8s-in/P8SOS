package ct

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/imports"
	"p8sos/internal/jobs"
)

func pveBackup(t *testing.T, dir, conf string, files map[string]string) string {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: "./etc/vzdump/", Mode: 0o755, Typeflag: tar.TypeDir})
	add("./etc/vzdump/pct.conf", conf)
	for _, d := range []string{"./", "./etc/", "./etc/network/", "./root/", "./dev/"} {
		tw.WriteHeader(&tar.Header{Name: d, Mode: 0o755, Typeflag: tar.TypeDir})
	}
	for n, b := range files {
		add(n, b)
	}
	tw.Close()
	zw.Close()
	f := filepath.Join(dir, "vzdump-lxc-105-2026_10_15-10_00_00.tar.gz")
	if err := os.WriteFile(f, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestImportPVEContainer(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar")
	}
	ctx := context.Background()
	s, run := newSvc(t)
	s.Jobs = &jobs.Manager{}
	var unpack []string
	s.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		unpack = append([]string{name}, args...)
		// lxc-usernsexec -m … -- tar …: run the tar part (no id shifting in a test)
		for i, a := range args {
			if a == "--" {
				return exec.CommandContext(ctx, args[i+1], args[i+2:]...)
			}
		}
		t.Fatalf("unexpected %s %v", name, args)
		return nil
	}
	conf := "hostname: shop\ncores: 2\nmemory: 768\nrootfs: local-lvm:vm-105-disk-0,size=4G\nunprivileged: 1\n" +
		"net0: name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:05,ip=192.168.1.50/24,gw=192.168.1.1,type=veth\n"
	f := pveBackup(t, t.TempDir(), conf, map[string]string{
		"./etc/hostname":           "shop\n",
		"./etc/hosts":              "127.0.0.1 localhost\n127.0.1.1 shop\n",
		"./etc/network/interfaces": "auto eth0\niface eth0 inet static\n\taddress 192.168.1.50/24\n",
		"./root/notes.txt":         "my files\n",
	})
	pl, err := imports.Inspect(f)
	if err != nil {
		t.Fatal(err)
	}
	no := false
	if _, err := s.StartImport(ctx, f, pl, ImportRequest{Name: "Bad Name", Start: &no}); err == nil {
		t.Fatal("bad name accepted")
	}
	j, err := s.StartImport(ctx, f, pl, ImportRequest{Name: "shop2", KeepMAC: true, Start: &no})
	if err != nil {
		t.Fatal(err)
	}
	s.Jobs.Wait()
	if j, _ = s.Jobs.Get(j.ID); j.State != jobs.Done || !strings.Contains(j.Message, "imported") {
		t.Fatalf("job %+v", j)
	}
	if unpack[0] != "lxc-usernsexec" || !strings.Contains(strings.Join(unpack, " "), "b:0:100000:65536 -- tar --numeric-owner -xpf -") {
		t.Fatalf("unpack %v", unpack)
	}
	root := s.rootfs("shop2")
	if b, _ := os.ReadFile(filepath.Join(root, "root/notes.txt")); string(b) != "my files\n" {
		t.Fatal("files not unpacked")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/vzdump")); err == nil {
		t.Fatal("pct.conf unpacked into the container")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/hostname")); string(b) != "shop2\n" {
		t.Fatalf("hostname %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/network/interfaces")); !strings.Contains(string(b), "iface eth0 inet dhcp") {
		t.Fatalf("network not switched to DHCP: %s", b)
	}
	if !strings.Contains(strings.Join(run.Calls, "\n"), "refquota=4G") {
		t.Fatalf("calls %v", run.Calls)
	}
	cfg, _ := os.ReadFile(filepath.Join(s.LXCDir, "shop2", "config"))
	if !strings.Contains(string(cfg), "bc:24:11:00:00:05") || !strings.Contains(string(cfg), "lxc.idmap = u 0 100000 65536") {
		t.Fatalf("config\n%s", cfg)
	}
	c, err := s.Get(ctx, "shop2")
	if err != nil || c.VCPUs != 2 || c.MemoryMiB != 768 || !strings.HasPrefix(c.Template, "import:") {
		t.Fatalf("%+v %v", c, err)
	}
	// the same name again
	if _, err := s.StartImport(ctx, f, pl, ImportRequest{Name: "shop2"}); err == nil {
		t.Fatal("duplicate accepted")
	}
}
