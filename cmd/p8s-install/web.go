package main

// The graphical installer: the same questions as the text menus, as a web
// page. The ISO's "Install P8S OS (graphical)" entry shows it full screen on
// the computer itself (cage + Firefox, served on 127.0.0.1:8099 without a
// PIN). The text installer serves it too, over HTTPS on port 8006, so a
// server without a screen can be installed from another computer's browser —
// with the 6-digit PIN shown on the server's screen.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"p8sos/internal/installer"
	"p8sos/internal/tlsutil"
)

//go:embed webui.html
var webPage []byte

// busy is set while an install runs (text menus or web page: only one at a
// time); lockFile does the same across processes (the graphical installer's
// page and the text installer that takes over when the screen can't start).
var (
	busy     atomic.Bool
	lockFile = "/run/p8s-installing"
)

func acquire() bool {
	if !busy.CompareAndSwap(false, true) {
		return false
	}
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		busy.Store(false)
		return false
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	return true
}

func release() {
	_ = os.Remove(lockFile)
	busy.Store(false)
}

type webInst struct {
	m                   *machine
	target, squash, log string
	pin                 string // needed by every client except 127.0.0.1

	// test hooks
	scan    func(ctx context.Context) error
	zfs     func(ctx context.Context) error
	install func(ctx context.Context, p *installer.Plan, progress func(int, string)) error
	reboot  func()
	reset   func(target string)

	mu       sync.Mutex
	pct      int
	title    string
	running  bool
	done     bool
	err      string
	failed   string
	fails    int
	lockTill time.Time
}

func newWeb(m *machine, target, squash, logPath string) *webInst {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return &webInst{m: m, target: target, squash: squash, log: logPath, pin: fmt.Sprintf("%06d", n.Int64()),
		scan: m.scan, zfs: zfsReady, install: install,
		reboot: func() { _ = exec.Command("systemctl", "reboot").Run() },
		reset: func(t string) {
			_ = exec.Command("umount", "-Rl", t).Run()
			_ = exec.Command("zpool", "export", "-a").Run()
		}}
}

func local(r *http.Request) bool {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// auth: local clients pass; others need the PIN (5 wrong tries lock it for a minute).
func (w *webInst) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if local(r) {
			next(rw, r)
			return
		}
		w.mu.Lock()
		locked := time.Now().Before(w.lockTill)
		w.mu.Unlock()
		if locked {
			jsonErr(rw, http.StatusTooManyRequests, "too many wrong PINs: wait a minute")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-PIN")), []byte(w.pin)) != 1 {
			w.mu.Lock()
			w.fails++
			if w.fails >= 5 {
				w.fails, w.lockTill = 0, time.Now().Add(time.Minute)
			}
			w.mu.Unlock()
			jsonErr(rw, http.StatusUnauthorized, "type the PIN shown on the server's screen")
			return
		}
		next(rw, r)
	}
}

func jsonOut(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

func jsonErr(rw http.ResponseWriter, code int, msg string) {
	jsonOut(rw, code, map[string]string{"error": msg})
}

type webDisk struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Model  string `json:"model"`
	Size   string `json:"size"`
	State  string `json:"state"`
	Usable bool   `json:"usable"`
	Empty  bool   `json:"empty"`
}

type webNIC struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Wireless bool   `json:"wireless"`
	Link     bool   `json:"link"`
}

type webLayout struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (w *webInst) machineInfo() map[string]any {
	var disks []webDisk
	for _, d := range w.m.disks {
		disks = append(disks, webDisk{Name: d.Name, Label: d.Label(), Model: strings.TrimSpace(d.Model), Size: installer.HumanBytes(d.Size),
			State: d.State(), Usable: d.Usable(), Empty: d.Empty()})
	}
	var nics []webNIC
	for _, n := range w.m.nics {
		nics = append(nics, webNIC{Name: n.Name, Label: n.Label(), Wireless: n.Wireless, Link: n.Link})
	}
	layouts := map[int][]webLayout{}
	for n := 1; n <= 12; n++ {
		for _, l := range installer.Fits(n) {
			layouts[n] = append(layouts[n], webLayout{ID: string(l), Title: l.Title()})
		}
	}
	return map[string]any{"disks": disks, "nics": nics, "uefi": w.m.uefi, "layouts": layouts,
		"hostname": installer.DefaultHostname(), "zones": commonZones, "keyboards": keyboards}
}

var commonZones = []string{"Etc/UTC", "Asia/Kolkata", "Asia/Dubai", "Asia/Singapore", "Asia/Tokyo", "Asia/Shanghai",
	"Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Madrid", "Europe/Rome", "Europe/Amsterdam",
	"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles", "America/Sao_Paulo",
	"Africa/Johannesburg", "Australia/Sydney", "Pacific/Auckland"}

var keyboards = []string{"us", "gb", "in", "de", "fr", "es", "it", "pt", "br", "ch", "se", "no", "dk", "fi", "nl", "be", "pl", "ru", "jp"}

func (w *webInst) plan(a installer.Answers) (*installer.Plan, error) {
	p, err := installer.NewPlan(a, w.m.disks, w.m.nics, w.m.uefi, zoneOK)
	if err != nil {
		return nil, err
	}
	w.m.configure(p, w.target, w.squash, w.log)
	return p, nil
}

