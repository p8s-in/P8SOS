package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/vm"
)

// cutNet lets a test pull a server's "cable": calls to its URL fail.
type cutNet struct {
	mu   sync.Mutex
	down map[string]bool
}

func (c *cutNet) client(string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		c.mu.Lock()
		d := c.down["https://"+r.URL.Host]
		c.mu.Unlock()
		if d {
			return nil, errors.New("connection timed out (cable pulled)")
		}
		return (&http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}).RoundTrip(r)
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMasterFailover(t *testing.T) {
	ctx := context.Background()
	net := &cutNet{down: map[string]bool{}}
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	var nodes []*clusterNode
	roles := map[string][]string{}
	var rmu sync.Mutex
	for i := 0; i < 3; i++ {
		o := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
		n := newClusterNode(t, 8192, o, o)
		n.m.HTTP, n.m.Now = net.client, now
		dir := filepath.Dir(n.m.Dir)
		n.m.DataDir, n.m.BookFiles = dir, []string{"users.json", "cluster/ha.json"}
		id := fmt.Sprint(i)
		n.m.OnRoleChange = func(role string) { rmu.Lock(); roles[id] = append(roles[id], role); rmu.Unlock() }
		nodes = append(nodes, n)
	}
	master, w1, w2 := nodes[0], nodes[1], nodes[2]
	os.WriteFile(filepath.Join(master.m.DataDir, "users.json"), []byte(`[{"name":"alice"}]`), 0o600)
	for _, w := range []*clusterNode{w1, w2} {
		b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
		code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
		w.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	}
	master.m.Poll(ctx) // hands out the book
	if p := w1.m.Peer(); p.MasterID != "n1" || p.MasterSeen != 0 || w1.m.ClusterSize() != 3 {
		t.Fatalf("worker after book: %+v size %d", p, w1.m.ClusterSize())
	}
	if !master.m.HasQuorum() || !w1.m.ReachesMajority(ctx) {
		t.Fatal("everyone should be in the majority")
	}
	// the master answers; nobody takes over
	clock.Add(10)
	w1.m.WatchOnce(ctx)
	if w1.m.Role() != "worker" {
		t.Fatal("took over while the master was fine")
	}
	// a vote while the master is heard is refused
	if r := w2.m.Vote(cluster.VoteRequest{Term: 5, Candidate: "n2"}); r.Granted {
		t.Fatalf("vote granted while hearing the master: %+v", r)
	}

	// the master dies
	net.mu.Lock()
	net.down[master.srv.URL] = true
	net.mu.Unlock()
	clock.Add(60)
	w2.m.WatchOnce(ctx) // n3 waits for the lower-numbered n2
	if w2.m.Role() != "worker" {
		t.Fatal("n3 must leave it to n2")
	}
	w1.m.WatchOnce(ctx)
	if w1.m.Role() != "master" || w1.m.State().Term != 1 {
		t.Fatalf("n2 should be master of term 1: %s %+v", w1.m.Role(), w1.m.State())
	}
	if p := w2.m.Peer(); p.MasterID != "n2" || p.Term != 1 {
		t.Fatalf("n3 follows n2: %+v", p)
	}
	if b, _ := os.ReadFile(filepath.Join(w1.m.DataDir, "users.json")); !strings.Contains(string(b), "alice") {
		t.Fatalf("accounts didn't come along: %q", b)
	}
	rmu.Lock()
	if strings.Join(roles["1"], ",") != "master" {
		t.Fatalf("role hooks %v", roles)
	}
	rmu.Unlock()
	// n2 is now the master of n3 (and of the old master, by its key)
	if ws := w1.m.Workers(); len(ws) != 2 {
		t.Fatalf("new master's workers %+v", ws)
	}

	// the old master comes back: it can't push its stale book, and follows n2
	net.mu.Lock()
	net.down[master.srv.URL] = false
	net.mu.Unlock()
	master.m.Poll(ctx)
	if master.m.Role() != "worker" {
		t.Fatalf("old master should step down, is %s", master.m.Role())
	}
	w1.m.Poll(ctx) // the new master's book reaches it
	if p := master.m.Peer(); p.MasterID != "n2" || p.Term != 1 {
		t.Fatalf("old master follows n2: %+v", p)
	}
	if ws := w1.m.Workers(); len(ws) != 2 {
		t.Fatalf("workers after return %+v", ws)
	}
	// a server alone can't win: n3 cut off from everyone
	net.mu.Lock()
	net.down[w1.srv.URL], net.down[master.srv.URL] = true, true
	net.mu.Unlock()
	clock.Add(120)
	w2.m.WatchOnce(ctx)
	if w2.m.Role() != "worker" {
		t.Fatal("a lone server must not become master")
	}
	if w1.m.HasQuorum() {
		// n2 still thinks it has a majority until it fails to reach the others for QuorumTimeout
		w1.m.Poll(ctx)
		clock.Add(40)
		if w1.m.HasQuorum() {
			t.Fatal("master cut off from the others should lose its majority")
		}
	}
	// promote by hand (two-server clusters)
	if err := w2.m.Promote(ctx, false); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if w2.m.Role() != "master" {
		t.Fatal("promote didn't take")
	}
}
