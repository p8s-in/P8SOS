package imports

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pctConf = `arch: amd64
cores: 2
features: nesting=1
hostname: Web.Server
memory: 1024
mp0: local-lvm:vm-105-disk-1,mp=/data,backup=1,size=4G
mp1: local-lvm:vm-105-disk-2,mp=/scratch,size=2G
net0: name=eth0,bridge=vmbr0,firewall=1,hwaddr=BC:24:11:AA:BB:CC,ip=192.168.1.50/24,gw=192.168.1.1,type=veth
ostype: debian
rootfs: local-lvm:vm-105-disk-0,size=8G
swap: 512
unprivileged: 1

[snap1]
cores: 1
`

// BuildLXCBackup makes a gzip'd vzdump-style container archive (pct.conf first).
func BuildLXCBackup(t *testing.T, conf string, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: "./etc/vzdump/", Mode: 0o755, Typeflag: tar.TypeDir})
	add("./etc/vzdump/pct.conf", conf)
	for _, d := range []string{"./", "./etc/", "./root/", "./dev/"} {
		tw.WriteHeader(&tar.Header{Name: d, Mode: 0o755, Typeflag: tar.TypeDir})
	}
	for n, b := range files {
		add(n, b)
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestLXCPlan(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "vzdump-lxc-105-2026_10_15-10_00_00.tar.gz")
	os.WriteFile(f, BuildLXCBackup(t, pctConf, map[string]string{"./etc/hostname": "web\n"}), 0o644)
	if KindOf(filepath.Base(f)) != KindLXC || KindOf("vzdump-lxc-1.tar.zst") != KindLXC || KindOf("vzdump-lxc-1.tgz") != KindLXC || KindOf("backup.tar") != "" {
		t.Fatal("kind")
	}
	p, err := Inspect(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindLXC || p.Name != "web-server" || p.VCPUs != 2 || p.MemoryMiB != 1024 || p.DiskGB != 12 ||
		p.MAC != "bc:24:11:aa:bb:cc" || p.StaticIP != "192.168.1.50/24" || p.CTOS != "debian" {
		t.Fatalf("plan %+v", p)
	}
	all := strings.Join(p.Warnings, "\n")
	for _, w := range []string{"mp0 (/data) is in the backup", "mp1 (/scratch) wasn't in the backup", "fixed address", "features"} {
		if !strings.Contains(all, w) {
			t.Errorf("no warning %q in\n%s", w, all)
		}
	}
	// a tar without pct.conf
	g := filepath.Join(dir, "vzdump-lxc-9.tar.gz")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "./etc/", Mode: 0o755, Typeflag: tar.TypeDir})
	tw.Close()
	zw.Close()
	os.WriteFile(g, buf.Bytes(), 0o644)
	if _, err := Inspect(g); err == nil || !strings.Contains(err.Error(), "pct.conf") {
		t.Fatalf("got %v", err)
	}
	if sizeGB("512M") != 1 || sizeGB("1T") != 1024 || sizeGB("10") != 10 || sizeGB("1.5G") != 2 {
		t.Fatal("sizes")
	}
}
