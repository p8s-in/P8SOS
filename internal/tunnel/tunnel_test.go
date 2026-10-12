package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeCF is a minimal Cloudflare API.
type fakeCF struct {
	mu      sync.Mutex
	tunnels map[string]string // id -> name
	dns     map[string]string // name -> content
	ingress []Ingress
	puts    int
}

const tid = "c1744f8b-faa1-48a4-9e5c-02ac921467fa"

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
	if r.Header.Get("Authorization") != "Bearer good" {
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "Authentication error"}}})
		return
	}
	p := r.URL.Path
	switch {
	case r.Method == "GET" && p == "/zones":
		if r.URL.Query().Get("name") == "example.com" {
			ok([]map[string]any{{"id": "zone1", "name": "example.com", "account": map[string]string{"id": "acct1"}}})
		} else {
			ok([]any{})
		}
	case r.Method == "GET" && p == "/accounts/acct1/cfd_tunnel":
		out := []map[string]string{}
		for id, n := range f.tunnels {
			if n == r.URL.Query().Get("name") {
				out = append(out, map[string]string{"id": id, "name": n})
			}
		}
		ok(out)
	case r.Method == "POST" && p == "/accounts/acct1/cfd_tunnel":
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["config_src"] != "cloudflare" {
			w.WriteHeader(400)
			return
		}
		f.tunnels[tid] = b["name"]
		ok(map[string]string{"id": tid})
	case r.Method == "GET" && p == "/accounts/acct1/cfd_tunnel/"+tid+"/token":
		ok("eyJ-tunnel-token")
	case r.Method == "PUT" && p == "/accounts/acct1/cfd_tunnel/"+tid+"/configurations":
		var b struct {
			Config struct{ Ingress []Ingress } `json:"config"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		f.ingress = b.Config.Ingress
		f.puts++
		ok(map[string]any{})
	case r.Method == "GET" && p == "/zones/zone1/dns_records":
		n := r.URL.Query().Get("name")
		out := []map[string]string{}
		if c, has := f.dns[n]; has {
			typ := "CNAME"
			if !strings.HasSuffix(c, ".cfargotunnel.com") {
				typ = "A"
			}
			out = append(out, map[string]string{"id": "rec-" + n, "type": typ, "name": n, "content": c})
		}
		ok(out)
	case r.Method == "POST" && p == "/zones/zone1/dns_records":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["proxied"] != true || b["type"] != "CNAME" {
			w.WriteHeader(400)
			return
		}
		f.dns[b["name"].(string)] = b["content"].(string)
		ok(map[string]any{})
	case r.Method == "DELETE" && strings.HasPrefix(p, "/zones/zone1/dns_records/rec-"):
		delete(f.dns, strings.TrimPrefix(p, "/zones/zone1/dns_records/rec-"))
		ok(map[string]any{})
	default:
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "no route " + r.Method + " " + p}}})
	}
}

func setup(t *testing.T) (*Manager, *fakeCF, map[string]string) {
	f := &fakeCF{tunnels: map[string]string{}, dns: map[string]string{"blog.example.com": "203.0.113.9"}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ips := map[string]string{"web1": "192.168.122.50"}
	m := &Manager{Dir: t.TempDir(), Dashboard: "https://127.0.0.1:8006",
		Client: func(tok string) *Cloudflare { return &Cloudflare{Token: tok, Base: srv.URL} },
		IPs:    func(context.Context) (map[string]string, error) { return ips, nil }}
	return m, f, ips
}

func TestSetupAndSites(t *testing.T) {
	ctx := context.Background()
	m, f, ips := setup(t)

	if _, err := m.Add(ctx, "web", "web1", 80, false, ""); !errors.Is(err, ErrNotSetUp) {
		t.Fatalf("add before setup: %v", err)
	}
	if _, _, err := m.Setup(ctx, "bad", "example.com", "p8s-node"); err == nil || !strings.Contains(err.Error(), "Authentication") {
		t.Fatalf("bad token: %v", err)
	}
	if _, _, err := m.Setup(ctx, "good", "other.org", "p8s-node"); err == nil {
		t.Fatal("unknown domain accepted")
	}
	st, tok, err := m.Setup(ctx, "good", "Example.com.", "p8s-node")
	if err != nil || tok != "eyJ-tunnel-token" || st.TunnelID != tid || st.Domain != "example.com" {
		t.Fatalf("setup: %+v %q %v", st, tok, err)
	}
	if fi, _ := os.Stat(filepath.Join(m.Dir, "tunnel.json")); fi.Mode().Perm() != 0o600 {
		t.Fatal("tunnel.json must be 0600 (holds the API token)")
	}
	// setting up again reuses the tunnel
	if st2, _, err := m.Setup(ctx, "good", "example.com", "p8s-node"); err != nil || st2.TunnelID != tid || len(f.tunnels) != 1 {
		t.Fatalf("second setup: %+v %v (%d tunnels)", st2, err, len(f.tunnels))
	}

	// dashboard
	if _, err := m.SetDashboard(ctx, "panel"); err != nil {
		t.Fatal(err)
	}
	if f.dns["panel.example.com"] != tid+".cfargotunnel.com" {
		t.Fatalf("dashboard DNS %v", f.dns)
	}
	if len(f.ingress) != 2 || f.ingress[0].Service != "https://127.0.0.1:8006" || f.ingress[0].OriginRequest["noTLSVerify"] != true || f.ingress[1].Service != "http_status:404" {
		t.Fatalf("dashboard ingress %+v", f.ingress)
	}

	// sites
	s, err := m.Add(ctx, "Shop", "web1", 80, false, "alice")
	if err != nil || s.Host != "shop.example.com" || s.IP != "192.168.122.50" {
		t.Fatalf("add: %+v %v", s, err)
	}
	if f.ingress[1].Hostname != "shop.example.com" || f.ingress[1].Service != "http://192.168.122.50:80" {
		t.Fatalf("site ingress %+v", f.ingress)
	}
	if _, err := m.Add(ctx, "shop", "web2", 80, false, "bob"); !errors.Is(err, ErrExists) {
		t.Fatal("duplicate host accepted")
	}
	if _, err := m.Add(ctx, "blog", "web1", 80, false, "alice"); !errors.Is(err, ErrExists) {
		t.Fatal("overwrote someone's DNS record")
	}
	if f.dns["blog.example.com"] != "203.0.113.9" {
		t.Fatal("foreign DNS record changed")
	}
	for _, bad := range []string{"a.b", "-x", "x_y", "", "www"} {
		if _, err := m.Add(ctx, bad, "web1", 80, false, "alice"); !errors.Is(err, ErrInvalid) {
			t.Errorf("label %q accepted", bad)
		}
	}
	if _, err := m.Add(ctx, "api2", "web1", 0, false, "alice"); !errors.Is(err, ErrInvalid) {
		t.Error("port 0 accepted")
	}
	if _, err := m.Add(ctx, "secure", "web1", 443, true, "alice"); err != nil {
		t.Fatal(err)
	}
	// VM without an address yet: kept, but no route until it has one
	if _, err := m.Add(ctx, "later", "web9", 8080, false, ""); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.ingress {
		if r.Hostname == "later.example.com" {
			t.Fatal("route for a VM without an IP")
		}
	}
	puts := f.puts
	if err := m.Sync(ctx); err != nil || f.puts != puts {
		t.Fatalf("unchanged sync pushed again (%v)", err)
	}
	ips["web9"] = "192.168.122.77"
	ips["web1"] = "192.168.122.51" // DHCP gave a new address
	if err := m.Sync(ctx); err != nil || f.puts != puts+1 {
		t.Fatalf("changed sync: %v", err)
	}
	got := map[string]string{}
	for _, r := range f.ingress {
		got[r.Hostname] = r.Service
	}
	if got["shop.example.com"] != "http://192.168.122.51:80" || got["later.example.com"] != "http://192.168.122.77:8080" || got["secure.example.com"] != "https://192.168.122.51:443" {
		t.Fatalf("routes after IP change %v", got)
	}
	if l, _ := m.Sites("", "alice"); len(l) != 2 {
		t.Fatalf("alice's sites %+v", l)
	}

	// remove
	if err := m.Remove(ctx, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, has := f.dns["shop.example.com"]; has {
		t.Fatal("DNS record left behind")
	}
	if err := m.Remove(ctx, "shop.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("removing twice")
	}
	if err := m.RemoveVM(ctx, "web1"); err != nil {
		t.Fatal(err)
	}
	if l, _ := m.Sites("", ""); len(l) != 1 || l[0].VM != "web9" {
		t.Fatalf("after RemoveVM %+v", l)
	}
	if _, err := m.SetDashboard(ctx, ""); err != nil || f.dns["panel.example.com"] != "" {
		t.Fatalf("dashboard off: %v %v", err, f.dns)
	}
}
