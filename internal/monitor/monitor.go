// Package monitor watches the server and tells people when something goes
// wrong, before it turns into lost data or downtime: ZFS pool health and
// fullness, disk health (SMART), disk space, memory, temperatures, crashed
// VMs, failed backups, and power (UPS through NUT, or a laptop's battery).
//
// Problems become alerts (warning or critical). New, worse and fixed alerts
// are sent by email, to a webhook (Slack, Discord, Teams, Mattermost, …) and
// to ntfy (phone push). On a power cut with the battery nearly empty — or
// when the CPU overheats, if enabled — the guests are shut down cleanly and
// then the server powers off, so nothing is lost.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/execx"
)

// Settings are kept in <data_dir>/alerts.json.
type Settings struct {
	Emails  []string `json:"emails"`  // who gets alert emails
	Webhook string   `json:"webhook"` // POST of a JSON message ({"text": ...} works with Slack/Mattermost/Discord "/slack")
	Ntfy    string   `json:"ntfy"`    // ntfy topic URL for phone push, e.g. https://ntfy.sh/p8s-secret-topic

	PoolWarnPct  int `json:"pool_warn_pct"`    // ZFS pool full (default 80)
	PoolCritPct  int `json:"pool_crit_pct"`    // (default 90)
	DiskWarnPct  int `json:"disk_warn_pct"`    // system disk full (default 85)
	MemWarnPct   int `json:"mem_warn_pct"`     // RAM really used (default 92)
	CPUTempWarnC int `json:"cpu_temp_warn_c"`  // (default 85)
	DiskTempWarn int `json:"disk_temp_warn_c"` // drives (default 55)
	// ThermalShutdownC: CPU this hot twice in a row → guests off, server off (0 = never).
	ThermalShutdownC int `json:"thermal_shutdown_c"`

	UPS string `json:"ups"` // NUT UPS name, e.g. "ups@localhost" ("" = not used)
	// PowerShutdown: on battery and nearly empty (UPS "LB", or a laptop below
	// BatteryShutdownPct) → shut the guests down cleanly, then power off.
	PowerShutdown      bool `json:"power_shutdown"`
	BatteryShutdownPct int  `json:"battery_shutdown_pct"` // default 10
}

// Defaults fills unset thresholds.
func (s Settings) Defaults() Settings {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&s.PoolWarnPct, 80)
	def(&s.PoolCritPct, 90)
	def(&s.DiskWarnPct, 85)
	def(&s.MemWarnPct, 92)
	def(&s.CPUTempWarnC, 85)
	def(&s.DiskTempWarn, 55)
	def(&s.BatteryShutdownPct, 10)
	if s.Emails == nil {
		s.Emails = []string{}
	}
	return s
}

// settle: pause before powering off so the last messages leave.
var settle = 3 * time.Second

const (
	Warning  = "warning"
	Critical = "critical"
)

// Alert is a problem that is going on now.
type Alert struct {
	Key      string `json:"key"`      // e.g. "pool:tank", "smart:/dev/sda"
	Severity string `json:"severity"` // warning | critical
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Since    string `json:"since"`
}

// Event is one line of the alert history.
type Event struct {
	Time     string `json:"time"`
	Key      string `json:"key"`
	Severity string `json:"severity,omitempty"`
	Title    string `json:"title"`
	What     string `json:"what"` // raised | worse | resolved | action | test
}

// Check is the latest reading of something watched (shown as the health list).
type Check struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warning | critical
	Value  string `json:"value"`
}

// Guest is a VM's state for the crash check.
type Guest struct {
	Name  string
	State string
}

// Monitor runs the checks. Fields with funcs are optional.
type Monitor struct {
	Dir      string // where alerts.json lives
	Run      execx.Runner
	Mail     func(to, subject, body string) // nil = no email
	HTTP     *http.Client
	Log      *slog.Logger
	Host     string
	Interval time.Duration // 0 = 1 minute
	Now      func() time.Time
	Root     string // prefix for /sys, /proc (tests); "" = /

	// what to watch
	Paths      []string                            // filesystems to watch for space (default "/")
	Guests     func(ctx context.Context) []Guest   // VMs
	FailedJobs func(since time.Time) []string      // failed backups/restores since (descriptions)
	MemUsedPct func() int                          // RAM really used, 0-100
	StopGuests func(ctx context.Context) error     // clean shutdown of every VM/container
	Poweroff   func() error                        // power the server off
	Storages   func(ctx context.Context) []Storage // network storages (NAS / SAN)
	// Ceph is the Ceph cluster's state; nil func or nil result = no Ceph here.
	Ceph func(ctx context.Context) *CephState

	mu       sync.Mutex
	set      *Settings
	active   map[string]*Alert
	history  []Event
	checks   map[string]Check
	smart    []finding // last SMART results (they run every 30 minutes)
	smartAt  time.Time
	hot      int  // ticks in a row over the thermal limit
	unsynced int  // ticks in a row with the clock not synced
	shutting bool // a power/thermal shutdown is under way
	lastJobs time.Time
}

