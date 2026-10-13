package license

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"p8sos/internal/netx"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Grace is how long paid features keep working after a key expires.
const Grace = 14 * 24 * time.Hour

// Manager holds this server's licence (data_dir/license.json).
type Manager struct {
	Path     string              // license.json
	ServerID string              // this server's ID ("" = from /etc/machine-id)
	Keys     []ed25519.PublicKey // trusted signing keys (nil = Trusted)
	CheckURL string              // licence server check URL; "" = never check online
	ShopURL  string              // where to buy and renew (shown in the dashboard)
	HTTP     *http.Client
	Now      func() time.Time

	mu     sync.Mutex
	loaded bool
	st     stored
	lic    *License
}

type stored struct {
	Key       string `json:"key,omitempty"`
	Installed string `json:"installed,omitempty"`
	Checked   string `json:"checked,omitempty"` // last successful online check
	Revoked   bool   `json:"revoked,omitempty"`
	Message   string `json:"message,omitempty"` // from the licence server
	ServerID  string `json:"server_id,omitempty"`
}

// Status is what the dashboard shows.
type Status struct {
	ServerID  string     `json:"server_id"`
	Tier      string     `json:"tier"` // effective tier
	TierName  string     `json:"tier_name"`
	State     string     `json:"state"` // none, active, expiring, grace, expired, revoked
	Message   string     `json:"message"`
	License   *License   `json:"license,omitempty"`
	DaysLeft  int        `json:"days_left"` // until expiry (negative = past)
	Features  []string   `json:"features"`
	Notice    bool       `json:"notice"`  // show the "no subscription" notice
	Channel   string     `json:"channel"` // stable or testing
	UserLimit int        `json:"user_limit"`
	Checked   string     `json:"checked,omitempty"`
	Shop      string     `json:"shop,omitempty"`
	Online    bool       `json:"online"` // checks with the licence server
	Tiers     []TierInfo `json:"tiers"`
}

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
	return Trusted
}

func (m *Manager) load() {
	if m.loaded {
		return
	}
	m.loaded = true
	if b, err := os.ReadFile(m.Path); err == nil {
		_ = json.Unmarshal(b, &m.st)
	}
	if m.ServerID == "" {
		m.ServerID = m.st.ServerID
	}
	if m.ServerID == "" {
		m.ServerID = MachineServerID(filepath.Dir(m.Path))
	}
	if m.st.ServerID != m.ServerID {
		m.st.ServerID = m.ServerID
		_ = m.save()
	}
	if m.st.Key != "" {
		if l, err := Parse(m.keys(), m.st.Key); err == nil && l.Covers(m.ServerID) {
			m.lic = &l
		}
	}
}

func (m *Manager) save() error {
	if m.Path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(m.st, "", "  ")
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o750); err != nil {
		return err
	}
	tmp := m.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.Path)
}

// MachineServerID derives a stable server ID from /etc/machine-id (or, when
// that is missing, a random one kept in dir/server-id).
func MachineServerID(dir string) string {
	seed := ""
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
			seed = strings.TrimSpace(string(b))
			break
		}
	}
	if seed == "" {
		f := filepath.Join(dir, "server-id")
		if b, err := os.ReadFile(f); err == nil && len(b) >= 16 {
			seed = strings.TrimSpace(string(b))
		} else {
			r := make([]byte, 16)
			_, _ = rand.Read(r)
			seed = hex.EncodeToString(r)
			_ = os.MkdirAll(dir, 0o750)
			_ = os.WriteFile(f, []byte(seed+"\n"), 0o600)
		}
	}
	return ServerIDFrom(seed)
}

// ServerIDFrom turns a machine id into P8S-XXXX-XXXX-XXXX-XXXX.
func ServerIDFrom(seed string) string {
	sum := sha256.Sum256([]byte("p8s-server:" + seed))
	h := strings.ToUpper(hex.EncodeToString(sum[:8]))
	return "P8S-" + h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16]
}

// ID returns this server's ID.
func (m *Manager) ID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	return m.ServerID
}

// Install checks and saves a key.
func (m *Manager) Install(key string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	if len(m.keys()) == 0 {
		return Status{}, fmt.Errorf("%w: this is a development build without a P8S signing key; keys can't be checked yet", ErrKey)
	}
	l, err := Parse(m.keys(), key)
	if err != nil {
		return Status{}, err
	}
	if !l.Covers(m.ServerID) {
		return Status{}, fmt.Errorf("%w (this server is %s; the key covers %s)", ErrServer, m.ServerID, strings.Join(l.Servers, ", "))
	}
	if m.now().After(l.End().Add(Grace)) {
		return Status{}, fmt.Errorf("%w: it expired on %s", ErrKey, l.Expires)
	}
	m.st = stored{Key: strings.Join(strings.Fields(key), ""), Installed: m.now().UTC().Format(time.RFC3339), ServerID: m.ServerID}
	m.lic = &l
	if err := m.save(); err != nil {
		return Status{}, err
	}
	return m.status(), nil
}

