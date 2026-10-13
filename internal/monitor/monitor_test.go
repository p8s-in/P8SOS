package monitor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/testfake"
)

func write(t *testing.T, root, p, s string) {
	t.Helper()
	p = filepath.Join(root, p)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(s), 0o644)
}

func TestMonitor(t *testing.T) {
	settle = 0
	root := t.TempDir()
	write(t, root, "/sys/class/hwmon/hwmon0/name", "coretemp\n")
	write(t, root, "/sys/class/hwmon/hwmon0/temp1_input", "61000\n")
	write(t, root, "/sys/class/hwmon/hwmon1/name", "nvme\n") // not the CPU
	write(t, root, "/sys/class/hwmon/hwmon1/temp1_input", "99000\n")
	write(t, root, "/sys/class/power_supply/BAT0/capacity", "80\n")
	write(t, root, "/sys/class/power_supply/BAT0/status", "Charging\n")

	run := &testfake.Runner{Out: map[string]string{
		"zpool list":       "tank\tONLINE\t42\n",
		"smartctl --scan":  `{"devices":[{"name":"/dev/sda","type":"sat"},{"name":"/dev/nvme0","type":"nvme"}]}`,
		"smartctl -d sat":  `{"model_name":"WDC RED","serial_number":"W1","smart_status":{"passed":true},"temperature":{"current":38},"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":0}}]}}`,
		"smartctl -d nvme": `{"model_name":"Samsung","serial_number":"S1","smart_status":{"passed":true},"temperature":{"current":45},"nvme_smart_health_information_log":{"critical_warning":0,"percentage_used":3,"media_errors":0}}`,
	}}
	var mu sync.Mutex
	var mails, hooks, pushes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if r.URL.Path == "/hook" {
			hooks = append(hooks, string(b))
		} else {
			pushes = append(pushes, r.Header.Get("Priority")+" "+r.Header.Get("Title"))
		}
		mu.Unlock()
	}))
	defer srv.Close()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	mem := 50
	guests := []Guest{{"web1", "running"}}
	var stopped, off bool
	m := &Monitor{Dir: t.TempDir(), Run: run, Root: root, Host: "node1", Now: func() time.Time { return now },
		Mail:       func(to, subj, body string) { mu.Lock(); mails = append(mails, to+": "+subj); mu.Unlock() },
		MemUsedPct: func() int { return mem },
		Guests:     func(context.Context) []Guest { return guests },
		StopGuests: func(context.Context) error { stopped = true; return nil },
		Poweroff:   func() error { off = true; return nil },
		Paths:      []string{"/"},
	}
	if _, err := m.SetSettings(Settings{Emails: []string{"ops@p8s.in"}, Webhook: srv.URL + "/hook", Ntfy: srv.URL + "/topic", PowerShutdown: true, DiskWarnPct: 99}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetSettings(Settings{Emails: []string{"not an email"}}); err == nil {
		t.Fatal("bad email accepted")
	}
	ctx := context.Background()

	// all fine
	m.Tick(ctx)
	st := m.Status()
	if len(st.Active) != 0 || len(mails) != 0 {
		t.Fatalf("expected no alerts: %+v", st.Active)
	}
	check := func(key string) Check {
		for _, c := range st.Checks {
			if c.Key == key {
				return c
			}
		}
		t.Fatalf("no check %s in %+v", key, st.Checks)
		return Check{}
	}
	if c := check("cputemp"); c.Value != "61 °C" { // the 99 °C NVMe sensor isn't the CPU
		t.Fatalf("cpu %+v", c)
	}
	if c := check("smart:/dev/sda"); c.Status != "ok" || !strings.Contains(c.Value, "38 °C") {
		t.Fatalf("smart %+v", c)
	}
	check("power")
	check("pool:tank")

	// pool degraded + 2 bad sectors (after the 30-minute SMART interval) + RAM full + a crashed VM
	run.Out["zpool list"] = "tank\tDEGRADED\t85\n"
	run.Out["smartctl -d sat"] = `{"model_name":"WDC RED","serial_number":"W1","smart_status":{"passed":true},"temperature":{"current":38},"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":2}}]}}`
	now = now.Add(31 * time.Minute)
	mem = 95
	guests = []Guest{{"web1", "crashed"}}
	m.Tick(ctx)
	st = m.Status()
	keys := map[string]string{}
	for _, a := range st.Active {
		keys[a.Key] = a.Severity
	}
	if keys["pool:tank"] != Critical || keys["poolfull:tank"] != Warning || keys["smart:/dev/sda"] != Warning || keys["memory"] != Warning || keys["vm:web1"] != Critical {
		t.Fatalf("alerts %+v", keys)
	}
	if st.Active[0].Severity != Critical {
		t.Fatal("critical first")
	}
	mu.Lock()
	if len(mails) != 5 || !strings.Contains(strings.Join(mails, "\n"), "[P8S node1] CRITICAL: ZFS pool tank is DEGRADED") || len(hooks) != 5 || len(pushes) != 5 {
		t.Fatalf("mails %v hooks %d pushes %v", mails, len(hooks), pushes)
	}
	mu.Unlock()

	// the same again: nothing new is sent
	m.Tick(ctx)
	if len(mails) != 5 {
		t.Fatalf("re-sent: %v", mails)
	}

	// fixed: resolved messages
	run.Out["zpool list"] = "tank\tONLINE\t40\n"
	mem = 40
	guests = []Guest{{"web1", "running"}}
	m.Tick(ctx)
	st = m.Status()
	if len(st.Active) != 1 || st.Active[0].Key != "smart:/dev/sda" { // SMART is re-read only every 30 minutes
		t.Fatalf("after fix %+v", st.Active)
	}
	if !strings.Contains(strings.Join(mails, "\n"), "RESOLVED: ZFS pool tank is DEGRADED") {
		t.Fatalf("no resolved mail: %v", mails)
	}

	// power cut: battery 30% → warning; 8% → clean shutdown, once
	write(t, root, "/sys/class/power_supply/BAT0/status", "Discharging\n")
	write(t, root, "/sys/class/power_supply/BAT0/capacity", "30\n")
	m.Tick(ctx)
	if stopped || off {
		t.Fatal("shut down too early")
	}
	write(t, root, "/sys/class/power_supply/BAT0/capacity", "8\n")
	m.Tick(ctx)
	if !stopped || !off {
		t.Fatal("no shutdown on an empty battery")
	}
	found := false
	for _, e := range m.Status().History {
		found = found || e.What == "action"
	}
	if !found {
		t.Fatal("shutdown not in history")
	}

	// settings and history survive a restart
	m2 := &Monitor{Dir: m.Dir}
	if s := m2.Settings(); len(s.Emails) != 1 || s.PoolWarnPct != 80 || !s.PowerShutdown {
		t.Fatalf("reloaded %+v", s)
	}
	if len(m2.Status().History) == 0 {
		t.Fatal("history lost")
	}
}

