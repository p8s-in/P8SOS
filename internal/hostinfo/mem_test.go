package hostinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMem(t *testing.T) {
	d := t.TempDir()
	w := func(p, s string) {
		p = filepath.Join(d, p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	// 16 GiB total, 6 GiB available, 1 GiB free; ARC 7 GiB with min 1 GiB
	w("proc/meminfo", "MemTotal: 16777216 kB\nMemFree: 1048576 kB\nMemAvailable: 6291456 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	w("proc/spl/kstat/zfs/arcstats", "name type data\nsize 4 7516192768\nc_min 4 1073741824\n")
	w("sys/fs/cgroup/machine.slice/memory.current", "2147483648\n")
	w("sys/fs/cgroup/system.slice/p8s-ct-web.service/memory.current", "536870912\n")
	old := Root
	Root = d
	defer func() { Root = old }()
	m := ReadMem()
	// used = 16-6 = 10 GiB, minus 6 GiB of ARC that can be given back = 4 GiB
	if m.TotalMiB != 16384 || m.UsedMiB != 4096 || m.ZFSCacheMiB != 7168 || m.GuestsMiB != 2560 || m.SystemMiB != 1536 || m.CacheMiB != 5120 {
		t.Fatalf("%+v", m)
	}
	// no ZFS: plain total - available
	os.Remove(filepath.Join(d, "proc/spl/kstat/zfs/arcstats"))
	if m := ReadMem(); m.UsedMiB != 10240 || m.ZFSCacheMiB != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestTopProcs(t *testing.T) {
	d := t.TempDir()
	w := func(p, s string) {
		p = filepath.Join(d, p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	w("proc/10/status", "Name: qemu\nVmRSS: 2097152 kB\n")
	w("proc/10/comm", "qemu-system-x86\n")
	w("proc/10/cmdline", "qemu-system-x86_64\x00-name\x00guest=web1,debug-threads=on\x00")
	w("proc/11/status", "VmRSS: 51200 kB\n")
	w("proc/11/comm", "bash\n")
	w("proc/11/cgroup", "0::/system.slice/p8s-ct-db.service/payload\n")
	w("proc/12/status", "VmRSS: 102400 kB\n")
	w("proc/12/comm", "p8sd\n")
	w("proc/13/status", "VmRSS: 51200 kB\n")
	w("proc/13/comm", "p8sd\n")
	w("proc/2/status", "Name: kthreadd\n")
	old := Root
	Root = d
	defer func() { Root = old }()
	got := TopProcs(10)
	if len(got) != 3 || got[0].Name != "VM web1" || got[0].RSSMiB != 2048 || got[1].Name != "p8sd" || got[1].Count != 2 || got[1].RSSMiB != 150 || got[2].Name != "container db" {
		t.Fatalf("%+v", got)
	}
}