type finding struct {
	key, name, status, value, title, detail string
	display                                 bool // only shown in the health list (another finding carries the alert)
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Monitor) path(p string) string {
	if m.Root == "" {
		return p
	}
	return filepath.Join(m.Root, p)
}

func (m *Monitor) file() string { return filepath.Join(m.Dir, "alerts.json") }

type stored struct {
	Settings Settings `json:"settings"`
	Active   []Alert  `json:"active"`
	History  []Event  `json:"history"`
}

func (m *Monitor) load() {
	if m.set != nil {
		return
	}
	var st stored
	if b, err := os.ReadFile(m.file()); err == nil {
		_ = json.Unmarshal(b, &st)
	} else {
		st.Settings.PowerShutdown = true // new servers: safe shutdown on power loss
	}
	s := st.Settings.Defaults()
	m.set, m.active, m.history, m.checks = &s, map[string]*Alert{}, st.History, map[string]Check{}
	for i := range st.Active {
		a := st.Active[i]
		m.active[a.Key] = &a
	}
}

func (m *Monitor) save() {
	st := stored{Settings: *m.set, History: m.history}
	for _, a := range m.active {
		st.Active = append(st.Active, *a)
	}
	sort.Slice(st.Active, func(i, j int) bool { return st.Active[i].Key < st.Active[j].Key })
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.MkdirAll(m.Dir, 0o750)
	if os.WriteFile(m.file()+".tmp", b, 0o600) == nil {
		_ = os.Rename(m.file()+".tmp", m.file())
	}
}

// Settings returns the current settings.
func (m *Monitor) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	return *m.set
}

// ErrInvalid: bad settings.
var ErrInvalid = errors.New("invalid alert settings")

// SetSettings validates and stores settings.
func (m *Monitor) SetSettings(s Settings) (Settings, error) {
	var clean []string
	for _, e := range s.Emails {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "@") || strings.ContainsAny(e, " \r\n,;") {
			return s, fmt.Errorf("%w: %q is not an email address", ErrInvalid, e)
		}
		clean = append(clean, e)
	}
	s.Emails = clean
	for _, u := range []string{s.Webhook, s.Ntfy} {
		if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return s, fmt.Errorf("%w: %q must start with https://", ErrInvalid, u)
		}
	}
	if s.PoolCritPct > 0 && s.PoolWarnPct > s.PoolCritPct {
		return s, fmt.Errorf("%w: the pool warning level must be below the critical level", ErrInvalid)
	}
	if s.ThermalShutdownC != 0 && (s.ThermalShutdownC < 70 || s.ThermalShutdownC > 110) {
		return s, fmt.Errorf("%w: thermal shutdown must be 70-110 °C (or 0 = off)", ErrInvalid)
	}
	if s.BatteryShutdownPct < 0 || s.BatteryShutdownPct > 50 {
		return s, fmt.Errorf("%w: battery shutdown level must be 1-50 %%", ErrInvalid)
	}
	s = s.Defaults()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	*m.set = s
	m.save()
	return s, nil
}

// Status is what the dashboard shows.
type Status struct {
	Active  []Alert  `json:"active"`
	Checks  []Check  `json:"checks"`
	History []Event  `json:"history"`
	Set     Settings `json:"settings"`
}

func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	st := Status{Active: []Alert{}, Checks: []Check{}, History: []Event{}, Set: *m.set}
	for _, a := range m.active {
		st.Active = append(st.Active, *a)
	}
	sort.Slice(st.Active, func(i, j int) bool {
		if st.Active[i].Severity != st.Active[j].Severity {
			return st.Active[i].Severity == Critical
		}
		return st.Active[i].Key < st.Active[j].Key
	})
	for _, c := range m.checks {
		st.Checks = append(st.Checks, c)
	}
	sort.Slice(st.Checks, func(i, j int) bool { return st.Checks[i].Key < st.Checks[j].Key })
	for i := len(m.history) - 1; i >= 0 && len(st.History) < 100; i-- {
		st.History = append(st.History, m.history[i])
	}
	return st
}

