package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"p8sos/internal/metrics"
)

// GET /api/v1/prometheus: the server's and its VMs' numbers in Prometheus's
// text format, for Prometheus / Grafana / VictoriaMetrics. Scrape it with an
// API token (Settings → API tokens) as the bearer token:
//
//	scrape_configs:
//	  - job_name: p8s
//	    scheme: https
//	    tls_config: {insecure_skip_verify: true}
//	    authorization: {credentials: "p8s_…"}
//	    metrics_path: /api/v1/prometheus
//	    static_configs: [{targets: ["192.168.0.106:8006"]}]
//
// Staff and auditors only.
func (s *Server) prometheus(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var b strings.Builder
	seen := map[string]bool{}
	metric := func(name, help, kind string) {
		if !seen[name] {
			seen[name] = true
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		}
	}
	val := func(name string, labels map[string]string, v float64) {
		fmt.Fprintf(&b, "%s%s %s\n", name, promLabels(labels), promNum(v))
	}

	metric("p8s_up", "1 when p8sd answers", "gauge")
	val("p8s_up", nil, 1)

	host := ""
	if s.Host != nil {
		var h struct {
			Hostname    string  `json:"hostname"`
			CPUs        int     `json:"cpus"`
			Load1       float64 `json:"load1"`
			MemTotalMiB int     `json:"mem_total_mib"`
			MemFreeMiB  int     `json:"mem_free_mib"`
			PoolSizeGB  float64 `json:"pool_size_gb"`
			PoolFreeGB  float64 `json:"pool_free_gb"`
			UptimeSec   int     `json:"uptime_sec"`
		}
		if raw, err := json.Marshal(s.Host(r.Context())); err == nil && json.Unmarshal(raw, &h) == nil {
			host = h.Hostname
			l := map[string]string{"server": host}
			for _, m := range []struct {
				n, help string
				v       float64
			}{
				{"p8s_host_cpus", "CPU threads of the server", float64(h.CPUs)},
				{"p8s_host_load1", "1-minute load average", h.Load1},
				{"p8s_host_memory_total_bytes", "RAM of the server", float64(h.MemTotalMiB)*(1<<20)},
				{"p8s_host_memory_free_bytes", "RAM not really used (caches count as free)", float64(h.MemFreeMiB)*(1<<20)},
				{"p8s_host_pool_size_bytes", "size of the VM storage pool", h.PoolSizeGB * 1e9},
				{"p8s_host_pool_free_bytes", "free space in the VM storage pool", h.PoolFreeGB * 1e9},
				{"p8s_host_uptime_seconds", "time since the server started", float64(h.UptimeSec)},
			} {
				metric(m.n, m.help, "gauge")
				val(m.n, l, m.v)
			}
		}
	}
	if s.Metrics != nil {
		if pts := s.Metrics.Host(); len(pts) > 0 {
			p := pts[len(pts)-1]
			metric("p8s_host_cpu_percent", "CPU use of the server, % of all threads", "gauge")
			val("p8s_host_cpu_percent", map[string]string{"server": host}, p.CPU)
		}
	}

	vms, err := s.VMs.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	for _, v := range vms {
		node := v.Node
		if node == "" {
			node = host
		}
		l := map[string]string{"vm": v.Name, "server": node}
		if v.Owner != "" {
			l["owner"] = v.Owner
		}
		up := 0.0
		if v.State == "running" {
			up = 1
		}
		metric("p8s_vm_up", "1 while the VM runs", "gauge")
		val("p8s_vm_up", l, up)
		metric("p8s_vm_vcpus", "vCPUs of the VM", "gauge")
		val("p8s_vm_vcpus", l, float64(v.VCPUs))
		metric("p8s_vm_memory_bytes", "RAM the VM is given", "gauge")
		val("p8s_vm_memory_bytes", l, float64(v.MemoryMiB)*(1<<20))
		metric("p8s_vm_disk_size_bytes", "size of the VM's system disk", "gauge")
		val("p8s_vm_disk_size_bytes", l, float64(v.DiskGB)*(1<<30))
		if up == 0 {
			continue
		}
		p, ok := s.lastPoint(v.Name, v.NodeID != "" && v.NodeID != "n1")
		if !ok {
			continue
		}
		for _, m := range []struct {
			n, help string
			v       float64
		}{
			{"p8s_vm_cpu_percent", "CPU use, % of the VM's own vCPUs", p.CPU},
			{"p8s_vm_memory_host_bytes", "server RAM the VM process uses", float64(p.MemMiB)*(1<<20)},
			{"p8s_vm_memory_guest_bytes", "RAM in use inside the guest (caches not counted)", float64(p.GuestMiB)*(1<<20)},
			{"p8s_vm_disk_read_bytes_per_second", "disk reads", p.DiskRead},
			{"p8s_vm_disk_write_bytes_per_second", "disk writes", p.DiskWrite},
			{"p8s_vm_network_receive_bytes_per_second", "network into the VM", p.NetRx},
			{"p8s_vm_network_transmit_bytes_per_second", "network out of the VM", p.NetTx},
		} {
			metric(m.n, m.help, "gauge")
			val(m.n, l, m.v)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// lastPoint is a VM's newest sample, from this server or (remote) its own.
func (s *Server) lastPoint(name string, remote bool) (metrics.Point, bool) {
	if remote && s.RemoteMetrics != nil {
		if out, ok := s.RemoteMetrics(name); ok {
			var m struct {
				Points []metrics.Point `json:"points"`
			}
			if raw, err := json.Marshal(out); err == nil && json.Unmarshal(raw, &m) == nil && len(m.Points) > 0 {
				return m.Points[len(m.Points)-1], true
			}
			return metrics.Point{}, false
		}
	}
	if s.Metrics == nil {
		return metrics.Point{}, false
	}
	pts := s.Metrics.VM(name)
	if len(pts) == 0 {
		return metrics.Point{}, false
	}
	return pts[len(pts)-1], true
}

func promLabels(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(l[k])
		parts[i] = fmt.Sprintf(`%s="%s"`, k, v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func promNum(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
