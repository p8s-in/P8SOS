package vpn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestVPN(t *testing.T) {
	run := &testfake.Runner{Fail: map[string]string{"ip link show": "does not exist"}}
	m := &Manager{Dir: t.TempDir(), Run: run}
	ctx := context.Background()
	if _, _, err := m.AddDevice(ctx, "phone", "admin"); err == nil {
		t.Fatal("device added while off")
	}
	if _, err := m.SetSettings(ctx, Settings{Enabled: true}); err == nil {
		t.Fatal("no endpoint accepted")
	}
	if _, err := m.SetSettings(ctx, Settings{Enabled: true, Endpoint: "myhome.duckdns.org", LAN: true, LANNet: "192.168.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	d, conf, err := m.AddDevice(ctx, "Ravi's phone", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if d.IP != "10.251.0.2" || !strings.Contains(conf, "Endpoint = myhome.duckdns.org:51830") || !strings.Contains(conf, "AllowedIPs = 10.251.0.0/24, 192.168.0.0/24") ||
		!strings.Contains(conf, "Address = 10.251.0.2/32") || !strings.Contains(conf, "PrivateKey = ") {
		t.Fatalf("config:\n%s", conf)
	}
	d2, _, _ := m.AddDevice(ctx, "laptop", "admin")
	if d2.IP != "10.251.0.3" {
		t.Fatal(d2.IP)
	}
	calls := strings.Join(run.Calls, "\n")
	for _, want := range []string{"ip link add p8s-vpn type wireguard", "wg setconf p8s-vpn", "ip address replace 10.251.0.1/24 dev p8s-vpn", "nft -f", "sysctl -qw net.ipv4.ip_forward=1"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in\n%s", want, calls)
		}
	}
	m.mu.Lock()
	sc, nf := m.st.serverConfig(), m.st.nftRules()
	m.mu.Unlock()
	if strings.Count(sc, "[Peer]") != 2 || !strings.Contains(sc, "AllowedIPs = 10.251.0.3/32") || !strings.Contains(nf, "ip saddr 10.251.0.0/24 ip daddr 192.168.0.0/24 masquerade") {
		t.Fatalf("server:\n%s\n%s", sc, nf)
	}
	if err := m.RemoveDevice(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	st, _ := m.Status(ctx)
	if len(st.Devices) != 1 || st.Devices[0].PSK != "" || st.Pub == "" {
		t.Fatalf("status %+v", st)
	}
	// the server key survives a restart
	m2 := &Manager{Dir: m.Dir}
	if st2, _ := m2.Status(ctx); st2.Pub != st.Pub || len(st2.Devices) != 1 {
		t.Fatal("reload")
	}
	run.Calls = nil
	m.SetSettings(ctx, Settings{Enabled: false, Endpoint: "x"})
	if !strings.Contains(strings.Join(run.Calls, "\n"), "ip link del p8s-vpn") {
		t.Fatal("not taken down")
	}
	if _, _, err := m.AddDevice(ctx, "<script>", "admin"); err == nil {
		t.Fatal("bad name")
	}
}

func TestUPnP(t *testing.T) {
	var mapped string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `<?xml version="1.0"?><root><device><friendlyName>Home Router</friendlyName><deviceList><device><deviceList><device>
			  <serviceList><service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>/ctl</controlURL></service></serviceList>
			  </device></deviceList></device></deviceList></device></root>`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("SOAPAction"), "AddPortMapping") {
			mapped = string(b)
		}
		fmt.Fprint(w, `<s:Envelope><s:Body><u:GetExternalIPAddressResponse><NewExternalIPAddress>100.72.1.9</NewExternalIPAddress></u:GetExternalIPAddressResponse></s:Body></s:Envelope>`)
	}))
	defer srv.Close()
	pc, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if strings.Contains(string(buf[:n]), "M-SEARCH") {
				pc.WriteTo([]byte("HTTP/1.1 200 OK\r\nLOCATION: "+srv.URL+"/desc.xml\r\nST: x\r\n\r\n"), from)
			}
		}
	}()
	old := SSDPAddr
	SSDPAddr = pc.LocalAddr().String()
	defer func() { SSDPAddr = old }()
	r, err := FindRouter(context.Background())
	if err != nil || !r.Found || r.Name != "Home Router" || r.ExternalIP != "100.72.1.9" || !r.CGNAT {
		t.Fatalf("%+v %v", r, err)
	}
	if err := r.ForwardUDP(context.Background(), 51830, "P8S VPN"); err != nil || !strings.Contains(mapped, "<NewExternalPort>51830</NewExternalPort>") || !strings.Contains(mapped, "<NewInternalClient>127.0.0.1</NewInternalClient>") {
		t.Fatalf("%v %s", err, mapped)
	}
}
