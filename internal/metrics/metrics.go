// Package metrics samples VM and host usage every few seconds and keeps the
// last hour in memory for the dashboard's charts. Nothing is written to disk:
// after a p8sd restart the charts start empty again.
package metrics

import (
	"bufio"
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Point is one VM sample. Rates are averages since the previous sample.
type Point struct {
	T         int64   `json:"t"`             // unix seconds
	CPU       float64 `json:"cpu"`           // % of the VM's own vCPUs (0-100)
	MemMiB    int     `json:"mem_mib"`       // host RAM the VM uses
	DiskRead  float64 `json:"disk_read_bps"` // bytes per second
	DiskWrite float64 `json:"disk_write_bps"`
	NetRx     float64 `json:"net_rx_bps"`
	NetTx     float64 `json:"net_tx_bps"`
}

// HostPoint is one host sample.
type HostPoint struct {
	T           int64   `json:"t"`
	CPU         float64 `json:"cpu"` // % of all host CPUs
	MemUsedMiB  int     `json:"mem_used_mib"`
	MemTotalMiB int     `json:"mem_total_mib"`
}

// Raw is one domain's cumulative counters (from `virsh domstats --raw`).
type Raw struct {
	CPUTimeNs                          uint64
	VCPUs                              int
	RSSKiB                             uint64
	RdBytes, WrBytes, RxBytes, TxBytes uint64
}

// ParseDomStats reads `virsh domstats --raw` output into counters per domain.
func ParseDomStats(out string) map[string]Raw {
	res := map[string]Raw{}
	var name string
	var r Raw
	flush := func() {
		if name != "" {
			res[name] = r
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Domain: "); ok {
			flush()
			name, r = strings.Trim(rest, "'\""), Raw{}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || name == "" {
			continue
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			continue
		}
		switch {
		case k == "cpu.time":
			r.CPUTimeNs = n
		case k == "vcpu.current":
			r.VCPUs = int(n)
		case k == "balloon.rss":
			r.RSSKiB = n
		case strings.HasPrefix(k, "block.") && strings.HasSuffix(k, ".rd.bytes"):
			r.RdBytes += n
		case strings.HasPrefix(k, "block.") && strings.HasSuffix(k, ".wr.bytes"):
			r.WrBytes += n
		case strings.HasPrefix(k, "net.") && strings.HasSuffix(k, ".rx.bytes"):
			r.RxBytes += n
		case strings.HasPrefix(k, "net.") && strings.HasSuffix(k, ".tx.bytes"):
			r.TxBytes += n
		}
	}
	flush()
	return res
}

// Collector samples on a timer. Stats and Host* are injectable for tests.
type Collector struct {
	Stats    func(ctx context.Context) (map[string]Raw, error) // running VMs by name
	HostCPU  func() (busy, total uint64, ok bool)              // cumulative jiffies
	HostMem  func() (usedMiB, totalMiB int)
	Interval time.Duration // 0 = 10s
	Window   time.Duration // history kept; 0 = 1h
	Now      func() time.Time

	mu        sync.Mutex
	vms       map[string][]Point
	host      []HostPoint
	prev      map[string]Raw
	prevT     time.Time
	prevBusy  uint64
	prevTotal uint64
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) window() time.Duration {
	if c.Window == 0 {
		return time.Hour
	}
	return c.Window
}

// Run samples until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	iv := c.Interval
	if iv == 0 {
		iv = 10 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	c.Sample(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Sample(ctx)
		}
	}
}

func rate(cur, prev uint64, secs float64) float64 {
	if cur < prev || secs <= 0 { // counter reset (VM restarted)
		return 0
	}
	return math.Round(float64(cur-prev) / secs)
}

// Sample takes one measurement of every running VM and the host.
func (c *Collector) Sample(ctx context.Context) {
	now := c.now()
	var raws map[string]Raw
	if c.Stats != nil {
		raws, _ = c.Stats(ctx)
	}
	var busy, total uint64
	cpuOK := false
	if c.HostCPU != nil {
		busy, total, cpuOK = c.HostCPU()
	}
	used, memTotal := 0, 0
	if c.HostMem != nil {
		used, memTotal = c.HostMem()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vms == nil {
		c.vms = map[string][]Point{}
	}
	secs := now.Sub(c.prevT).Seconds()
	first := c.prevT.IsZero()
	for name, r := range raws {
		p, seen := c.prev[name]
		if first || !seen || r.CPUTimeNs < p.CPUTimeNs {
			continue // need two samples of the same run for rates
		}
		vcpus := max(r.VCPUs, 1)
		cpu := rate(r.CPUTimeNs, p.CPUTimeNs, secs) / 1e9 / float64(vcpus) * 100
		c.vms[name] = append(c.vms[name], Point{
			T: now.Unix(), CPU: round1(min(cpu, 100)), MemMiB: int(r.RSSKiB / 1024),
			DiskRead: rate(r.RdBytes, p.RdBytes, secs), DiskWrite: rate(r.WrBytes, p.WrBytes, secs),
			NetRx: rate(r.RxBytes, p.RxBytes, secs), NetTx: rate(r.TxBytes, p.TxBytes, secs),
		})
	}
	if cpuOK && !first && total > c.prevTotal && busy >= c.prevBusy {
		hc := float64(busy-c.prevBusy) / float64(total-c.prevTotal) * 100
		c.host = append(c.host, HostPoint{T: now.Unix(), CPU: round1(hc), MemUsedMiB: used, MemTotalMiB: memTotal})
	}
	c.prev, c.prevT, c.prevBusy, c.prevTotal = raws, now, busy, total

	// Forget what is older than the window (and VMs with nothing left).
	cut := now.Add(-c.window()).Unix()
	for name, pts := range c.vms {
		i := 0
		for i < len(pts) && pts[i].T < cut {
			i++
		}
		if i == len(pts) {
			delete(c.vms, name)
		} else if i > 0 {
			c.vms[name] = append([]Point(nil), pts[i:]...)
		}
	}
	i := 0
	for i < len(c.host) && c.host[i].T < cut {
		i++
	}
	if i > 0 {
		c.host = append([]HostPoint(nil), c.host[i:]...)
	}
}

// VM returns a VM's samples, oldest first.
func (c *Collector) VM(name string) []Point {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Point{}, c.vms[name]...)
}

// Host returns the host's samples, oldest first.
func (c *Collector) Host() []HostPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]HostPoint{}, c.host...)
}

func round1(x float64) float64 { return float64(int(x*10+0.5)) / 10 }

// ProcStat reads cumulative busy and total CPU time from /proc/stat.
func ProcStat() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	return parseCPULine(sc.Text())
}

func parseCPULine(line string) (busy, total uint64, ok bool) {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	for i, s := range f[1:] {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += n
		if i != 3 && i != 4 { // idle, iowait
			busy += n
		}
	}
	return busy, total, true
}

// ProcMem reads used (total - available) and total memory from /proc/meminfo.
func ProcMem() (usedMiB, totalMiB int) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var tot, avail int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		kb, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
		switch k {
		case "MemTotal":
			tot = kb
		case "MemAvailable":
			avail = kb
		}
	}
	return (tot - avail) / 1024, tot / 1024
}
