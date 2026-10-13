package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/cluster"
	"p8sos/internal/config"
	"p8sos/internal/users"
	"p8sos/internal/vm"
)

type vncVMs struct {
	*ownedVMs
	addr string
}

func (v *vncVMs) ConsoleAddr(ctx context.Context, n string) (string, error) {
	if _, err := v.Get(ctx, n); err != nil {
		return "", err
	}
	return v.addr, nil
}

// clusterNode is one in-process p8sd: API over TLS with its own fake VMs.
type clusterNode struct {
	srv  *httptest.Server
	s    *Server
	m    *cluster.Manager
	vms  *cluster.VMs
	mem  int
	used *ownedVMs
}

func newClusterNode(t *testing.T, mem int, local cluster.Local, owned *ownedVMs) *clusterNode {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte(secret))
	n := &clusterNode{mem: mem, used: owned}
	n.m = &cluster.Manager{Dir: filepath.Join(dir, "cluster")}
	n.vms = &cluster.VMs{Local: local, M: n.m}
	n.s = &Server{VMs: n.vms, Version: "test", Tokens: []config.Token{{Name: "host", SHA256: hex.EncodeToString(sum[:])}},
		Users: &users.Store{Path: filepath.Join(dir, "users.json")}, Sessions: &users.Sessions{}, Limiter: &users.Limiter{Max: 3},
		Cluster: n.m, ClusterVMs: n.vms, Dial: n.vms.Dial, RemoteMetrics: n.vms.RemoteMetrics}
	n.m.LocalCapacity = func(ctx context.Context) cluster.Capacity {
		c := cluster.Capacity{CPUs: 4, MemTotalMiB: n.mem, PoolFreeGB: 500, Networks: []string{"nat"}}
		l, _ := owned.List(ctx)
		for _, v := range l {
			c.MemUsedMiB += v.MemoryMiB
			c.VMs++
		}
		return c
	}
	n.srv = httptest.NewTLSServer(n.s.Handler())
	t.Cleanup(n.srv.Close)
	cert := filepath.Join(dir, "p8sd.crt")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: n.srv.Certificate().Raw}), 0o600)
	n.m.CertFile = cert
	n.m.TestURL = n.srv.URL
	return n
}

func (n *clusterNode) req(t *testing.T, method, path, tok, body string, want int) string {
	t.Helper()
	req, _ := http.NewRequest(method, n.srv.URL+path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %d %s (want %d)", method, path, resp.StatusCode, b, want)
	}
	return string(b)
}