// Loop checks every Interval until ctx ends.
func (m *Monitor) Loop(ctx context.Context) {
	iv := m.Interval
	if iv == 0 {
		iv = time.Minute
	}
	time.Sleep(20 * time.Second) // let VMs and pools come up after boot
	for {
		m.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(iv):
		}
	}
}

// Tick runs all checks once and sends what changed.
func (m *Monitor) Tick(ctx context.Context) {
	m.mu.Lock()
	m.load()
	set := *m.set
	needSmart := m.smartAt.IsZero() || m.now().Sub(m.smartAt) >= 30*time.Minute
	m.mu.Unlock()

	var fs []finding
	fs = append(fs, m.checkPools(ctx, set)...)
	fs = append(fs, m.checkSpace(set)...)
	fs = append(fs, m.checkMemory(set)...)
	temp, tf := m.checkTemps(set)
	fs = append(fs, tf...)
	fs = append(fs, m.checkGuests(ctx)...)
	fs = append(fs, m.checkJobs()...)
	fs = append(fs, m.checkClock(ctx)...)
	fs = append(fs, m.checkStorages(ctx)...)
	fs = append(fs, m.checkCeph(ctx)...)
	power, pf := m.checkPower(ctx, set)
	fs = append(fs, pf...)
	if needSmart {
		sm := m.checkSmart(ctx, set)
		m.mu.Lock()
		m.smart, m.smartAt = sm, m.now()
		m.mu.Unlock()
	}
	m.mu.Lock()
	fs = append(fs, m.smart...)
	m.mu.Unlock()

	m.apply(fs)

	// last line of defence: power and heat
	if set.PowerShutdown && power.critical {
		m.emergency(ctx, "power", "power is failing ("+power.why+"): shutting the guests down and powering off")
	}
	if set.ThermalShutdownC > 0 && temp >= float64(set.ThermalShutdownC) {
		m.mu.Lock()
		m.hot++
		hot := m.hot
		m.mu.Unlock()
		if hot >= 2 {
			m.emergency(ctx, "thermal", fmt.Sprintf("the CPU is at %.0f °C (limit %d °C): shutting the guests down and powering off", temp, set.ThermalShutdownC))
		}
	} else {
		m.mu.Lock()
		m.hot = 0
		m.mu.Unlock()
	}
}

// apply turns findings into alerts and notifications.
func (m *Monitor) apply(fs []finding) {
	m.mu.Lock()
	now := m.now().UTC().Format(time.RFC3339)
	seen := map[string]bool{}
	var send []Event
	var details = map[string]string{}
	m.checks = map[string]Check{}
	for _, f := range fs {
		if f.name != "" {
			m.checks[f.key] = Check{Key: f.key, Name: f.name, Status: f.status, Value: f.value}
		}
		if f.display || (f.status != Warning && f.status != Critical) {
			continue
		}
		seen[f.key] = true
		a, ok := m.active[f.key]
		switch {
		case !ok:
			m.active[f.key] = &Alert{Key: f.key, Severity: f.status, Title: f.title, Detail: f.detail, Since: now}
			send = append(send, Event{Time: now, Key: f.key, Severity: f.status, Title: f.title, What: "raised"})
		case a.Severity == Warning && f.status == Critical:
			a.Severity, a.Title, a.Detail = Critical, f.title, f.detail
			send = append(send, Event{Time: now, Key: f.key, Severity: f.status, Title: f.title, What: "worse"})
		default:
			a.Title, a.Detail = f.title, f.detail
		}
		details[f.key] = f.detail
	}
	for k, a := range m.active {
		if !seen[k] {
			delete(m.active, k)
			send = append(send, Event{Time: now, Key: k, Severity: a.Severity, Title: a.Title, What: "resolved"})
		}
	}
	m.history = append(m.history, send...)
	if len(m.history) > 500 {
		m.history = m.history[len(m.history)-500:]
	}
	if len(send) > 0 {
		m.save()
	}
	set := *m.set
	m.mu.Unlock()
	for _, e := range send {
		m.notify(set, e, details[e.Key])
	}
}