func TestUPS(t *testing.T) {
	run := &testfake.Runner{Out: map[string]string{"upsc ups@localhost": "battery.charge: 95\nups.status: OL\n"}}
	m := &Monitor{Dir: t.TempDir(), Run: run, Root: t.TempDir()}
	s := Settings{UPS: "ups@localhost"}.Defaults()
	if p, f := m.checkPower(context.Background(), s); p.critical || f[0].status != "ok" {
		t.Fatalf("%+v %+v", p, f)
	}
	run.Out["upsc ups@localhost"] = "battery.charge: 40\nbattery.runtime: 600\nups.status: OB DISCHRG\n"
	if p, f := m.checkPower(context.Background(), s); p.critical || f[0].status != Warning {
		t.Fatalf("%+v %+v", p, f)
	}
	run.Out["upsc ups@localhost"] = "battery.charge: 9\nups.status: OB DISCHRG LB\n"
	if p, f := m.checkPower(context.Background(), s); !p.critical || f[0].status != Critical {
		t.Fatalf("%+v %+v", p, f)
	}
}

func TestClock(t *testing.T) {
	settle = 0
	run := &testfake.Runner{Out: map[string]string{"timedatectl show": "no\n"}}
	m := &Monitor{Dir: t.TempDir(), Run: run, Root: t.TempDir()}
	for i := 0; i < 9; i++ {
		m.Tick(context.Background())
	}
	clock := func() bool {
		for _, a := range m.Status().Active {
			if a.Key == "clock" {
				return true
			}
		}
		return false
	}
	if clock() {
		t.Fatal("warned too early")
	}
	m.Tick(context.Background())
	if !clock() {
		t.Fatalf("%+v", m.Status().Active)
	}
	run.Out["timedatectl show"] = "yes\n"
	m.Tick(context.Background())
	if clock() {
		t.Fatal("not resolved")
	}
}