// Handler serves the page and its API.
func (w *webInst) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		_, _ = rw.Write(webPage)
	})
	mux.HandleFunc("GET /api/hello", func(rw http.ResponseWriter, r *http.Request) {
		jsonOut(rw, http.StatusOK, map[string]any{"pin": !local(r)})
	})
	mux.HandleFunc("GET /api/machine", w.auth(func(rw http.ResponseWriter, r *http.Request) {
		jsonOut(rw, http.StatusOK, w.machineInfo())
	}))
	mux.HandleFunc("POST /api/rescan", w.auth(func(rw http.ResponseWriter, r *http.Request) {
		if !busy.Load() {
			_ = w.scan(r.Context())
		}
		jsonOut(rw, http.StatusOK, w.machineInfo())
	}))
	mux.HandleFunc("POST /api/plan", w.auth(func(rw http.ResponseWriter, r *http.Request) {
		var a installer.Answers
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&a); err != nil {
			jsonErr(rw, http.StatusBadRequest, "bad request")
			return
		}
		p, err := w.plan(a)
		if err != nil {
			jsonErr(rw, http.StatusBadRequest, err.Error())
			return
		}
		var data []string
		for _, d := range p.HasData() {
			data = append(data, d.Label()+" — "+d.State())
		}
		var names []string
		for _, d := range p.Disks {
			names = append(names, d.Name)
		}
		jsonOut(rw, http.StatusOK, map[string]any{"summary": p.Summary(), "has_data": data, "disks": names})
	}))
	mux.HandleFunc("POST /api/install", w.auth(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			installer.Answers
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&body); err != nil {
			jsonErr(rw, http.StatusBadRequest, "bad request")
			return
		}
		if body.Confirm != "ERASE" {
			jsonErr(rw, http.StatusBadRequest, "type ERASE to confirm")
			return
		}
		a := body.Answers
		a.Erase = true
		p, err := w.plan(a)
		if err != nil {
			jsonErr(rw, http.StatusBadRequest, err.Error())
			return
		}
		if err := w.zfs(r.Context()); err != nil {
			jsonErr(rw, http.StatusConflict, err.Error())
			return
		}
		if !acquire() {
			jsonErr(rw, http.StatusConflict, "an install is already running (on the screen or in another browser)")
			return
		}
		w.mu.Lock()
		w.pct, w.title, w.running, w.done, w.err, w.failed = 0, "Starting", true, false, "", ""
		w.mu.Unlock()
		go w.run(p)
		jsonOut(rw, http.StatusAccepted, map[string]any{"started": true})
	}))
	mux.HandleFunc("GET /api/progress", w.auth(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		out := map[string]any{"running": w.running, "pct": w.pct, "title": w.title, "done": w.done, "error": w.err, "failed_step": w.failed}
		w.mu.Unlock()
		if r.URL.Query().Get("log") == "1" {
			if b, err := os.ReadFile(w.log); err == nil {
				s := string(b)
				if len(s) > 60000 {
					s = s[len(s)-60000:]
				}
				out["log"] = s
			}
		}
		jsonOut(rw, http.StatusOK, out)
	}))
	mux.HandleFunc("POST /api/reset", w.auth(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		failed := !w.running && w.err != ""
		if failed {
			w.err, w.failed, w.pct, w.title = "", "", 0, ""
		}
		w.mu.Unlock()
		if failed {
			w.reset(w.target) // the half-finished install: start fresh next time
			_ = w.scan(r.Context())
		}
		jsonOut(rw, http.StatusOK, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/reboot", w.auth(func(rw http.ResponseWriter, r *http.Request) {
		if busy.Load() {
			jsonErr(rw, http.StatusConflict, "an install is running")
			return
		}
		jsonOut(rw, http.StatusOK, map[string]any{"ok": true})
		go func() { time.Sleep(time.Second); w.reboot() }()
	}))
	return mux
}

func (w *webInst) run(p *installer.Plan) {
	defer release()
	err := w.install(context.Background(), p, func(pct int, t string) {
		w.mu.Lock()
		w.pct, w.title = pct, t
		w.mu.Unlock()
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.running = false
	if err != nil {
		w.err = err.Error()
		if b, e := os.ReadFile(w.log); e == nil {
			w.failed = installer.FailedStep(string(b), 40)
		}
		return
	}
	w.pct, w.title, w.done = 100, "Installed", true
}

// serveWeb starts the page: plain HTTP on 127.0.0.1:8099 for the screen of
// this computer, and HTTPS on :8006 for other computers (with the PIN).
func (w *webInst) serveWeb(remote bool) {
	h := w.Handler()
	go func() {
		s := &http.Server{Addr: "127.0.0.1:8099", Handler: h, ReadHeaderTimeout: 10 * time.Second}
		if err := s.ListenAndServe(); err != nil {
			log.Printf("graphical installer (local): %v", err)
		}
	}()
	if !remote {
		return
	}
	go func() {
		dir := "/run/p8s-install-tls"
		cert, key, err := tlsutil.Ensure(dir)
		if err != nil {
			log.Printf("graphical installer (remote): %v", err)
			return
		}
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			log.Printf("graphical installer (remote): %v", err)
			return
		}
		s := &http.Server{Addr: ":8006", Handler: h, ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}
		if err := s.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("graphical installer (remote): %v", err)
		}
	}()
}

// remoteHint is the line the screen shows: where another computer can install from.
func (w *webInst) remoteHint() string {
	var ips []string
	l, _ := net.InterfaceAddrs()
	for _, a := range l {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			ips = append(ips, "https://"+n.IP.String()+":8006")
		}
	}
	if len(ips) == 0 {
		return ""
	}
	return "From another computer's browser: " + strings.Join(ips, " or ") + "  (PIN " + w.pin + ")"
}
