package hostinfo

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Mem is where the host's RAM goes.
//
// Linux counts the ZFS cache (ARC) as "used", unlike the normal page cache,
// although ZFS hands most of it back as soon as VMs need memory. Used therefore
// leaves out the part of the ARC that can be given back (above zfs_arc_min),
// so an idle server doesn't look full.
type Mem struct {
	TotalMiB    int `json:"total_mib"`
	UsedMiB     int `json:"used_mib"`      // really in use (VMs, containers, system)
	GuestsMiB   int `json:"guests_mib"`    // running VMs + containers
	SystemMiB   int `json:"system_mib"`    // everything else: kernel, p8sd, libvirt, desktop...
	ZFSCacheMiB int `json:"zfs_cache_mib"` // ARC size (given back under pressure)
	CacheMiB    int `json:"cache_mib"`     // normal file cache (given back under pressure)
	FreeMiB     int `json:"free_mib"`      // not used at all
	KSMSavedMiB int `json:"ksm_saved_mib"` // RAM saved by merging identical VM pages
	SwapUsedMiB int `json:"swap_used_mib"`
}

// Root is prefixed to /proc and /sys paths (tests point it at a fake tree).
var Root = "/"

func kv(path string, sep string) map[string]int64 {
	m := map[string]int64{}
	f, err := os.Open(filepath.Join(Root, path))
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var k, v string
		if sep == "" { // arcstats: "name type value"
			fs := strings.Fields(sc.Text())
			if len(fs) != 3 {
				continue
			}
			k, v = fs[0], fs[2]
		} else {
			a, b, ok := strings.Cut(sc.Text(), sep)
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(a), strings.TrimSuffix(strings.TrimSpace(b), " kB")
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			m[k] = n
		}
	}
	return m
}

func readInt(path string) int64 {
	b, err := os.ReadFile(filepath.Join(Root, path))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// ReadMem reads /proc/meminfo, ZFS arcstats, KSM and the guests' cgroups.
func ReadMem() Mem {
	mi := kv("proc/meminfo", ":") // kB
	var m Mem
	tot, avail, free := mi["MemTotal"], mi["MemAvailable"], mi["MemFree"]
	if tot == 0 {
		return m
	}
	m.TotalMiB = int(tot / 1024)
	m.FreeMiB = int(free / 1024)
	m.SwapUsedMiB = int((mi["SwapTotal"] - mi["SwapFree"]) / 1024)
	m.CacheMiB = int(max(avail-free, 0) / 1024)
	used := (tot - avail) * 1024 // bytes

	arc := kv("proc/spl/kstat/zfs/arcstats", "")
	if size := arc["size"]; size > 0 {
		m.ZFSCacheMiB = int(size >> 20)
		if give := size - arc["c_min"]; give > 0 && give < used {
			used -= give
		}
	}
	if pages := readInt("sys/kernel/mm/ksm/pages_sharing"); pages > 0 {
		m.KSMSavedMiB = int(pages * int64(os.Getpagesize()) >> 20)
	}

	// VMs live in machine.slice (libvirt), containers in p8s-ct-*.service
	g := readInt("sys/fs/cgroup/machine.slice/memory.current")
	cts, _ := filepath.Glob(filepath.Join(Root, "sys/fs/cgroup/system.slice/p8s-ct-*.service/memory.current"))
	for _, p := range cts {
		rel, _ := filepath.Rel(Root, p)
		g += readInt(rel)
	}
	m.UsedMiB = int(used >> 20)
	m.GuestsMiB = min(int(g>>20), m.UsedMiB)
	m.SystemMiB = m.UsedMiB - m.GuestsMiB
	return m
}

// Proc is one program's resident memory (same-named processes summed).
type Proc struct {
	Name   string `json:"name"`
	Count  int    `json:"count"`
	RSSMiB int    `json:"rss_mib"`
}

// TopProcs lists the n programs using the most RAM. A VM's qemu shows as
// "VM <name>", a container's processes as "container <name>".
func TopProcs(n int) []Proc {
	dirs, _ := filepath.Glob(filepath.Join(Root, "proc/[0-9]*"))
	sum := map[string]*Proc{}
	for _, d := range dirs {
		rel, _ := filepath.Rel(Root, d)
		st := kv(filepath.Join(rel, "status"), ":")
		rss := st["VmRSS"] // kB; kernel threads have none
		if rss == 0 {
			continue
		}
		name := ""
		if b, err := os.ReadFile(filepath.Join(d, "comm")); err == nil {
			name = strings.TrimSpace(string(b))
		}
		if cg, err := os.ReadFile(filepath.Join(d, "cgroup")); err == nil {
			c := string(cg)
			if i := strings.Index(c, "p8s-ct-"); i >= 0 {
				ct, _, _ := strings.Cut(c[i+7:], ".service")
				name = "container " + ct
			}
		}
		if strings.HasPrefix(name, "qemu") {
			if b, err := os.ReadFile(filepath.Join(d, "cmdline")); err == nil {
				args := strings.Split(string(b), "\x00")
				for i, a := range args {
					if a == "-name" && i+1 < len(args) {
						v, _, _ := strings.Cut(strings.TrimPrefix(args[i+1], "guest="), ",")
						name = "VM " + v
					}
				}
			}
		}
		if name == "" {
			continue
		}
		p := sum[name]
		if p == nil {
			p = &Proc{Name: name}
			sum[name] = p
		}
		p.Count++
		p.RSSMiB += int(rss / 1024)
	}
	out := make([]Proc, 0, len(sum))
	for _, p := range sum {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RSSMiB > out[j].RSSMiB })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// ARCLimits returns zfs_arc_max / zfs_arc_min in MiB (0 = ZFS default).
func ARCLimits() (maxMiB, minMiB int) {
	return int(readInt("sys/module/zfs/parameters/zfs_arc_max") >> 20), int(readInt("sys/module/zfs/parameters/zfs_arc_min") >> 20)
}
