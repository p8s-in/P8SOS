// Package hostinfo reports host capacity for the dashboard.
package hostinfo

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"

	"p8sos/internal/execx"
)

type Info struct {
	Hostname    string  `json:"hostname"`
	CPUs        int     `json:"cpus"`
	Load1       float64 `json:"load1"`
	MemTotalMiB int     `json:"mem_total_mib"`
	MemFreeMiB  int     `json:"mem_free_mib"` // available, not just free
	PoolSizeGB  float64 `json:"pool_size_gb"`
	PoolFreeGB  float64 `json:"pool_free_gb"`
	UptimeSec   int     `json:"uptime_sec"`
}

// Collect never fails as a whole: missing pieces are left at zero.
func Collect(ctx context.Context, run execx.Runner, dataset string) Info {
	in := Info{CPUs: runtime.NumCPU()}
	in.Hostname, _ = os.Hostname()
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			in.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			u, _ := strconv.ParseFloat(f[0], 64)
			in.UptimeSec = int(u)
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			kb, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			switch k {
			case "MemTotal":
				in.MemTotalMiB = kb / 1024
			case "MemAvailable":
				in.MemFreeMiB = kb / 1024
			}
		}
		f.Close()
	}
	if run != nil && dataset != "" {
		pool, _, _ := strings.Cut(dataset, "/")
		if out, err := run.Run(ctx, "zfs", "list", "-Hp", "-o", "used,avail", pool); err == nil {
			if f := strings.Fields(string(out)); len(f) == 2 {
				used, _ := strconv.ParseFloat(f[0], 64)
				avail, _ := strconv.ParseFloat(f[1], 64)
				in.PoolSizeGB = round1((used + avail) / 1e9)
				in.PoolFreeGB = round1(avail / 1e9)
			}
		}
	}
	return in
}

func round1(x float64) float64 { return float64(int(x*10+0.5)) / 10 }
