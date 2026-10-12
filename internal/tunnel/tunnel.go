// Package tunnel publishes VMs' web services (and the dashboard) on the
// internet through a Cloudflare Tunnel: cloudflared on the host keeps an
// outgoing connection to Cloudflare, so nothing has to be opened on the router
// and it works behind CGNAT. Each published site is a hostname under the
// host's domain (web1.example.com) routed to a VM's IP and port.
//
// p8sd owns the tunnel's routes (remotely-managed tunnel, set via the API) and
// the matching DNS records; settings live in two 0600 JSON files.
package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Settings is the host's tunnel (tunnel.json). APIToken never leaves p8sd.
type Settings struct {
	APIToken   string `json:"api_token"`
	AccountID  string `json:"account_id"`
	ZoneID     string `json:"zone_id"`
	Domain     string `json:"domain"`
	TunnelID   string `json:"tunnel_id"`
	TunnelName string `json:"tunnel_name"`
	Dashboard  string `json:"dashboard,omitempty"` // public hostname of the dashboard, "" = not published
}

// Site is one published web service of a VM.
type Site struct {
	Host      string `json:"host"` // full hostname, e.g. web1.example.com
	VM        string `json:"vm"`
	Port      int    `json:"port"`
	HTTPS     bool   `json:"https"` // the VM serves HTTPS itself (certificate not checked)
	Owner     string `json:"owner,omitempty"`
	CreatedAt string `json:"created_at"`
	IP        string `json:"ip,omitempty"` // last known address of the VM
}

var (
	ErrNotSetUp = errors.New("public access is not set up on this host (admin: p8 tunnel setup)")
	ErrInvalid  = errors.New("invalid")
	ErrExists   = errors.New("already published")
	ErrNotFound = errors.New("no such site")

	reLabel  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reDomain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	reTunnel = regexp.MustCompile(`^[0-9a-f-]{36}$`)
	reTName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// reserved labels customers can't take.
var reserved = map[string]bool{"www": true, "mail": true, "api": true, "admin": true, "panel": true, "dashboard": true, "p8s": true, "ns1": true, "ns2": true}

// Manager keeps the files, Cloudflare and cloudflared's routes in step.
type Manager struct {
	Dir string // e.g. /var/lib/p8s
	// Dashboard is the local address cloudflared forwards the dashboard to,
	// e.g. https://127.0.0.1:8006 (self-signed: not verified).
	Dashboard string
	// IPs returns the current IPv4 of each VM (missing or "" = unknown).
	IPs func(ctx context.Context) (map[string]string, error)
	// Client makes the Cloudflare client (tests point it at a fake).
	Client func(token string) *Cloudflare
	Now    func() time.Time

	mu      sync.Mutex
	lastPut string // hash of the routes Cloudflare has
}

func (m *Manager) client(tok string) *Cloudflare {
	if m.Client != nil {
		return m.Client(tok)
	}
	return &Cloudflare{Token: tok}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) settingsPath() string { return filepath.Join(m.Dir, "tunnel.json") }
func (m *Manager) sitesPath() string    { return filepath.Join(m.Dir, "sites.json") }

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) settings() (Settings, error) {
	var s Settings
	ok, err := readJSON(m.settingsPath(), &s)
	if err != nil {
		return s, err
	}
	if !ok || s.TunnelID == "" {
		return s, ErrNotSetUp
	}
	return s, nil
}

func (m *Manager) sites() ([]Site, error) {
	var l []Site
	_, err := readJSON(m.sitesPath(), &l)
	if l == nil {
		l = []Site{}
	}
	return l, err
}