func TestCluster(t *testing.T) {
	ctx := context.Background()
	// a "VNC server" on the worker: echoes what it gets
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	mOwned := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{"web1": {Name: "web1", State: "running", MemoryMiB: 2048}}}
	wOwned := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{"far1": {Name: "far1", State: "running", MemoryMiB: 1024, Owner: "alice"}}}
	master := newClusterNode(t, 4096, mOwned, mOwned)
	worker := newClusterNode(t, 16384, &vncVMs{wOwned, ln.Addr().String()}, wOwned)

	// not clustered yet: just the local VMs
	if b := master.req(t, "GET", "/api/v1/vms", secret, "", 200); strings.Contains(b, "far1") {
		t.Fatal("worker VM before joining")
	}
	b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
	var code string
	if i := strings.Index(b, `"code":"`); i >= 0 {
		code = strings.SplitN(b[i+8:], `"`, 2)[0]
	}
	if !strings.Contains(b, "p8 join "+master.srv.URL) || len(code) < 40 {
		t.Fatalf("join code %s", b)
	}
	// a wrong certificate pin is refused before anything is sent
	bad := code[:len(code)-4] + "0000"
	worker.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, bad), 502)
	worker.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	// the code works once
	worker2 := newClusterNode(t, 8192, &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}, &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}})
	worker2.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 502)

	if master.m.Role() != "master" || worker.m.Role() != "worker" || len(master.m.Workers()) != 1 {
		t.Fatalf("roles %q %q", master.m.Role(), worker.m.Role())
	}
	master.m.Poll(ctx)
	if st := master.m.NodeStatus("n2"); !st.Online || st.Capacity.MemTotalMiB != 16384 {
		t.Fatalf("status %+v", st)
	}
	cl := master.req(t, "GET", "/api/v1/cluster", secret, "", 200)
	if !strings.Contains(cl, `"role":"master"`) || !strings.Contains(cl, `"id":"n2"`) || strings.Contains(cl, `"token"`) {
		t.Fatalf("cluster view %s", cl)
	}

	// one list over both servers, tagged with the server
	l, _ := master.vms.List(ctx)
	if len(l) != 2 || l[0].Name != "far1" || l[0].NodeID != "n2" || l[1].NodeID != "n1" {
		t.Fatalf("list %+v", l)
	}
	// customers see their VMs wherever they run; the master checks ownership
	master.s.Users.Add("alice", users.Customer, "alice-password", users.Quota{VMs: 5, VCPUs: 8, MemoryMiB: 8192, DiskGB: 100})
	master.s.Users.Add("bob", users.Customer, "bob-password-1", users.Quota{VMs: 5, VCPUs: 8, MemoryMiB: 8192, DiskGB: 100})
	alice := loginOn(t, master, "alice", "alice-password")
	bob := loginOn(t, master, "bob", "bob-password-1")
	if b := master.req(t, "GET", "/api/v1/vms", alice, "", 200); !strings.Contains(b, "far1") || strings.Contains(b, "web1") {
		t.Fatalf("alice sees %s", b)
	}
	master.req(t, "POST", "/api/v1/vms/far1/stop", bob, `{"force":true}`, 404)
	master.req(t, "POST", "/api/v1/vms/far1/start", alice, "", 200)

	// placement: the master has 4 GB (2 GB used), the worker 16 GB → worker
	b = master.req(t, "POST", "/api/v1/vms", secret, `{"name":"new1","template":"debian-13","memory_mib":2048}`, 201)
	if !strings.Contains(b, `"node_id":"n2"`) {
		t.Fatalf("placed %s", b)
	}
	if _, err := wOwned.Get(ctx, "new1"); err != nil {
		t.Fatal("not created on the worker")
	}
	// asked for a server by name
	master.req(t, "POST", "/api/v1/vms", secret, `{"name":"here1","template":"debian-13","memory_mib":512,"node":"n1"}`, 201)
	if _, err := mOwned.Get(ctx, "here1"); err != nil {
		t.Fatal("not created on the master")
	}
	// names are unique across servers
	master.req(t, "POST", "/api/v1/vms", secret, `{"name":"far1","template":"debian-13","node":"n1"}`, 409)
	// an owner the worker doesn't know is fine: accounts live on the master
	master.req(t, "PUT", "/api/v1/vms/new1/owner", secret, `{"owner":"alice"}`, 200)
	if v, _ := wOwned.Get(ctx, "new1"); v.Owner != "alice" {
		t.Fatalf("owner on worker %+v", v)
	}

	// console of a worker VM goes through the tunnel to its VNC port
	addr, err := master.vms.ConsoleAddr(ctx, "far1")
	if err != nil || addr != "cluster:n2/far1" {
		t.Fatalf("addr %q %v", addr, err)
	}
	conn, err := master.vms.Dial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "RFB 003.008\n")
	got := make([]byte, 12)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "RFB 003.008\n" {
		t.Fatalf("vnc echo %q %v", got, err)
	}
	conn.Close()
	// only the master may open those streams
	worker.req(t, "GET", "/api/v1/cluster/vnc/far1", secret, "", 403)

	// delete on the worker through the master
	master.req(t, "DELETE", "/api/v1/vms/new1", secret, "", 204)
	if _, err := wOwned.Get(ctx, "new1"); err == nil {
		t.Fatal("not deleted on the worker")
	}
	// worker offline: its VMs still show, as unknown
	worker.srv.Close()
	l, _ = master.vms.List(ctx)
	found := false
	for _, v := range l {
		if v.Name == "far1" {
			found = v.State == "unknown"
		}
	}
	if !found {
		t.Fatalf("offline worker's VMs %+v", l)
	}
	master.m.Poll(ctx)
	if master.m.NodeStatus("n2").Online {
		t.Fatal("still online")
	}
	// removing a server that still has VMs needs force
	master.req(t, "DELETE", "/api/v1/cluster/nodes/n2", secret, "", 409)
	master.req(t, "DELETE", "/api/v1/cluster/nodes/n2?force=1", secret, "", 204)
}

func loginOn(t *testing.T, n *clusterNode, user, pw string) string {
	b := n.req(t, "POST", "/api/v1/login", "", fmt.Sprintf(`{"user":%q,"password":%q}`, user, pw), 200)
	i := strings.Index(b, `"token":"`)
	return strings.SplitN(b[i+9:], `"`, 2)[0]
}
