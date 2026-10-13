// Package cluster joins several P8S OS servers into one: a master (dashboard,
// users, billing, placement) and workers ("extenders") that run VMs for it.
//
// Servers talk over a WireGuard tunnel the master runs (10.250.0.0/24, UDP
// 51820). A worker dials out to the master, so it can sit behind NAT/CGNAT,
// in another building or another city. Over the tunnel the master calls each
// worker's normal HTTPS API with a token the worker gave it when it joined;
// TLS certificates are pinned both ways.
//
// Joining: the master admin makes a one-time join code; on the new server
// `p8 join https://MASTER:8006 CODE` registers it (the code also pins the
// master's certificate, so the first contact can't be intercepted).
package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/execx"
)

const (
	Subnet    = "10.250.0"
	Iface     = "p8s-wg"
	WGPort    = 51820
	joinValid = time.Hour
)

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
	ErrRole     = errors.New("wrong role")
	reNodeName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,62}$`)
)

// Node is one server of the cluster.
type Node struct {
	ID      string `json:"id"`   // n1 (master), n2, n3 …
	Name    string `json:"name"` // host name, shown in the dashboard
	WGIP    string `json:"wg_ip"`
	WGPub   string `json:"wg_pub"`
	API     string `json:"api"`             // https://10.250.0.2:8006
	Token   string `json:"token,omitempty"` // to call this node's API (kept by the master only)
	CertSHA string `json:"cert_sha"`        // pinned TLS certificate (sha256 of DER)
	Joined  string `json:"joined,omitempty"`
}

type joinCode struct {
	Hash    string `json:"hash"`
	Expires string `json:"expires"`
}

// State is what this server knows about the cluster (data_dir/cluster/state.json).
type State struct {
	Role     string     `json:"role"` // "", master, worker
	Self     Node       `json:"self"`
	WGPriv   string     `json:"wg_priv,omitempty"`
	Endpoint string     `json:"endpoint,omitempty"` // master: host:port workers dial
	Nodes    []Node     `json:"nodes"`              // master: workers; worker: [master]
	Joins    []joinCode `json:"joins,omitempty"`    // master: unused join codes (hashed)
	Accept   []string   `json:"accept,omitempty"`   // tokens (sha256) that may call this server as the cluster
}

// Status is how a node is doing, as the master last saw it.
type Status struct {
	Online   bool     `json:"online"`
	Seen     string   `json:"seen,omitempty"`
	Error    string   `json:"error,omitempty"`
	Capacity Capacity `json:"capacity"`
}

// Capacity is what a node has and what its VMs already take.
type Capacity struct {
	CPUs        int      `json:"cpus"`
	MemTotalMiB int      `json:"mem_total_mib"`
	MemUsedMiB  int      `json:"mem_used_mib"` // given to VMs and containers
	PoolFreeGB  float64  `json:"pool_free_gb"`
	VMs         int      `json:"vms"`
	Networks    []string `json:"networks"`
	Version     string   `json:"version,omitempty"`
}

// Manager keeps the cluster state of this server.
type Manager struct {
	Dir      string       // data_dir/cluster
	CertFile string       // this server's TLS certificate (pinned by the others)
	APIPort  string       // port p8sd listens on, e.g. "8006"
	Run      execx.Runner // ip / wg; nil = no tunnel (tests)
	Log      *slog.Logger
	HTTP     func(pin string) *http.Client // tests; nil = pinned TLS client
	Now      func() time.Time
	// LocalCapacity describes this server (for placement and the dashboard).
	LocalCapacity func(ctx context.Context) Capacity
	TestURL       string // tests: this server's API URL (no tunnel)

	mu     sync.Mutex
	st     State
	loaded bool
	status map[string]*Status
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) path() string { return filepath.Join(m.Dir, "state.json") }

func (m *Manager) load() error {
	if m.loaded {
		return nil
	}
	if m.status == nil {
		m.status = map[string]*Status{}
	}
	b, err := os.ReadFile(m.path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(b, &m.st); err != nil {
			return err
		}
	}
	m.loaded = true
	return nil
}

func (m *Manager) save() error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m.st, "", "  ")
	if err := os.WriteFile(m.path()+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(m.path()+".tmp", m.path())
}

// State returns a copy of the state (secrets left out).
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	st := m.st
	st.WGPriv, st.Joins, st.Accept = "", nil, nil
	st.Nodes = make([]Node, len(m.st.Nodes))
	for i, n := range m.st.Nodes {
		n.Token = ""
		st.Nodes[i] = n
	}
	return st
}

// Role is "", "master" or "worker".
func (m *Manager) Role() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	return m.st.Role
}

// Workers returns the master's workers (with their tokens).
func (m *Manager) Workers() []Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	if m.st.Role != "master" {
		return nil
	}
	return append([]Node(nil), m.st.Nodes...)
}

// SelfID is this server's node id ("" when not clustered).
func (m *Manager) SelfID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	return m.st.Self.ID
}

// SelfName is this server's name in the cluster.
func (m *Manager) SelfName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	if m.st.Self.Name != "" {
		return m.st.Self.Name
	}
	h, _ := os.Hostname()
	return h
}

// Accepts reports whether tok is a cluster token for this server.
func (m *Manager) Accepts(tok string) bool {
	if tok == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	sum := sha256.Sum256([]byte(tok))
	got := []byte(hex.EncodeToString(sum[:]))
	ok := 0
	for _, h := range m.st.Accept {
		ok |= subtle.ConstantTimeCompare(got, []byte(h))
	}
	return ok == 1
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(b), "="))
}

func hashOf(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

// CertSHA is the sha256 of a PEM certificate file's DER bytes.
func CertSHA(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return "", fmt.Errorf("%s: no certificate", file)
	}
	sum := sha256.Sum256(blk.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// pinned is an HTTPS client that only trusts a certificate with this sha256.
func (m *Manager) client(pin string) *http.Client {
	if m.HTTP != nil {
		return m.HTTP(pin)
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy: nil, // cluster traffic never goes through a web proxy
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, // replaced by the pin check below
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("no certificate")
				}
				sum := sha256.Sum256(raw[0])
				if !strings.HasPrefix(hex.EncodeToString(sum[:]), pin) || len(pin) < 16 {
					return fmt.Errorf("certificate doesn't match the cluster (got %x…)", sum[:6])
				}
				return nil
			}},
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}}
}

// --- master: join codes and registration ---

// JoinCode makes a one-time code for a new server (valid one hour). The first
// call turns this server into the master. endpoint is how workers reach the
// tunnel ("host" or "host:port"); masterURL is how they reach this dashboard.
func (m *Manager) JoinCode(ctx context.Context, masterURL, endpoint string) (code, command string, err error) {
	u, err := url.Parse(masterURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", "", fmt.Errorf("%w: master address must be like https://192.168.0.106:8006", ErrInvalid)
	}
	if endpoint == "" {
		endpoint = u.Hostname()
	}
	if !strings.Contains(strings.TrimPrefix(endpoint, "["), "]:") && strings.Count(endpoint, ":") != 1 {
		endpoint = net.JoinHostPort(strings.Trim(endpoint, "[]"), fmt.Sprint(WGPort))
	}
	if _, _, err := net.SplitHostPort(endpoint); err != nil {
		return "", "", fmt.Errorf("%w: tunnel address: %v", ErrInvalid, err)
	}
	pin, err := CertSHA(m.CertFile)
	if err != nil {
		return "", "", fmt.Errorf("this server's certificate: %w", err)
	}
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return "", "", err
	}
	switch m.st.Role {
	case "worker":
		m.mu.Unlock()
		return "", "", fmt.Errorf("%w: this server is a worker; add servers on the master", ErrRole)
	case "":
		priv, pub, err := wgKeys()
		if err != nil {
			m.mu.Unlock()
			return "", "", err
		}
		host, _ := os.Hostname()
		m.st = State{Role: "master", WGPriv: priv,
			Self: Node{ID: "n1", Name: host, WGIP: Subnet + ".1", WGPub: pub, CertSHA: pin, Joined: m.now().UTC().Format(time.RFC3339)}}
	}
	m.st.Endpoint = endpoint
	secret := randToken(15)
	now := m.now()
	keep := m.st.Joins[:0]
	for _, j := range m.st.Joins {
		if t, err := time.Parse(time.RFC3339, j.Expires); err == nil && t.After(now) {
			keep = append(keep, j)
		}
	}
	m.st.Joins = append(keep, joinCode{Hash: hashOf(secret), Expires: now.Add(joinValid).UTC().Format(time.RFC3339)})
	err = m.save()
	m.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	if err := m.applyWG(ctx); err != nil {
		return "", "", fmt.Errorf("start the tunnel: %w", err)
	}
	code = secret + "." + pin[:32]
	return code, fmt.Sprintf("p8 join %s %s", strings.TrimRight(masterURL, "/"), code), nil
}

// RegisterRequest is what a joining server sends the master.
type RegisterRequest struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	WGPub   string `json:"wg_pub"`
	Token   string `json:"token"` // the master calls the worker's API with it
	CertSHA string `json:"cert_sha"`
	APIPort string `json:"api_port"`
	APIURL  string `json:"api_url,omitempty"` // tests only (no tunnel)
}

// RegisterReply tells the new worker how to reach the master.
type RegisterReply struct {
	ID        string `json:"id"`
	WGIP      string `json:"wg_ip"`
	MasterID  string `json:"master_id"`
	MasterPub string `json:"master_wg_pub"`
	MasterIP  string `json:"master_wg_ip"`
	Endpoint  string `json:"endpoint"`
	Name      string `json:"master_name"`
	CertSHA   string `json:"master_cert_sha"`
	MasterAPI string `json:"master_api"`
	YourName  string `json:"your_name"` // the name the master gave this server (made unique)
}

// Register adds a worker (called by the joining server with its join code).
func (m *Manager) Register(ctx context.Context, r RegisterRequest) (RegisterReply, error) {
	secret, _, _ := strings.Cut(r.Code, ".")
	if !reNodeName.MatchString(r.Name) {
		return RegisterReply{}, fmt.Errorf("%w: server name", ErrInvalid)
	}
	if _, err := wgPubOK(r.WGPub); err != nil || len(r.Token) < 20 || len(r.CertSHA) != 64 {
		return RegisterReply{}, fmt.Errorf("%w: incomplete join request", ErrInvalid)
	}
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return RegisterReply{}, err
	}
	if m.st.Role != "master" {
		m.mu.Unlock()
		return RegisterReply{}, fmt.Errorf("%w: this server isn't a cluster master", ErrRole)
	}
	now, idx := m.now(), -1
	for i, j := range m.st.Joins {
		t, _ := time.Parse(time.RFC3339, j.Expires)
		if subtle.ConstantTimeCompare([]byte(j.Hash), []byte(hashOf(secret))) == 1 && t.After(now) {
			idx = i
		}
	}
	if idx < 0 {
		m.mu.Unlock()
		return RegisterReply{}, fmt.Errorf("%w: the join code is wrong, used or expired (make a new one on the master)", ErrInvalid)
	}
	m.st.Joins = append(m.st.Joins[:idx], m.st.Joins[idx+1:]...)
	// next free id and address; the same machine joining again (same TLS
	// certificate) replaces its old entry
	used := map[string]bool{"1": true}
	for i := 0; i < len(m.st.Nodes); i++ {
		if m.st.Nodes[i].CertSHA == r.CertSHA || m.st.Nodes[i].WGPub == r.WGPub {
			m.st.Nodes = append(m.st.Nodes[:i], m.st.Nodes[i+1:]...)
			i--
			continue
		}
		used[strings.TrimPrefix(m.st.Nodes[i].ID, "n")] = true
	}
	// names must tell servers apart (installed laptops are often all "p8s-node")
	taken := map[string]bool{strings.ToLower(m.st.Self.Name): true}
	for _, x := range m.st.Nodes {
		taken[strings.ToLower(x.Name)] = true
	}
	for base, k := r.Name, 2; taken[strings.ToLower(r.Name)]; k++ {
		r.Name = fmt.Sprintf("%s-%d", base, k)
	}
	n := 2
	for used[fmt.Sprint(n)] {
		n++
	}
	if n > 250 {
		m.mu.Unlock()
		return RegisterReply{}, fmt.Errorf("%w: the cluster is full", ErrInvalid)
	}
	port := r.APIPort
	if port == "" {
		port = "8006"
	}
	node := Node{ID: fmt.Sprintf("n%d", n), Name: r.Name, WGIP: fmt.Sprintf("%s.%d", Subnet, n), WGPub: r.WGPub,
		Token: r.Token, CertSHA: r.CertSHA, Joined: now.UTC().Format(time.RFC3339)}
	node.API = "https://" + net.JoinHostPort(node.WGIP, port)
	if r.APIURL != "" && m.Run == nil { // tests: no tunnel
		node.API = r.APIURL
	}
	m.st.Nodes = append(m.st.Nodes, node)
	sort.Slice(m.st.Nodes, func(i, j int) bool { return m.st.Nodes[i].ID < m.st.Nodes[j].ID })
	err := m.save()
	self, ep := m.st.Self, m.st.Endpoint
	m.mu.Unlock()
	if err != nil {
		return RegisterReply{}, err
	}
	if err := m.applyWG(ctx); err != nil {
		return RegisterReply{}, fmt.Errorf("add to the tunnel: %w", err)
	}
	if m.Log != nil {
		m.Log.Info("server joined the cluster", "id", node.ID, "name", node.Name, "wg_ip", node.WGIP)
	}
	return RegisterReply{ID: node.ID, WGIP: node.WGIP, MasterID: self.ID, MasterPub: self.WGPub, MasterIP: self.WGIP,
		Endpoint: ep, Name: self.Name, CertSHA: self.CertSHA, MasterAPI: "https://" + net.JoinHostPort(self.WGIP, m.port()), YourName: node.Name}, nil
}

func (m *Manager) port() string {
	if m.APIPort != "" {
		return m.APIPort
	}
	return "8006"
}

// Remove drops a worker from the cluster (its VMs keep running there).
func (m *Manager) Remove(ctx context.Context, id string) error {
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return err
	}
	found := false
	for i, n := range m.st.Nodes {
		if n.ID == id {
			m.st.Nodes = append(m.st.Nodes[:i], m.st.Nodes[i+1:]...)
			found = true
			break
		}
	}
	if !found || m.st.Role != "master" {
		m.mu.Unlock()
		return fmt.Errorf("%w: server %s", ErrNotFound, id)
	}
	delete(m.status, id)
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return m.applyWG(ctx)
}

// --- worker: joining and leaving ---

// Join registers this server with a master (`p8 join URL CODE`).
func (m *Manager) Join(ctx context.Context, masterURL, code string) (RegisterReply, error) {
	secret, pin, ok := strings.Cut(strings.TrimSpace(code), ".")
	if !ok || len(secret) < 20 || len(pin) < 16 {
		return RegisterReply{}, fmt.Errorf("%w: the join code looks incomplete; copy the whole line from the master", ErrInvalid)
	}
	u, err := url.Parse(strings.TrimRight(masterURL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return RegisterReply{}, fmt.Errorf("%w: master address must be like https://192.168.0.106:8006", ErrInvalid)
	}
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return RegisterReply{}, err
	}
	if m.st.Role != "" {
		m.mu.Unlock()
		return RegisterReply{}, fmt.Errorf("%w: this server is already a cluster %s (p8 cluster leave first)", ErrRole, m.st.Role)
	}
	m.mu.Unlock()
	myPin, err := CertSHA(m.CertFile)
	if err != nil {
		return RegisterReply{}, fmt.Errorf("this server's certificate: %w", err)
	}
	priv, pub, err := wgKeys()
	if err != nil {
		return RegisterReply{}, err
	}
	token := randToken(30)
	host, _ := os.Hostname()
	req := RegisterRequest{Code: code, Name: host, WGPub: pub, Token: token, CertSHA: myPin, APIPort: m.port()}
	if m.Run == nil && m.TestURL != "" {
		req.APIURL = m.TestURL
	}
	body, _ := json.Marshal(req)
	hr, _ := http.NewRequestWithContext(ctx, "POST", u.String()+"/api/v1/cluster/register", bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	resp, err := m.client(pin).Do(hr)
	if err != nil {
		return RegisterReply{}, fmt.Errorf("reach the master: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return RegisterReply{}, fmt.Errorf("the master said: %s", apiError(resp))
	}
	var rep RegisterReply
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return RegisterReply{}, err
	}
	m.mu.Lock()
	m.st = State{Role: "worker", WGPriv: priv, Endpoint: rep.Endpoint,
		Self:   Node{ID: rep.ID, Name: firstOf(rep.YourName, host), WGIP: rep.WGIP, WGPub: pub, CertSHA: myPin, Joined: m.now().UTC().Format(time.RFC3339)},
		Nodes:  []Node{{ID: rep.MasterID, Name: rep.Name, WGIP: rep.MasterIP, WGPub: rep.MasterPub, CertSHA: rep.CertSHA, API: rep.MasterAPI}},
		Accept: []string{hashOf(token)}}
	err = m.save()
	m.mu.Unlock()
	if err != nil {
		return RegisterReply{}, err
	}
	if err := m.applyWG(ctx); err != nil {
		return rep, fmt.Errorf("joined, but the tunnel didn't start: %w", err)
	}
	return rep, nil
}

// Leave makes this server standalone again (the master should remove it too).
func (m *Manager) Leave(ctx context.Context) error {
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return err
	}
	if m.st.Role == "master" && len(m.st.Nodes) > 0 {
		m.mu.Unlock()
		return fmt.Errorf("%w: remove the other servers first", ErrRole)
	}
	m.st = State{}
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return m.downWG(ctx)
}

func apiError(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