// Status is what the dashboard shows (no API token).
type Status struct {
	SetUp      bool   `json:"set_up"`
	Domain     string `json:"domain,omitempty"`
	TunnelName string `json:"tunnel_name,omitempty"`
	TunnelID   string `json:"tunnel_id,omitempty"`
	Dashboard  string `json:"dashboard,omitempty"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.settings()
	if err != nil {
		return Status{}
	}
	return Status{SetUp: true, Domain: s.Domain, TunnelName: s.TunnelName, TunnelID: s.TunnelID, Dashboard: s.Dashboard}
}

// Setup connects the host to Cloudflare: checks the API token, finds the
// domain, creates (or reuses) the tunnel and returns the token cloudflared
// needs (`cloudflared service install <token>`).
func (m *Manager) Setup(ctx context.Context, apiToken, domain, name string) (Status, string, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	apiToken = strings.TrimSpace(apiToken)
	if !reDomain.MatchString(domain) {
		return Status{}, "", fmt.Errorf("%w: %q is not a domain name like example.com", ErrInvalid, domain)
	}
	if !reTName.MatchString(name) {
		return Status{}, "", fmt.Errorf("%w: tunnel name %q", ErrInvalid, name)
	}
	if apiToken == "" {
		return Status{}, "", fmt.Errorf("%w: a Cloudflare API token is needed", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cf := m.client(apiToken)
	zone, account, err := cf.Zone(ctx, domain)
	if err != nil {
		return Status{}, "", err
	}
	id, err := cf.FindTunnel(ctx, account, name)
	if err != nil {
		return Status{}, "", fmt.Errorf("%w (does the API token have Account › Cloudflare Tunnel › Edit?)", err)
	}
	if id == "" {
		if id, err = cf.CreateTunnel(ctx, account, name); err != nil {
			return Status{}, "", err
		}
	}
	if !reTunnel.MatchString(id) {
		return Status{}, "", fmt.Errorf("cloudflare returned an odd tunnel id %q", id)
	}
	tok, err := cf.TunnelToken(ctx, account, id)
	if err != nil {
		return Status{}, "", err
	}
	old, _ := m.settings()
	s := Settings{APIToken: apiToken, AccountID: account, ZoneID: zone, Domain: domain, TunnelID: id, TunnelName: name}
	if old.TunnelID == id && old.Domain == domain {
		s.Dashboard = old.Dashboard
	}
	if err := writeJSON(m.settingsPath(), s); err != nil {
		return Status{}, "", err
	}
	m.lastPut = ""
	if err := m.sync(ctx); err != nil {
		return Status{}, "", err
	}
	return Status{SetUp: true, Domain: s.Domain, TunnelName: name, TunnelID: id, Dashboard: s.Dashboard}, tok, nil
}

// SetDashboard publishes the dashboard as label.domain ("" = take it off the internet).
func (m *Manager) SetDashboard(ctx context.Context, label string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.settings()
	if err != nil {
		return Status{}, err
	}
	cf := m.client(s.APIToken)
	label = strings.ToLower(strings.TrimSpace(label))
	if label == "" {
		if s.Dashboard != "" {
			if err := cf.DeleteCNAME(ctx, s.ZoneID, s.Dashboard, s.TunnelID); err != nil {
				return Status{}, err
			}
		}
		s.Dashboard = ""
	} else {
		if !reLabel.MatchString(label) {
			return Status{}, fmt.Errorf("%w: %q: use letters, digits and hyphens", ErrInvalid, label)
		}
		host := label + "." + s.Domain
		if sites, _ := m.sites(); hasHost(sites, host) {
			return Status{}, fmt.Errorf("%w: %s is used by a VM", ErrExists, host)
		}
		if err := cf.EnsureCNAME(ctx, s.ZoneID, host, s.TunnelID); err != nil {
			return Status{}, err
		}
		if s.Dashboard != "" && s.Dashboard != host {
			_ = cf.DeleteCNAME(ctx, s.ZoneID, s.Dashboard, s.TunnelID)
		}
		s.Dashboard = host
	}
	if err := writeJSON(m.settingsPath(), s); err != nil {
		return Status{}, err
	}
	if err := m.sync(ctx); err != nil {
		return Status{}, err
	}
	return Status{SetUp: true, Domain: s.Domain, TunnelName: s.TunnelName, TunnelID: s.TunnelID, Dashboard: s.Dashboard}, nil
}

func hasHost(l []Site, host string) bool {
	for _, x := range l {
		if x.Host == host {
			return true
		}
	}
	return false
}

// Sites lists published sites; vm or owner "" = all.
func (m *Manager) Sites(vm, owner string) ([]Site, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.sites()
	out := []Site{}
	for _, s := range all {
		if (vm == "" || s.VM == vm) && (owner == "" || s.Owner == owner) {
			out = append(out, s)
		}
	}
	return out, err
}

// Add publishes label.domain -> vm:port.
func (m *Manager) Add(ctx context.Context, label, vm string, port int, https bool, owner string) (Site, error) {
	label = strings.ToLower(strings.TrimSpace(label))
	if !reLabel.MatchString(label) {
		return Site{}, fmt.Errorf("%w: name %q: use lowercase letters, digits and hyphens (one level, e.g. shop)", ErrInvalid, label)
	}
	if reserved[label] && owner != "" {
		return Site{}, fmt.Errorf("%w: %q is reserved; pick another name", ErrInvalid, label)
	}
	if port < 1 || port > 65535 {
		return Site{}, fmt.Errorf("%w: port must be 1-65535", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.settings()
	if err != nil {
		return Site{}, err
	}
	host := label + "." + s.Domain
	all, err := m.sites()
	if err != nil {
		return Site{}, err
	}
	if hasHost(all, host) || host == s.Dashboard {
		return Site{}, fmt.Errorf("%w: %s is taken", ErrExists, host)
	}
	if err := m.client(s.APIToken).EnsureCNAME(ctx, s.ZoneID, host, s.TunnelID); err != nil {
		if errors.Is(err, ErrNameTaken) {
			return Site{}, fmt.Errorf("%w: %v", ErrExists, err)
		}
		return Site{}, err
	}
	site := Site{Host: host, VM: vm, Port: port, HTTPS: https, Owner: owner, CreatedAt: m.now().UTC().Format(time.RFC3339)}
	all = append(all, site)
	if err := writeJSON(m.sitesPath(), all); err != nil {
		return Site{}, err
	}
	if err := m.sync(ctx); err != nil {
		return site, fmt.Errorf("published, but the route isn't active yet: %w", err)
	}
	if s2, _ := m.sitesAfter(host); s2 != nil { // sync filled in the IP
		site = *s2
	}
	return site, nil
}

func (m *Manager) sitesAfter(host string) (*Site, error) {
	all, err := m.sites()
	for i := range all {
		if all[i].Host == host {
			return &all[i], err
		}
	}
	return nil, err
}

// Get returns one site.
func (m *Manager) Get(host string) (Site, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.sitesAfter(host)
	if err != nil {
		return Site{}, err
	}
	if s == nil {
		return Site{}, fmt.Errorf("%w: %s", ErrNotFound, host)
	}
	return *s, nil
}

// Remove takes a site off the internet.
func (m *Manager) Remove(ctx context.Context, host string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remove(ctx, func(x Site) bool { return x.Host == host }, true)
}

// RemoveVM takes all of a deleted VM's sites off.
func (m *Manager) RemoveVM(ctx context.Context, vm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.settings(); err != nil {
		return nil
	}
	return m.remove(ctx, func(x Site) bool { return x.VM == vm }, false)
}

func (m *Manager) remove(ctx context.Context, match func(Site) bool, mustExist bool) error {
	s, err := m.settings()
	if err != nil {
		return err
	}
	all, err := m.sites()
	if err != nil {
		return err
	}
	keep, gone := []Site{}, []Site{}
	for _, x := range all {
		if match(x) {
			gone = append(gone, x)
		} else {
			keep = append(keep, x)
		}
	}
	if len(gone) == 0 {
		if mustExist {
			return ErrNotFound
		}
		return nil
	}
	if err := writeJSON(m.sitesPath(), keep); err != nil {
		return err
	}
	if err := m.sync(ctx); err != nil { // route gone before the DNS record
		return err
	}
	cf := m.client(s.APIToken)
	var errs []error
	for _, x := range gone {
		errs = append(errs, cf.DeleteCNAME(ctx, s.ZoneID, x.Host, s.TunnelID))
	}
	return errors.Join(errs...)
}

// Sync updates VM addresses and pushes the routes to Cloudflare when they
// changed (p8sd calls it every minute and after every change).
func (m *Manager) Sync(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.settings(); errors.Is(err, ErrNotSetUp) {
		return nil
	}
	return m.sync(ctx)
}

func (m *Manager) sync(ctx context.Context) error {
	s, err := m.settings()
	if err != nil {
		return err
	}
	all, err := m.sites()
	if err != nil {
		return err
	}
	if m.IPs != nil && len(all) > 0 {
		ips, err := m.IPs(ctx)
		if err == nil {
			changed := false
			for i := range all {
				if ip := ips[all[i].VM]; ip != "" && ip != all[i].IP {
					all[i].IP, changed = ip, true
				}
			}
			if changed {
				if err := writeJSON(m.sitesPath(), all); err != nil {
					return err
				}
			}
		}
	}
	rules := Routes(s, all, m.Dashboard)
	b, _ := json.Marshal(rules)
	h := sha256.Sum256(append([]byte(s.TunnelID), b...))
	sum := hex.EncodeToString(h[:])
	if sum == m.lastPut {
		return nil
	}
	if err := m.client(s.APIToken).PutIngress(ctx, s.AccountID, s.TunnelID, rules); err != nil {
		return err
	}
	m.lastPut = sum
	return nil
}

// Routes builds the tunnel's ingress rules. Sites whose VM never had an
// address are left out until it gets one.
func Routes(s Settings, sites []Site, dashboard string) []Ingress {
	var out []Ingress
	if s.Dashboard != "" && dashboard != "" {
		r := Ingress{Hostname: s.Dashboard, Service: dashboard}
		if strings.HasPrefix(dashboard, "https://") {
			r.OriginRequest = map[string]any{"noTLSVerify": true}
		}
		out = append(out, r)
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Host < sites[j].Host })
	for _, x := range sites {
		if x.IP == "" {
			continue
		}
		scheme := "http"
		r := Ingress{Hostname: x.Host}
		if x.HTTPS {
			scheme = "https"
			r.OriginRequest = map[string]any{"noTLSVerify": true}
		}
		r.Service = fmt.Sprintf("%s://%s:%d", scheme, x.IP, x.Port)
		out = append(out, r)
	}
	return out
}
