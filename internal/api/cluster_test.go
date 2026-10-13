package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
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
	// every server has its own certificate (pinning tells them apart)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "p8s"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	n.srv = httptest.NewUnstartedServer(n.s.Handler())
	if os.Getenv("DEV") != "" { // browser tests: reachable on every address (discovery finds the LAN one)
		n.srv.Listener.Close()
		n.srv.Listener, _ = net.Listen("tcp4", "0.0.0.0:0")
	}
	n.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	n.srv.StartTLS()
	t.Cleanup(n.srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(n.srv.URL, "https://"))
	n.m.APIPort = port
	cert := filepath.Join(dir, "p8sd.crt")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
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

func TestClusterDiscoverInvite(t *testing.T) {
	ctx := context.Background()
	empty := func() *ownedVMs { return &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}} }
	mo, wo := empty(), empty()
	master := newClusterNode(t, 8192, mo, mo)
	worker := newClusterNode(t, 8192, wo, wo)
	// the worker answers discovery on a test port
	pc, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	worker.m.DiscoverPort, master.m.DiscoverPort = port, port
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go worker.m.Respond(rctx, "test")
	time.Sleep(100 * time.Millisecond)

	found, err := master.m.Discover(ctx, "test", "127.0.0.1")
	if err != nil || len(found) != 1 || !found[0].Available || found[0].IP != "127.0.0.1" {
		t.Fatalf("found %+v %v", found, err)
	}
	// the master sends an invitation; nothing happens until the worker's admin accepts
	b := master.req(t, "POST", "/api/v1/cluster/invite-send", secret, mustJSON(found[0]), 200)
	confirm := strings.SplitN(strings.SplitN(b, `"confirm":"`, 2)[1], `"`, 2)[0]
	if worker.m.Role() != "" {
		t.Fatal("joined without being accepted")
	}
	inv := worker.m.Invites()
	if len(inv) != 1 || inv[0].Confirm != confirm || inv[0].MasterName == "" {
		t.Fatalf("invites %+v (want number %s)", inv, confirm)
	}
	if strings.Contains(worker.req(t, "GET", "/api/v1/cluster/invites", secret, "", 200), `"code"`) {
		t.Fatal("join code leaked in the invite list")
	}
	worker.req(t, "POST", "/api/v1/cluster/invites/"+inv[0].ID+"/accept", secret, "", 200)
	if worker.m.Role() != "worker" || len(master.m.Workers()) != 1 {
		t.Fatalf("after accept: %q %d", worker.m.Role(), len(master.m.Workers()))
	}
	// found again: now a member, not available
	found, _ = master.m.Discover(ctx, "test", "127.0.0.1")
	if len(found) != 1 || found[0].Available || found[0].Why != "already in this cluster" {
		t.Fatalf("after joining %+v", found)
	}
	// a server in a cluster refuses invitations
	worker.req(t, "POST", "/api/v1/cluster/invite", "", `{"master_name":"evil","master_url":"https://10.0.0.9:8006","code":"`+strings.Repeat("x", 50)+`"}`, 409)
	// declined invitations are gone
	lone := newClusterNode(t, 4096, empty(), empty())
	time.Sleep(2100 * time.Millisecond) // invitation rate limit is per server; this is a new one anyway
	lone.req(t, "POST", "/api/v1/cluster/invite", "", `{"master_name":"lap1","master_url":"https://10.0.0.9:8006","code":"`+strings.Repeat("x", 50)+`"}`, 202)
	id := lone.m.Invites()[0].ID
	lone.req(t, "POST", "/api/v1/cluster/invites/"+id+"/decline", secret, "", 204)
	if len(lone.m.Invites()) != 0 {
		t.Fatal("declined invite still there")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
