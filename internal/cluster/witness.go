package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"p8sos/internal/witness"
)

// The witness (QDevice): a third vote for a cluster of two servers, on any
// always-on box running p8s-witness. With it set, two servers and the
// witness are three voters, so the majority rules of quorum.go apply: the
// surviving server takes over by itself when the master dies, and a server
// cut off from both the other server and the witness never does.

// WitnessHTTP is set by tests (nil: the pinned TLS client).
var WitnessHTTP *http.Client

func (m *Manager) witness() *witness.Client {
	if m.st.Witness == nil || m.st.Witness.URL == "" {
		return nil
	}
	return &witness.Client{Conf: *m.st.Witness, HTTP: WitnessHTTP}
}

// voters is the number of votes in the cluster: the servers plus the witness
// (caller holds mu).
func (m *Manager) voters() int {
	if m.st.Role == "" {
		return 1
	}
	n := len(m.st.Nodes) + len(m.st.Mesh) + 1
	if m.witness() != nil {
		n++
	}
	return n
}

// Voters is how many votes the cluster has (servers + witness).
func (m *Manager) Voters() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	return m.voters()
}

var rePin = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cleanWitness checks a witness setting.
func cleanWitness(c witness.Conf) (witness.Conf, error) {
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	c.Token = strings.TrimSpace(c.Token)
	c.Pin = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(c.Pin), ":", ""))
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" {
		return c, fmt.Errorf("%w: the witness address is like https://192.168.0.50:8007", ErrInvalid)
	}
	if len(c.Token) < 16 || strings.ContainsAny(c.Token, " \t\r\n") {
		return c, fmt.Errorf("%w: the witness key is the token p8s-witness init printed", ErrInvalid)
	}
	if !rePin.MatchString(c.Pin) {
		return c, fmt.Errorf("%w: the fingerprint is the 64 hex characters p8s-witness init printed", ErrInvalid)
	}
	return c, nil
}

// SetWitness adds (or changes) the cluster's witness. Master only; it must answer.
func (m *Manager) SetWitness(ctx context.Context, c witness.Conf) (witness.Reply, error) {
	c, err := cleanWitness(c)
	if err != nil {
		return witness.Reply{}, err
	}
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return witness.Reply{}, err
	}
	if m.st.Role != "master" {
		m.mu.Unlock()
		return witness.Reply{}, fmt.Errorf("%w: set the witness on the cluster master", ErrRole)
	}
	if n := len(m.st.Nodes) + 1; n != 2 {
		m.mu.Unlock()
		return witness.Reply{}, fmt.Errorf("%w: a witness is for clusters of exactly 2 servers (this one has %d)", ErrRole, n)
	}
	c.Cluster = "c" + randToken(12)
	if m.st.Witness != nil && m.st.Witness.Cluster != "" {
		c.Cluster = m.st.Witness.Cluster // same cluster: keep its record at the witness
	}
	term, self := m.st.Term, m.st.Self.ID
	m.mu.Unlock()
	cl := &witness.Client{Conf: c, HTTP: WitnessHTTP}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	r, err := cl.Beat(cctx, term, self)
	if err != nil {
		return r, fmt.Errorf("the witness at %s: %v", c.URL, err)
	}
	if !r.OK {
		return r, fmt.Errorf("the witness refused: %s", r.Why)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Witness = &c
	return r, m.save()
}

// ClearWitness removes the witness (master only).
func (m *Manager) ClearWitness() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return err
	}
	if m.st.Role != "master" {
		return fmt.Errorf("%w: remove the witness on the cluster master", ErrRole)
	}
	m.st.Witness = nil
	return m.save()
}

// WitnessView is the witness as the dashboard shows it (no key).
type WitnessView struct {
	URL        string `json:"url"`
	Online     bool   `json:"online"`
	Error      string `json:"error,omitempty"`
	Term       int    `json:"term"`
	MasterSeen int    `json:"master_seen_s"`
}

// WitnessStatus asks the witness how it is (nil: none set).
func (m *Manager) WitnessStatus(ctx context.Context) *WitnessView {
	m.mu.Lock()
	_ = m.load()
	cl := m.witness()
	m.mu.Unlock()
	if cl == nil {
		return nil
	}
	v := &WitnessView{URL: cl.Conf.URL, MasterSeen: -1}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second) // the dashboard waits for it
	defer cancel()
	r, err := cl.Ping(cctx)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Online, v.Term, v.MasterSeen = true, r.Term, r.MasterSeen
	return v
}