// Remove deletes the key (the server goes back to Home).
func (m *Manager) Remove() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	m.st = stored{ServerID: m.ServerID}
	m.lic = nil
	return m.save()
}

// Status reports the licence state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	return m.status()
}

func (m *Manager) status() Status {
	if len(m.keys()) == 0 { // no P8S signing key built in yet: licensing is off
		t, _ := Tier(Internal)
		return Status{ServerID: m.ServerID, Tier: Internal, TierName: "Development", State: "development", Tiers: Tiers(),
			Features: append([]string{}, t.Features...), Channel: "testing",
			Message: "Development build: licensing is not switched on yet (no P8S signing key in internal/license/pubkeys.txt), so everything is unlocked."}
	}
	st := Status{ServerID: m.ServerID, Tier: Home, State: "none", Checked: m.st.Checked, Tiers: Tiers(), Shop: m.ShopURL,
		Online:  m.CheckURL != "" && m.CheckURL != "off",
		Message: "No subscription: this server runs P8S OS Home (free)."}
	if l := m.lic; l != nil {
		cp := *l
		st.License = &cp
		now := m.now()
		end := l.End()
		st.DaysLeft = int(end.Sub(now).Hours() / 24)
		if end.Before(now) {
			st.DaysLeft = -int(now.Sub(end).Hours()/24) - 1
		}
		switch {
		case m.st.Revoked:
			st.State, st.Message = "revoked", "This licence was cancelled by P8S. "+m.st.Message
		case now.After(end.Add(Grace)):
			st.State, st.Message = "expired", "The subscription expired on "+l.Expires+"; this server is back on Home. Renew it to get the paid features back."
		case now.After(end):
			st.State, st.Tier = "grace", l.Tier
			st.Message = fmt.Sprintf("The subscription expired on %s. Paid features stop on %s; renew it now.", l.Expires, end.Add(Grace).Format("2006-01-02"))
		case st.DaysLeft < 30:
			st.State, st.Tier = "expiring", l.Tier
			st.Message = fmt.Sprintf("The subscription ends on %s (%d days). Renew it to keep the paid features.", l.Expires, st.DaysLeft)
		default:
			st.State, st.Tier = "active", l.Tier
			st.Message = "Subscription active until " + l.Expires + "."
		}
	}
	t, _ := Tier(st.Tier)
	st.TierName = t.Name
	st.Features = append([]string{}, t.Features...)
	st.Notice = !Allows(st.Tier, FeatNoNotice)
	st.Channel = "testing"
	if Allows(st.Tier, FeatStable) {
		st.Channel = "stable"
	}
	if !Allows(st.Tier, FeatUsers) {
		st.UserLimit = HomeUsers
	}
	return st
}

// Allows reports whether this server's tier unlocks a feature.
func (m *Manager) Allows(feature string) bool {
	if m == nil {
		return true // no licensing (tests, development)
	}
	return Allows(m.Status().Tier, feature)
}

// Check asks the licence server whether the key is still good (revoked
// keys stop working; a network failure changes nothing).
func (m *Manager) Check(ctx context.Context) error {
	m.mu.Lock()
	m.load()
	if m.lic == nil || m.CheckURL == "" || m.CheckURL == "off" {
		m.mu.Unlock()
		return nil
	}
	id, server := m.lic.ID, m.ServerID
	url := m.CheckURL
	m.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"id": id, "server": server})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := m.HTTP
	if hc == nil {
		hc = netx.Client(20 * time.Second)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("licence check: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Status  string `json:"status"` // active, revoked, unknown
		Message string `json:"message"`
		Key     string `json:"key"` // a renewed key, when there is one
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("licence check: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return fmt.Errorf("licence check: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lic == nil || m.lic.ID != id {
		return nil // changed meanwhile
	}
	m.st.Checked = m.now().UTC().Format(time.RFC3339)
	m.st.Revoked = out.Status == "revoked"
	m.st.Message = out.Message
	if out.Key != "" { // renewed online: pick up the new key
		if l, err := Parse(m.keys(), out.Key); err == nil && l.Covers(m.ServerID) && l.ID == id && l.Expires > m.lic.Expires {
			m.st.Key, m.lic = strings.Join(strings.Fields(out.Key), ""), &l
		}
	}
	return m.save()
}

// Run checks online once a day.
func (m *Manager) Run(ctx context.Context, onErr func(error)) {
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
