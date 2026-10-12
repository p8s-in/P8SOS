// Package updates keeps a P8S OS server up to date from the P8S update
// server: check the newest release on the server's channel (stable with a
// subscription, testing without), download it, verify P8S's signature and
// run update.sh in its own systemd unit (p8sd restarts during the update).
package updates

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"p8sos/internal/execx"
	"p8sos/internal/license"
	"p8sos/internal/release"
)

// Channel tells the manager where this server gets updates from.
type Channel struct {
	Allowed []string // channels this server may use (testing; stable with a subscription)
	License string   // licence id ("" = none)
	Server  string   // server id
}

type Manager struct {
	Dir     string // data_dir/updates
	Server  string // update server base URL, e.g. https://licence.example.com ("" = off)
	Current string // running version
	Info    func() Channel
	Keys    []ed25519.PublicKey // nil = license.Trusted
	Run     execx.Runner
	HTTP    *http.Client
	Now     func() time.Time

	mu      sync.Mutex
	loaded  bool
	st      state
	working bool
}

type state struct {
	Channel   string            `json:"channel,omitempty"` // chosen channel ("" = best allowed)
	Checked   string            `json:"checked,omitempty"`
	Available *release.Manifest `json:"available,omitempty"`
	Signature string            `json:"signature,omitempty"`
	Download  string            `json:"download,omitempty"`
	Error     string            `json:"error,omitempty"`
	Unit      string            `json:"unit,omitempty"` // systemd unit of the last install
	Target    string            `json:"target,omitempty"`
	From      string            `json:"from,omitempty"`
	Started   string            `json:"started,omitempty"`
}

// Status is what the dashboard shows.
type Status struct {
	Enabled   bool              `json:"enabled"`
	Current   string            `json:"current"`
	Channel   string            `json:"channel"`
	Channels  []string          `json:"channels"`
	Available *release.Manifest `json:"available,omitempty"`
	Update    bool              `json:"update"` // the available release differs from this one
	Checked   string            `json:"checked,omitempty"`
	Error     string            `json:"error,omitempty"`
	Install   *Install          `json:"install,omitempty"`
}

// Install is the last update run.
type Install struct {
	Version string `json:"version"`
	From    string `json:"from"`
	Started string `json:"started"`
	State   string `json:"state"` // running, done, failed
	Log     string `json:"log,omitempty"`
}

var (
	ErrOff     = errors.New("no update server is set (license_server in /etc/p8s/p8sd.json)")
	ErrBusy    = errors.New("an update is already running")
	ErrNone    = errors.New("no update available")
	ErrChannel = errors.New("channel not available")
)

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) keys() []ed25519.PublicKey {
	if m.Keys != nil {
		return m.Keys
	}
	return license.Trusted
}

func (m *Manager) load() {
	if m.loaded {
		return
	}
	m.loaded = true
	if b, err := os.ReadFile(filepath.Join(m.Dir, "state.json")); err == nil {
		_ = json.Unmarshal(b, &m.st)
	}
}

