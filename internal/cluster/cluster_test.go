package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/testfake"
)

func testCert(t *testing.T) string {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}}, &k.PublicKey, k)
	p := filepath.Join(t.TempDir(), "c.pem")
	os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	return p
}

func TestMasterTunnel(t *testing.T) {
	ctx := context.Background()
	run := &testfake.Runner{Fail: map[string]string{"ip link show": "does not exist"}}
	m := &Manager{Dir: t.TempDir(), CertFile: testCert(t), Run: run}
	if _, _, err := m.JoinCode(ctx, "http://x:8006", ""); err == nil {
		t.Fatal("http master url accepted")
	}
	code, cmd, err := m.JoinCode(ctx, "https://192.168.0.106:8006", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cmd, "p8 join https://192.168.0.106:8006 ") || !strings.Contains(code, ".") {
		t.Fatalf("%q", cmd)
	}
	st := m.State()
	if st.Role != "master" || st.Self.WGIP != "10.250.0.1" || st.Endpoint != "192.168.0.106:51820" || st.WGPriv != "" {
		t.Fatalf("state %+v", st)
	}
	calls := strings.Join(run.Calls, "\n")
	for _, want := range []string{"ip link add p8s-wg type wireguard", "wg setconf p8s-wg ", "ip address replace 10.250.0.1/24 dev p8s-wg", "ip link set p8s-wg mtu 1280 up"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing %q in\n%s", want, calls)
		}
	}
	_, pub, _ := wgKeys()
	rep, err := m.Register(ctx, RegisterRequest{Code: code, Name: "lap2", WGPub: pub, Token: strings.Repeat("t", 30), CertSHA: strings.Repeat("a", 64)})
	if err != nil || rep.ID != "n2" || rep.WGIP != "10.250.0.2" || rep.Endpoint != "192.168.0.106:51820" {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := m.Register(ctx, RegisterRequest{Code: code, Name: "lap3", WGPub: pub, Token: strings.Repeat("t", 30), CertSHA: strings.Repeat("b", 64)}); err == nil {
		t.Fatal("join code used twice")
	}
	// a second server with the same name is refused (rename it first)
	codeN, _, _ := m.JoinCode(ctx, "https://192.168.0.106:8006", "")
	_, pubN, _ := wgKeys()
	if _, err := m.Register(ctx, RegisterRequest{Code: codeN, Name: "lap2", WGPub: pubN, Token: strings.Repeat("t", 30), CertSHA: strings.Repeat("d", 64)}); err == nil || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("same name accepted: %v", err)
	}
	conf, _ := os.ReadFile(filepath.Join(m.Dir, "wg.conf"))
	if !strings.Contains(string(conf), "ListenPort = 51820") || !strings.Contains(string(conf), "PublicKey = "+pub) ||
		!strings.Contains(string(conf), "AllowedIPs = 10.250.0.2/32") || strings.Contains(string(conf), "Endpoint") {
		t.Fatalf("master conf:\n%s", conf)
	}
	if fi, _ := os.Stat(filepath.Join(m.Dir, "wg.conf")); fi.Mode().Perm() != 0o600 {
		t.Fatal("wg.conf readable by others")
	}
	// a worker's config dials the master and keeps NAT open
	w := State{Role: "worker", WGPriv: "k", Endpoint: "203.0.113.5:51820", Nodes: []Node{{ID: "n1", WGPub: "p", WGIP: "10.250.0.1"}}}
	if c := w.wgConfig(); !strings.Contains(c, "Endpoint = 203.0.113.5:51820") || !strings.Contains(c, "PersistentKeepalive = 25") ||
		!strings.Contains(c, "ListenPort = 51820") || !strings.Contains(c, "AllowedIPs = 10.250.0.0/24") {
		t.Fatalf("worker conf:\n%s", c)
	}
	// with the book: direct to the workers it can dial; the rest through the master
	w.Self = Node{ID: "n2"}
	w.Mesh = []Node{{ID: "n3", WGPub: "p3", WGIP: "10.250.0.3", Endpoint: "192.168.0.7:51820"}, {ID: "n4", WGPub: "p4", WGIP: "10.250.0.4"}}
	c := w.wgConfig()
	if !strings.Contains(c, "AllowedIPs = 10.250.0.1/32, 10.250.0.4/32") || !strings.Contains(c, "AllowedIPs = 10.250.0.3/32\nEndpoint = 192.168.0.7:51820") ||
		strings.Contains(c, "PublicKey = p4") {
		t.Fatalf("mesh conf:\n%s", c)
	}
	// the code expires
	m.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	code2, _, _ := m.JoinCode(ctx, "https://192.168.0.106:8006", "vpn.example.com")
	m.Now = func() time.Time { return time.Now().Add(4 * time.Hour) }
	if _, err := m.Register(ctx, RegisterRequest{Code: code2, Name: "lap3", WGPub: pub, Token: strings.Repeat("t", 30), CertSHA: strings.Repeat("c", 64)}); err == nil {
		t.Fatal("expired code accepted")
	}
	if m.State().Endpoint != "vpn.example.com:51820" {
		t.Fatal("endpoint not updated")
	}
	if err := m.Leave(ctx); err == nil {
		t.Fatal("master left with workers")
	}
	if err := m.Remove(ctx, "n2"); err != nil || len(m.Workers()) != 0 {
		t.Fatal(err)
	}
}
