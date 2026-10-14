package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/vm"
	"p8sos/internal/witness"
)

// Two servers and a witness: the survivor takes over by itself, the old
// master steps down when it is back, and a cut-off server never takes over.
func TestWitnessFailover(t *testing.T) {
	ctx := context.Background()
	net := &cutNet{down: map[string]bool{}}
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	ws := &witness.Server{File: filepath.Join(t.TempDir(), "w.json"), Token: "witness-token-0123456789", Now: now}
	wsrv := httptest.NewTLSServer(ws.Handler())
	defer wsrv.Close()
	cluster.WitnessHTTP = net.client("")
	defer func() { cluster.WitnessHTTP = nil }()
	pin := witness.Fingerprint(wsrv.Certificate().Raw)

	var nodes []*clusterNode
	for i := 0; i < 2; i++ {
		o := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
		n := newClusterNode(t, 8192, o, o)
		n.m.HTTP, n.m.Now = net.client, now
		nodes = append(nodes, n)
	}
	master, w1 := nodes[0], nodes[1]
	b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
	code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
	w1.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)

	// bad settings are refused; the worker can't set it
	master.req(t, "POST", "/api/v1/cluster/witness", secret, `{"url":"http://x:8007","token":"witness-token-0123456789","pin":"`+pin+`"}`, 400)
	w1.req(t, "POST", "/api/v1/cluster/witness", secret, fmt.Sprintf(`{"url":%q,"token":"witness-token-0123456789","pin":%q}`, wsrv.URL, pin), 409)
	master.req(t, "POST", "/api/v1/cluster/witness", secret, fmt.Sprintf(`{"url":%q,"token":"wrong-token-0123456789","pin":%q}`, wsrv.URL, pin), 502)
	out := master.req(t, "POST", "/api/v1/cluster/witness", secret, fmt.Sprintf(`{"url":%q,"token":"witness-token-0123456789","pin":%q}`, wsrv.URL, pin), 200)
	if !strings.Contains(out, `"voters":3`) || !strings.Contains(out, `"online":true`) {
		t.Fatalf("set witness: %s", out)
	}
	master.m.Poll(ctx) // book (with the witness) reaches the worker; heartbeat to the witness
	if w1.m.Voters() != 3 || !master.m.HasQuorum() || !w1.m.ReachesMajority(ctx) {
		t.Fatalf("voters %d", w1.m.Voters())
	}
	if g := master.req(t, "GET", "/api/v1/cluster", secret, "", 200); !strings.Contains(g, `"witness":{`) || strings.Contains(g, "witness-token") {
		t.Fatalf("cluster view: %s", g)
	}

	// the worker loses only its link to the master: the witness still hears it, so no takeover
	net.mu.Lock()
	net.down[master.srv.URL] = true
	net.mu.Unlock()
	clock.Add(60)
	if r, _ := (&witness.Client{Conf: *master.m.State().Witness, HTTP: cluster.WitnessHTTP}).Beat(ctx, master.m.State().Term, "n1"); !r.OK {
		t.Fatalf("beat %+v", r) // (the master still reaches the witness)
	}
	w1.m.WatchOnce(ctx)
	if w1.m.Role() != "worker" {
		t.Fatal("took over while the witness hears the master")
	}

	// the master dies for real: after the timeout the witness gives its vote
	clock.Add(60)
	w1.m.WatchOnce(ctx)
	if w1.m.Role() != "master" || w1.m.State().Term != 1 {
		t.Fatalf("worker should be master of term 1: %s %+v", w1.m.Role(), w1.m.State())
	}
	w1.m.Poll(ctx) // tells the witness about term 1
	if w1.m.State().Witness == nil || !w1.m.HasQuorum() {
		t.Fatal("new master keeps the witness and has a majority (itself + witness)")
	}

	// the old master comes back but is cut off from the new one: the witness tells it it's stale
	net.mu.Lock()
	net.down[master.srv.URL], net.down[w1.srv.URL] = false, true
	net.mu.Unlock()
	master.m.Poll(ctx)
	if master.m.Role() != "worker" {
		t.Fatalf("old master should step down, is %s", master.m.Role())
	}
	net.mu.Lock()
	net.down[w1.srv.URL] = false
	net.mu.Unlock()
	w1.m.Poll(ctx)
	if p := master.m.Peer(); p.MasterID != "n2" || p.Term != 1 {
		t.Fatalf("old master follows n2: %+v", p)
	}

	// the master cut off from both loses its majority; the worker alone with no witness can't win
	net.mu.Lock()
	net.down[wsrv.URL], net.down[master.srv.URL] = true, true
	net.mu.Unlock()
	w1.m.Poll(ctx)
	clock.Add(40)
	if w1.m.HasQuorum() {
		t.Fatal("master reaching nobody should lose its majority")
	}
	clock.Add(60)
	master.m.WatchOnce(ctx) // (the old master, now a worker, can't reach n2 or the witness)
	net.mu.Lock()
	net.down[w1.srv.URL] = true
	net.mu.Unlock()
	master.m.WatchOnce(ctx)
	if master.m.Role() != "worker" {
		t.Fatal("a server alone must not take over")
	}

	// removing it
	net.mu.Lock()
	net.down = map[string]bool{}
	net.mu.Unlock()
	w1.req(t, "DELETE", "/api/v1/cluster/witness", secret, "", 204)
	if w1.m.Voters() != 2 {
		t.Fatal("witness not removed")
	}
}

func TestWitnessDownload(t *testing.T) {
	o := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
	n := newClusterNode(t, 8192, o, o)
	old := WitnessBinDir
	WitnessBinDir = t.TempDir()
	defer func() { WitnessBinDir = old }()
	n.req(t, "GET", "/api/v1/cluster/witness/download/arm64", secret, "", 404)
	n.req(t, "GET", "/api/v1/cluster/witness/download/..%2fx", secret, "", 404)
	os.WriteFile(filepath.Join(WitnessBinDir, "p8s-witness-arm64"), []byte("ELF"), 0o755)
	if b := n.req(t, "GET", "/api/v1/cluster/witness/download/arm64", secret, "", 200); b != "ELF" {
		t.Fatalf("got %q", b)
	}
}
