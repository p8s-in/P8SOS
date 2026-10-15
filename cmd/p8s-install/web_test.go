package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/installer"
)

func testWeb(t *testing.T) (*webInst, *httptest.Server) {
	m := &machine{disks: []installer.Disk{
		{Name: "sda", Path: "/dev/sda", Size: 500 << 30, Model: "Disk A", Contents: []string{}, Mounted: []string{}},
		{Name: "sdb", Path: "/dev/sdb", Size: 500 << 30, Model: "Disk B", Parts: 1, Contents: []string{"ntfs Windows"}, Mounted: []string{}},
		{Name: "sdc", Path: "/dev/sdc", Size: 8 << 30, Model: "Stick", Why: "the installer is running from this disk", Live: true},
	}, nics: []installer.NIC{{Name: "enp3s0", MAC: "52:54:00:12:34:56", Link: true}}, uefi: true}
	lockFile = filepath.Join(t.TempDir(), "installing")
	w := newWeb(m, "/mnt", "", filepath.Join(t.TempDir(), "install.log"))
	w.scan = func(context.Context) error { return nil }
	w.zfs = func(context.Context) error { return nil }
	w.reboot = func() {}
	w.reset = func(string) {}
	srv := httptest.NewServer(w.Handler())
	t.Cleanup(srv.Close)
	return w, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, pin string, body any, want int) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	if pin != "" {
		req.Header.Set("X-PIN", pin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %d %v (want %d)", method, path, resp.StatusCode, out, want)
	}
	return out
}

func answers() map[string]any {
	return map[string]any{"disks": []string{"sda", "sdb"}, "layout": "mirror", "hostname": "p8s-test", "timezone": "Etc/UTC",
		"keyboard": "us", "user": "admin", "password": "secret-pass", "network": map[string]any{"interface": "enp3s0", "mode": "dhcp"}}
}

func TestWebInstaller(t *testing.T) {
	w, srv := testWeb(t)
	page, _ := http.Get(srv.URL + "/")
	b, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(b), "P8S OS installer") {
		t.Fatal("page")
	}
	m := call(t, srv, "GET", "/api/machine", "", nil, 200) // httptest is 127.0.0.1: no PIN
	if len(m["disks"].([]any)) != 3 || m["layouts"].(map[string]any)["2"] == nil {
		t.Fatalf("%v", m)
	}
	bad := answers()
	bad["password"] = "short"
	call(t, srv, "POST", "/api/plan", "", bad, 400)
	bad = answers()
	bad["disks"] = []string{"sdc"}
	call(t, srv, "POST", "/api/plan", "", bad, 400)
	p := call(t, srv, "POST", "/api/plan", "", answers(), 200)
	if !strings.Contains(p["summary"].(string), "mirror") || len(p["has_data"].([]any)) != 1 {
		t.Fatalf("%v", p)
	}
	// no ERASE: nothing starts
	call(t, srv, "POST", "/api/install", "", answers(), 400)

	release := make(chan struct{})
	var got *installer.Plan
	w.install = func(ctx context.Context, pl *installer.Plan, progress func(int, string)) error {
		got = pl
		progress(42, "Copying the system")
		<-release
		return nil
	}
	a := answers()
	a["confirm"] = "ERASE"
	call(t, srv, "POST", "/api/install", "", a, 202)
	call(t, srv, "POST", "/api/install", "", a, 409) // only one at a time
	time.Sleep(50 * time.Millisecond)
	pr := call(t, srv, "GET", "/api/progress", "", nil, 200)
	if pr["running"] != true || pr["pct"].(float64) != 42 {
		t.Fatalf("%v", pr)
	}
	call(t, srv, "POST", "/api/reboot", "", nil, 409)
	close(release)
	time.Sleep(50 * time.Millisecond)
	pr = call(t, srv, "GET", "/api/progress", "", nil, 200)
	if pr["done"] != true || !got.A.Erase || got.A.Hostname != "p8s-test" || got.Target != "/mnt" {
		t.Fatalf("%v %+v", pr, got)
	}
	if busy.Load() {
		t.Fatal("still busy")
	}

	// a failed install shows the step, and can start again
	os.WriteFile(w.log, []byte("### [3/9] Making the pools\n### zpool create\ncannot create 'rpool': pool already exists\n### FAILED: exit status 1\n"), 0o600)
	w.install = func(context.Context, *installer.Plan, func(int, string)) error {
		return errors.New("step 3: exit status 1")
	}
	call(t, srv, "POST", "/api/install", "", a, 202)
	time.Sleep(50 * time.Millisecond)
	pr = call(t, srv, "GET", "/api/progress?log=1", "", nil, 200)
	if pr["error"] == "" || !strings.Contains(pr["failed_step"].(string), "pool already exists") || pr["log"] == nil {
		t.Fatalf("%v", pr)
	}
	call(t, srv, "POST", "/api/reset", "", nil, 200)
	if pr = call(t, srv, "GET", "/api/progress", "", nil, 200); pr["error"] != "" {
		t.Fatalf("%v", pr)
	}
}

func TestWebPIN(t *testing.T) {
	w, _ := testWeb(t)
	h := w.Handler()
	do := func(pin string) int {
		r := httptest.NewRequest("GET", "/api/machine", nil)
		r.RemoteAddr = "192.168.0.50:5555" // another computer
		if pin != "" {
			r.Header.Set("X-PIN", pin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if do("") != 401 || do("000000x") != 401 {
		t.Fatal("no PIN accepted")
	}
	if do(w.pin) != 200 {
		t.Fatal("right PIN refused")
	}
	for i := 0; i < 5; i++ {
		do("bad")
	}
	if do(w.pin) != 429 {
		t.Fatal("not locked after 5 wrong PINs")
	}
	r := httptest.NewRequest("GET", "/api/hello", nil)
	r.RemoteAddr = "192.168.0.50:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), `"pin":true`) {
		t.Fatal(rec.Body.String())
	}
}