func (m *Manager) save() error {
	b, _ := json.MarshalIndent(m.st, "", "  ")
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(m.Dir, "state.json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// channel picks the channel to use: the chosen one if still allowed, else
// the best allowed (stable before testing).
func (m *Manager) channel(c Channel) string {
	for _, a := range c.Allowed {
		if a == m.st.Channel {
			return a
		}
	}
	for _, a := range c.Allowed {
		if a == "stable" {
			return a
		}
	}
	return "testing"
}

// SetChannel chooses stable or testing.
func (m *Manager) SetChannel(ch string) error {
	c := m.Info()
	ok := false
	for _, a := range c.Allowed {
		ok = ok || a == ch
	}
	if !ok {
		return fmt.Errorf("%w: %q (this server can use %s)", ErrChannel, ch, strings.Join(c.Allowed, ", "))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	if m.st.Channel != ch {
		m.st.Channel, m.st.Available, m.st.Signature, m.st.Download, m.st.Checked = ch, nil, "", "", ""
	}
	return m.save()
}

// Status reports versions and the last install.
func (m *Manager) Status(ctx context.Context) Status {
	c := m.Info()
	m.mu.Lock()
	m.load()
	st := Status{Enabled: m.Server != "", Current: m.Current, Channel: m.channel(c), Channels: c.Allowed,
		Available: m.st.Available, Checked: m.st.Checked, Error: m.st.Error}
	st.Update = st.Available != nil && st.Available.Version != m.Current
	unit, target, from, started := m.st.Unit, m.st.Target, m.st.From, m.st.Started
	m.mu.Unlock()
	if unit != "" {
		in := &Install{Version: target, From: from, Started: started}
		active, _ := m.Run.Run(ctx, "systemctl", "is-active", unit)
		switch s := strings.TrimSpace(string(active)); {
		case s == "active" || s == "activating":
			in.State = "running"
		case m.Current == target:
			in.State = "done"
		default:
			in.State = "failed"
		}
		if log, err := m.Run.Run(ctx, "journalctl", "-u", unit, "-o", "cat", "-n", "200", "--no-pager"); err == nil {
			in.Log = string(log)
		}
		st.Install = in
	}
	return st
}

type answer struct {
	Release   *release.Manifest `json:"release"`
	Signature string            `json:"signature"`
	Download  string            `json:"download"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (m *Manager) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// Check asks the update server for the newest release on the channel.
func (m *Manager) Check(ctx context.Context) error {
	if m.Server == "" {
		return ErrOff
	}
	c := m.Info()
	m.mu.Lock()
	m.load()
	ch := m.channel(c)
	m.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"channel": ch, "license": c.License, "server": c.Server, "current": m.Current})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.Server, "/")+"/api/v1/updates", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var a answer
	err = func() error {
		resp, err := m.client().Do(req)
		if err != nil {
			return fmt.Errorf("update server: %w", err)
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a)
		if resp.StatusCode != http.StatusOK {
			msg := http.StatusText(resp.StatusCode)
			if a.Error != nil && a.Error.Message != "" {
				msg = a.Error.Message
			}
			return fmt.Errorf("update server: %s", msg)
		}
		if a.Release != nil && !release.Verify(m.keys(), *a.Release, a.Signature) {
			return errors.New("update server: the release is not signed by P8S; not offered")
		}
		return nil
	}()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Checked = m.now().UTC().Format(time.RFC3339)
	if err != nil {
		m.st.Error = err.Error()
		_ = m.save()
		return err
	}
	m.st.Error, m.st.Available, m.st.Signature, m.st.Download = "", a.Release, a.Signature, a.Download
	return m.save()
}

// Install downloads, verifies and starts installing the available release.
// It returns once update.sh is running (in its own unit, so p8sd can restart).
func (m *Manager) Install(ctx context.Context) (string, error) {
	if m.Server == "" {
		return "", ErrOff
	}
	m.mu.Lock()
	if m.working {
		m.mu.Unlock()
		return "", ErrBusy
	}
	m.working = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.working = false; m.mu.Unlock() }()

	if st := m.Status(ctx); st.Install != nil && st.Install.State == "running" {
		return "", ErrBusy
	}
	if err := m.Check(ctx); err != nil {
		return "", err
	}
	m.mu.Lock()
	rel, sig, dl := m.st.Available, m.st.Signature, m.st.Download
	m.mu.Unlock()
	if rel == nil || rel.Version == m.Current {
		return "", ErrNone
	}
	if !release.Verify(m.keys(), *rel, sig) {
		return "", errors.New("the release is not signed by P8S")
	}
	if rel.Size <= 0 || rel.Size > release.MaxSize {
		return "", errors.New("the release is too large")
	}
	u, err := url.Parse(dl)
	if err != nil || u.IsAbs() || !strings.HasPrefix(u.Path, "/api/v1/updates/download") {
		return "", errors.New("update server gave a bad download address")
	}

	// fresh folders for this version; older downloads go
	bundle := filepath.Join(m.Dir, "p8sos-"+rel.Version+".tar.gz")
	src := filepath.Join(m.Dir, "src-"+rel.Version)
	for _, pat := range []string{"src-*", "p8sos-*.tar.gz"} {
		old, _ := filepath.Glob(filepath.Join(m.Dir, pat))
		for _, o := range old {
			_ = os.RemoveAll(o)
		}
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(m.Server, "/")+dl, nil)
	if err != nil {
		return "", err
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(bundle, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, release.MaxSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	sum, size, err := release.FileSHA256(bundle)
	if err != nil {
		return "", err
	}
	if sum != rel.SHA256 || size != rel.Size {
		_ = os.Remove(bundle)
		return "", errors.New("download is damaged (checksum mismatch); try again")
	}
	if err := release.Extract(bundle, src); err != nil {
		_ = os.RemoveAll(src)
		return "", fmt.Errorf("unpack: %w", err)
	}
	unit := fmt.Sprintf("p8s-update-%d", m.now().Unix())
	if _, err := m.Run.Run(ctx, "systemd-run", "--unit="+unit, "--collect", "--quiet",
		"--setenv=HOME=/root", "--setenv=DEBIAN_FRONTEND=noninteractive",
		"--property=TimeoutStartSec=3600", "bash", filepath.Join(src, "update.sh")); err != nil {
		return "", fmt.Errorf("start update: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Unit, m.st.Target, m.st.From, m.st.Started = unit, rel.Version, m.Current, m.now().UTC().Format(time.RFC3339)
	return unit, m.save()
}

// Loop checks once a day (after start-up) so the dashboard can say "update available".
func (m *Manager) Loop(ctx context.Context, onErr func(error)) {
	if m.Server == "" {
		return
	}
	for {
		if err := m.Check(ctx); err != nil && onErr != nil && !errors.Is(err, context.Canceled) {
			onErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}
