package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"p8sos/internal/metrics"
)

type promMetrics struct{}

func (promMetrics) VM(name string) []metrics.Point {
	return []metrics.Point{{T: 1, CPU: 12.5, MemMiB: 1024, GuestMiB: 512, DiskRead: 1000, NetTx: 2000}}
}
func (promMetrics) Host() []metrics.HostPoint { return []metrics.HostPoint{{T: 1, CPU: 33.3}} }

func TestPrometheus(t *testing.T) {
	m := newMultiUserCfg(t, nil)
	m.server.Metrics = promMetrics{}
	m.server.Host = func(context.Context) any {
		return map[string]any{"hostname": "p8s-node", "cpus": 8, "mem_total_mib": 16000, "pool_size_gb": 400.5}
	}
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	m.req("GET", "/api/v1/prometheus", alice, "", 403)
	req, _ := http.NewRequest("GET", m.srv.URL+"/api/v1/prometheus", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := m.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	out := string(b)
	for _, want := range []string{"# TYPE p8s_up gauge\np8s_up 1", `p8s_host_cpus{server="p8s-node"} 8`, `p8s_host_cpu_percent{server="p8s-node"} 33.3`,
		`p8s_host_memory_total_bytes{server="p8s-node"} 16777216000`, `p8s_vm_up{server="p8s-node",vm="web1"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Count(out, "# TYPE p8s_vm_up") != 1 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("format:\n%s", out)
	}
}
