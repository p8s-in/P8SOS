package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/cluster"
	"p8sos/internal/ippool"
	"p8sos/internal/vm"
)

// poolVMs: the fake VMs take their public address from the server's pools.
type poolVMs struct {
	*ownedVMs
	pools *ippool.Store
}

func (p *poolVMs) Create(ctx context.Context, r vm.CreateRequest) (vm.VM, error) {
	if r.IPPool != "" {
		if _, err := p.pools.Allocate(r.IPPool, r.Name, ""); err != nil {
			return vm.VM{}, fmt.Errorf("%w: %v", vm.ErrInvalid, err)
		}
	}
	return p.ownedVMs.Create(ctx, r)
}

func (p *poolVMs) ChangePublicIPs(_ context.Context, name string, r vm.PublicIPRequest) (vm.PublicIPResult, error) {
	l, err := p.pools.AddExtra(name, r.IP)
	return vm.PublicIPResult{Lease: l}, err
}

func TestClusterPublicIPsOnWorkers(t *testing.T) {
	ctx := context.Background()
	mk := func() *poolVMs { return &poolVMs{ownedVMs: &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}} }
	mv, wv := mk(), mk()
	master := newClusterNode(t, 4096, mv, mv.ownedVMs)
	worker := newClusterNode(t, 16384, wv, wv.ownedVMs)
	bridges := map[*clusterNode][]string{master: {"br0"}, worker: {}}
	for _, n := range []*clusterNode{master, worker} {
		n.m.DataDir = filepath.Dir(n.m.Dir)
		n.m.BookFiles = []string{cluster.PoolsFile}
		st := &ippool.Store{Path: filepath.Join(n.m.DataDir, cluster.PoolsFile), Leader: cluster.PoolsLeader{M: n.m}}
		n.s.Pools = st
		n.vms.PoolBridge = func(name string) (string, bool) { p, err := st.Get(name); return p.Bridge, err == nil }
		capf, node := n.m.LocalCapacity, n
		n.m.LocalCapacity = func(ctx context.Context) cluster.Capacity {
			c := capf(ctx)
			c.Bridges, c.SharedPools = bridges[node], true
			return c
		}
		n.s.LocalPublicIPs = n.vms.Local.(*poolVMs)
	}
	mv.pools, wv.pools = master.s.Pools, worker.s.Pools
	master.req(t, "POST", "/api/v1/ippools", secret, `{"name":"dc1","bridge":"br0","cidr":"203.0.113.8/29","gateway":"203.0.113.9"}`, 201)

	b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
	code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
	worker.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	master.m.Poll(ctx) // the book: the worker gets the master's pools
	master.m.Poll(ctx)

	// the worker sees the master's pool, but can't change it
	if b := worker.req(t, "GET", "/api/v1/ippools", secret, "", 200); !strings.Contains(b, `"name":"dc1"`) {
		t.Fatalf("worker pools %s", b)
	}
	worker.req(t, "POST", "/api/v1/ippools", secret, `{"name":"dc2","bridge":"br0","cidr":"192.0.2.0/29","gateway":"192.0.2.1"}`, 400)

	// the worker has no br0: the VM goes to the master (although the worker has more room)
	b = master.req(t, "POST", "/api/v1/vms", secret, `{"name":"web1","memory_mib":1024,"ip_pool":"dc1"}`, 201)
	if !strings.Contains(b, `"node_id":"n1"`) {
		t.Fatalf("placed %s", b)
	}
	if cs := master.vms.Candidates(ctx, vm.CreateRequest{MemoryMiB: 1024, IPPool: "dc1"}); cs[1].Reason == "" || !strings.Contains(cs[1].Reason, "no bridge br0") {
		t.Fatalf("candidates %+v", cs)
	}

	// with br0 there it goes to the worker; the master hands out the address
	bridges[worker] = []string{"br0", "virbr0"}
	master.m.Poll(ctx)
	b = master.req(t, "POST", "/api/v1/vms", secret, `{"name":"web2","memory_mib":1024,"ip_pool":"dc1"}`, 201)
	if !strings.Contains(b, `"node_id":"n2"`) {
		t.Fatalf("placed %s", b)
	}
	ml, ok := master.s.Pools.Lookup("web2")
	if !ok || ml.IP != "203.0.113.11" {
		t.Fatalf("master lease %+v %v", ml, ok)
	}
	if wl, ok := worker.s.Pools.Lookup("web2"); !ok || wl.IP != ml.IP {
		t.Fatal("the worker doesn't see its address")
	}
	// one more address, asked on the master: done on the worker, from the master's pool
	b = master.req(t, "POST", "/api/v1/vms/web2/public-ips", secret, `{"action":"add"}`, 200)
	if l, _ := master.s.Pools.Lookup("web2"); len(l.Extra) != 1 || !strings.Contains(b, l.Extra[0]) {
		t.Fatalf("extra %+v %s", l, b)
	}
	// the worker leaves: it keeps web2's addresses (p8sd sets this hook)
	worker.m.BeforeLeave = func() {
		worker.s.Pools.Adopt(func(name string) bool { _, err := wv.Get(ctx, name); return err == nil })
	}
	master.req(t, "DELETE", "/api/v1/cluster/nodes/n2?force=1", secret, "", 204)
	if l, ok := worker.s.Pools.Lookup("web2"); !ok || l.IP != ml.IP || len(l.Extra) != 1 {
		t.Fatalf("after leaving %+v %v", l, ok)
	}
	if _, ok := worker.s.Pools.Lookup("web1"); ok {
		t.Fatal("the master's VM came along")
	}
	if _, ok := master.s.Pools.Lookup("web2"); !ok {
		t.Fatal("the master must keep it taken")
	}
}