// notify sends one event everywhere it is configured.
func (m *Monitor) notify(set Settings, e Event, detail string) {
	host := m.Host
	if host == "" {
		host, _ = os.Hostname()
	}
	label := strings.ToUpper(e.Severity)
	switch e.What {
	case "resolved":
		label = "RESOLVED"
	case "action":
		label = "ACTION"
	case "test":
		label = "TEST"
	}
	subject := fmt.Sprintf("[P8S %s] %s: %s", host, label, e.Title)
	body := subject + "\n\n" + detail + "\n\nTime: " + e.Time + "\nCheck: " + e.Key +
		"\n\nSee Server → Health & alerts in the P8S OS dashboard.\n"
	if m.Mail != nil {
		for _, to := range set.Emails {
			m.Mail(to, subject, body)
		}
	}
	c := m.HTTP
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	post := func(url string, req *http.Request) {
		resp, err := c.Do(req)
		if err != nil {
			m.logf("alert to "+url, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			m.logf("alert to "+url, fmt.Errorf("HTTP %d", resp.StatusCode))
		}
	}
	if set.Webhook != "" {
		b, _ := json.Marshal(map[string]string{
			"text": subject + "\n" + detail, "content": subject + "\n" + detail, // Slack/Mattermost/Teams and Discord
			"title": e.Title, "severity": e.Severity, "event": e.What, "check": e.Key, "host": host, "time": e.Time, "detail": detail,
		})
		if req, err := http.NewRequest("POST", set.Webhook, bytes.NewReader(b)); err == nil {
			req.Header.Set("Content-Type", "application/json")
			post(set.Webhook, req)
		}
	}
	if set.Ntfy != "" {
		if req, err := http.NewRequest("POST", set.Ntfy, strings.NewReader(detail)); err == nil {
			req.Header.Set("Title", subject)
			pr := "default"
			switch {
			case e.What == "resolved":
				pr = "low"
			case e.Severity == Critical:
				pr = "urgent"
			case e.Severity == Warning:
				pr = "high"
			}
			req.Header.Set("Priority", pr)
			req.Header.Set("Tags", map[bool]string{true: "white_check_mark", false: "warning"}[e.What == "resolved"])
			post(set.Ntfy, req)
		}
	}
}

// Test sends a test message to every configured channel.
func (m *Monitor) Test() error {
	set := m.Settings()
	if len(set.Emails) == 0 && set.Webhook == "" && set.Ntfy == "" {
		return fmt.Errorf("%w: add an email address, a webhook or an ntfy topic first", ErrInvalid)
	}
	e := Event{Time: m.now().UTC().Format(time.RFC3339), Key: "test", Title: "test message", What: "test"}
	m.notify(set, e, "Alerts from this P8S OS server reach you here.")
	m.mu.Lock()
	m.history = append(m.history, e)
	m.save()
	m.mu.Unlock()
	return nil
}

// Notice records and sends a one-off event from elsewhere (e.g. high
// availability: "web1 restarted on node2").
func (m *Monitor) Notice(key, severity, title, detail string) {
	e := Event{Time: m.now().UTC().Format(time.RFC3339), Key: key, Severity: severity, Title: title, What: "action"}
	m.mu.Lock()
	m.load()
	m.history = append(m.history, e)
	if len(m.history) > 500 {
		m.history = m.history[len(m.history)-500:]
	}
	m.save()
	set := *m.set
	m.mu.Unlock()
	m.notify(set, e, detail)
}

// emergency shuts everything down once (power failing / overheating).
func (m *Monitor) emergency(ctx context.Context, key, why string) {
	m.mu.Lock()
	if m.shutting {
		m.mu.Unlock()
		return
	}
	m.shutting = true
	e := Event{Time: m.now().UTC().Format(time.RFC3339), Key: key, Severity: Critical, Title: why, What: "action"}
	m.history = append(m.history, e)
	m.save()
	set := *m.set
	m.mu.Unlock()
	m.notify(set, e, "P8S OS is shutting down every VM and container cleanly, then powering this server off.")
	if m.Log != nil {
		m.Log.Error("emergency shutdown", "why", why)
	}
	if m.StopGuests != nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
		m.logf("stop guests", m.StopGuests(sctx))
		cancel()
	}
	if m.Poweroff != nil {
		time.Sleep(settle) // let the mail/webhook leave
		m.logf("power off", m.Poweroff())
	}
}

func (m *Monitor) logf(what string, err error) {
	if err != nil && m.Log != nil {
		m.Log.Warn(what, "err", err)
	}
}
