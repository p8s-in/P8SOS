package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"p8sos/internal/witness"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// No single master: with 3 or more servers the cluster survives losing its
// master. The master hands every server the cluster book (all servers, their
// keys and addresses, and the master's settings such as accounts and HA)
// every few seconds. When the master has been silent for MasterTimeout, the
// lowest-numbered server that still sees the others asks for votes; with a
// majority of all servers it becomes the master of the next term. A master
// that can no longer reach a majority stops acting as master and switches its
// protected VMs off (they start on the other side), so there is never more
// than one master — and never a VM running twice.
//
// Two servers can't outvote each other: there, a server takes over only by
// hand (`p8 cluster promote`, when you know the master is really gone) —
// unless the cluster has a witness (witness.go), the third vote.

// Book is the replicated cluster state the master sends every server.
type Book struct {
	Term     int    `json:"term"`
	MasterID string `json:"master_id"`
	Nodes    []Node `json:"nodes"` // every server, the master too, with the key to call it
	// Files are the master's settings that a new master carries on with
	// (accounts, high availability, IP pools, security, sign-in), by path
	// relative to the data directory.
	Files map[string][]byte `json:"files,omitempty"`
	At    string            `json:"at"`
	// Witness: the two-server tie-breaker (every server needs it to campaign).
	Witness *witness.Conf `json:"witness,omitempty"`
}

const (
	// MasterTimeout: a server that hasn't had the book this long thinks the master is gone.
	MasterTimeout = 45 * time.Second
	// QuorumTimeout: a master that reaches no majority this long stops being the master.
	QuorumTimeout = 30 * time.Second
	maxBookFile   = 4 << 20
)

// ErrStale means a book or vote is from an older term than this server knows.
var ErrStale = errors.New("stale term")

// PeerInfo is what a server tells the others during an election.
type PeerInfo struct {
	ID       string `json:"id"`
	Role     string `json:"role"`
	Term     int    `json:"term"`
	MasterID string `json:"master_id"`
	// MasterSeen: seconds since this server last heard from the master (-1 = it is the master).
	MasterSeen int `json:"master_seen_s"`
}

// VoteRequest asks a server to accept a new master.
type VoteRequest struct {
	Term      int    `json:"term"`
	Candidate string `json:"candidate"`
}

// VoteReply is the answer.
type VoteReply struct {
	Granted bool   `json:"granted"`
	Term    int    `json:"term"`
	Why     string `json:"why,omitempty"`
}

func (m *Manager) selfAPI() string {
	if m.Run == nil && m.TestURL != "" {
		return m.TestURL
	}
	return "https://" + m.st.Self.WGIP + ":" + m.port()
}

// ensureKey gives this server a key the others call it with (caller holds mu).
func (m *Manager) ensureKey() bool {
	if m.st.SelfToken != "" {
		return false
	}
	m.st.SelfToken = randToken(30)
	m.st.Accept = append(m.st.Accept, hashOf(m.st.SelfToken))
	return true
}

// buildBook is the book the master hands out now (caller holds mu).
func (m *Manager) buildBook() Book {
	self := m.st.Self
	self.Token, self.API, self.Endpoint = m.st.SelfToken, m.selfAPI(), m.st.Endpoint
	b := Book{Term: m.st.Term, MasterID: self.ID, Nodes: []Node{self}, At: m.now().UTC().Format(time.RFC3339)}
	b.Nodes = append(b.Nodes, m.st.Nodes...)
	b.Witness = m.st.Witness
	b.Files = map[string][]byte{}
	for _, f := range m.BookFiles {
		p := filepath.Join(m.DataDir, f)
		if st, err := os.Stat(p); err == nil && st.Size() <= maxBookFile {
			if data, err := os.ReadFile(p); err == nil {
				b.Files[f] = data
			}
		}
	}
	return b
}

// pushBook gives every worker the current book (the master runs it with Poll).
func (m *Manager) pushBook(ctx context.Context) {
	m.mu.Lock()
	if err := m.load(); err != nil || m.st.Role != "master" {
		m.mu.Unlock()
		return
	}
	if m.ensureKey() {
		_ = m.save()
	}
	b := m.buildBook()
	workers := append([]Node(nil), m.st.Nodes...)
	wit, voters := m.witness(), m.voters()
	m.mu.Unlock()
	reached := 1
	if wit != nil {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		r, err := wit.Beat(cctx, b.Term, b.MasterID)
		cancel()
		switch {
		case err == nil && r.OK:
			reached++
		case err == nil && r.Term > b.Term: // a newer master told the witness first
			m.demote(context.WithoutCancel(ctx), r.Term, "the witness follows a newer master")
			return
		}
	}
	for _, n := range workers {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		var r VoteReply
		err := m.call(cctx, n, "POST", "/cluster/book", b, &r)
		cancel()
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == "stale" { // someone else is the master of a newer term
			var p PeerInfo
			pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			if m.call(pctx, n, "GET", "/cluster/peer", nil, &p) == nil {
				m.demote(context.WithoutCancel(ctx), p.Term, "server "+n.Name+" follows a newer master")
			}
			cancel()
			return
		}
		if err == nil {
			reached++
		}
	}
	m.mu.Lock()
	if reached*2 > voters || voters < 3 {
		m.quorumAt = m.now()
	}
	m.mu.Unlock()
}

// HasQuorum says whether this master still reaches a majority of the voters
// (always true below 3 voters: there is nobody to outvote it).
func (m *Manager) HasQuorum() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	if m.st.Role != "master" || m.voters() < 3 {
		return true
	}
	if m.quorumAt.IsZero() {
		m.quorumAt = m.now() // just started: give it a moment
	}
	return m.now().Sub(m.quorumAt) < QuorumTimeout
}

// ReceiveBook takes the master's book (worker side, or an old master learning
// it was replaced).
func (m *Manager) ReceiveBook(ctx context.Context, b Book) (VoteReply, error) {
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return VoteReply{}, err
	}
	if m.st.Role == "" {
		m.mu.Unlock()
		return VoteReply{}, fmt.Errorf("%w: this server isn't in a cluster", ErrRole)
	}
	if b.Term < m.st.Term || b.Term == m.st.Term && m.st.Role == "master" && b.MasterID != m.st.Self.ID {
		t := m.st.Term
		m.mu.Unlock()
		return VoteReply{Term: t}, fmt.Errorf("%w: this server is at term %d", ErrStale, t)
	}
	var master *Node
	others := []Node{}
	for i := range b.Nodes {
		n := b.Nodes[i]
		switch {
		case n.ID == m.st.Self.ID:
		case n.ID == b.MasterID:
			master = &b.Nodes[i]
		default:
			others = append(others, n)
		}
	}
	if master == nil {
		m.mu.Unlock()
		return VoteReply{}, fmt.Errorf("%w: book without its master", ErrInvalid)
	}
	wasMaster := m.st.Role == "master"
	changed := wasMaster || len(m.st.Nodes) == 0 || m.st.Nodes[0].ID != master.ID || m.st.Nodes[0].Endpoint != master.Endpoint ||
		len(m.st.Mesh) != len(others)
	m.ensureKey()
	for _, n := range b.Nodes {
		if n.ID == m.st.Self.ID && n.Endpoint != "" {
			m.st.Self.Endpoint = n.Endpoint
		}
	}
	m.st.Role, m.st.Term = "worker", b.Term
	m.st.Nodes = []Node{*master}
	m.st.Mesh = others
	if master.Endpoint != "" {
		m.st.Endpoint = master.Endpoint
	}
	m.st.Joins = nil
	m.st.Witness = b.Witness
	m.lastMaster = m.now()
	m.saveStandby(b.Files)
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return VoteReply{}, err
	}
	if changed {
		if err := m.applyWG(ctx); err != nil && m.Log != nil {
			m.Log.Warn("tunnel after new cluster book", "err", err)
		}
	}
	if wasMaster {
		if m.Log != nil {
			m.Log.Warn("another server is the cluster master now; this one is a worker", "master", master.Name, "term", b.Term)
		}
		if m.OnRoleChange != nil {
			m.OnRoleChange("worker")
		}
	}
	return VoteReply{Granted: true, Term: b.Term}, nil
}

func (m *Manager) standbyDir() string { return filepath.Join(m.Dir, "standby") }

// saveStandby keeps the master's settings for a takeover (caller holds mu).
func (m *Manager) saveStandby(files map[string][]byte) {
	dir := m.standbyDir()
	_ = os.MkdirAll(dir, 0o700)
	for name, data := range files {
		if !okBookPath(name) {
			continue
		}
		p := filepath.Join(dir, strings.ReplaceAll(name, "/", "__"))
		if os.WriteFile(p+".tmp", data, 0o600) == nil {
			_ = os.Rename(p+".tmp", p)
		}
	}
}

func okBookPath(name string) bool {
	return name != "" && !strings.Contains(name, "..") && !filepath.IsAbs(name) && len(name) < 100
}

// Peer is this server's answer to "who is the master, and do you see it?".
func (m *Manager) Peer() PeerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	p := PeerInfo{ID: m.st.Self.ID, Role: m.st.Role, Term: m.st.Term, MasterSeen: -1}
	if m.st.Role == "master" {
		p.MasterID = m.st.Self.ID
		return p
	}
	if len(m.st.Nodes) > 0 {
		p.MasterID = m.st.Nodes[0].ID
	}
	p.MasterSeen = 1 << 20
	if !m.lastMaster.IsZero() {
		p.MasterSeen = int(m.now().Sub(m.lastMaster).Seconds())
	}
	return p
}

// Vote answers a candidate: yes if its term is new, this server hasn't voted
// in it, and this server doesn't hear the master either.
func (m *Manager) Vote(r VoteRequest) VoteReply {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	rep := VoteReply{Term: m.st.Term}
	switch {
	case m.st.Role == "master":
		rep.Why = "I am the master"
	case r.Term <= m.st.Term:
		rep.Why = fmt.Sprintf("term %d is not newer than %d", r.Term, m.st.Term)
	case r.Term <= m.st.VotedTerm && m.st.VotedFor != r.Candidate:
		rep.Why = "already voted for " + m.st.VotedFor + " in this term"
	case m.st.Role == "worker" && !m.lastMaster.IsZero() && m.now().Sub(m.lastMaster) < MasterTimeout/2:
		rep.Why = "I still hear the master"
	default:
		m.st.VotedTerm, m.st.VotedFor = r.Term, r.Candidate
		_ = m.save()
		rep.Granted = true
	}
	return rep
}

// allNodes is every server of the book this worker knows (master first).
func (m *Manager) allNodes() []Node {
	l := append([]Node(nil), m.st.Nodes...)
	return append(l, m.st.Mesh...)
}

// Watch runs the election side: workers look out for a silent master; the
// master checks it still has a majority.
func (m *Manager) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.WatchOnce(ctx)
	}
}

// WatchOnce is one round of Watch.
func (m *Manager) WatchOnce(ctx context.Context) {
	m.mu.Lock()
	_ = m.load()
	role, total := m.st.Role, m.voters()
	if role == "worker" && m.lastMaster.IsZero() {
		m.lastMaster = m.now() // just started: give the master time to send the book
	}
	silent := role == "worker" && m.now().Sub(m.lastMaster) > MasterTimeout
	m.mu.Unlock()
	switch {
	case role == "master" && total >= 3 && !m.HasQuorum():
		if m.Log != nil {
			m.Log.Warn("cluster master reaches no majority of the servers; acting as a worker until it does")
		}
	case silent && total >= 3:
		m.campaign(ctx)
	}
}

// campaign: the master is silent. The lowest-numbered server that the others
// can reach asks for votes; with a majority of all servers it takes over.
func (m *Manager) campaign(ctx context.Context) {
	m.mu.Lock()
	self, term := m.st.Self, m.st.Term
	masterID := ""
	if len(m.st.Nodes) > 0 {
		masterID = m.st.Nodes[0].ID
	}
	nodes := m.allNodes()
	wit, total := m.witness(), m.voters()
	m.mu.Unlock()
	var alive []Node
	newest := term
	witAlive := 0
	if wit != nil {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		r, err := wit.Ping(cctx)
		cancel()
		if err == nil {
			if r.MasterSeen >= 0 && time.Duration(r.MasterSeen)*time.Second < MasterTimeout/2 {
				return // the witness still hears the master: only this server is cut off
			}
			if r.Term > newest {
				newest = r.Term
			}
			witAlive = 1
		}
	}
	for _, n := range nodes {
		if n.ID == masterID {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		var p PeerInfo
		err := m.call(cctx, n, "GET", "/cluster/peer", nil, &p)
		cancel()
		if err != nil {
			continue
		}
		if p.Term > newest {
			newest = p.Term
		}
		if p.Role == "master" || p.MasterSeen >= 0 && time.Duration(p.MasterSeen)*time.Second < MasterTimeout/2 {
			return // someone still hears a master (maybe a new one): wait for its book
		}
		alive = append(alive, n)
	}
	if (len(alive)+1+witAlive)*2 <= total {
		return // this side has no majority: never take over
	}
	for _, n := range alive {
		if idNum(n.ID) < idNum(self.ID) {
			return // a lower-numbered server takes over
		}
	}
	next := newest + 1
	m.mu.Lock()
	if m.st.VotedTerm >= next && m.st.VotedFor != self.ID {
		m.mu.Unlock()
		return
	}
	m.st.VotedTerm, m.st.VotedFor = next, self.ID
	_ = m.save()
	m.mu.Unlock()
	votes := 1
	for _, n := range alive {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		var r VoteReply
		err := m.call(cctx, n, "POST", "/cluster/vote", VoteRequest{Term: next, Candidate: self.ID}, &r)
		cancel()
		if err == nil && r.Granted {
			votes++
		}
	}
	if witAlive == 1 {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		r, err := wit.Vote(cctx, next, self.ID)
		cancel()
		if err == nil && r.OK {
			votes++
		} else if err == nil && m.Log != nil {
			m.Log.Warn("cluster election: the witness said no", "why", r.Why)
		}
	}
	if votes*2 <= total {
		if m.Log != nil {
			m.Log.Warn("cluster election: not enough votes", "votes", votes, "servers", total)
		}
		return
	}
	if err := m.takeOver(ctx, next, fmt.Sprintf("elected with %d of %d votes", votes, total)); err != nil && m.Log != nil {
		m.Log.Error("cluster takeover", "err", err)
	}
}

func idNum(id string) int {
	n := 0
	fmt.Sscanf(strings.TrimPrefix(id, "n"), "%d", &n)
	return n
}

// Promote makes this worker the master by hand (two-server clusters, or when
// the master is gone for good). force skips the "the master must be silent" check.
func (m *Manager) Promote(ctx context.Context, force bool) error {
	m.mu.Lock()
	_ = m.load()
	if m.st.Role != "worker" {
		m.mu.Unlock()
		return fmt.Errorf("%w: only a worker can take over", ErrRole)
	}
	if len(m.st.Mesh) == 0 && len(m.st.Nodes) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("%w: this server has no cluster book yet (it gets one from the master every few seconds)", ErrInvalid)
	}
	if !force && !m.lastMaster.IsZero() && m.now().Sub(m.lastMaster) < MasterTimeout {
		m.mu.Unlock()
		return fmt.Errorf("%w: the master answered %ds ago; it isn't gone", ErrRole, int(m.now().Sub(m.lastMaster).Seconds()))
	}
	term := max(m.st.Term, m.st.VotedTerm) + 1
	m.mu.Unlock()
	return m.takeOver(ctx, term, "taken over by hand")
}

// takeOver makes this server the master of term: the others become its
// workers, the master's settings from the book become this server's.
func (m *Manager) takeOver(ctx context.Context, term int, why string) error {
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return err
	}
	old := ""
	if len(m.st.Nodes) > 0 {
		old = m.st.Nodes[0].Name
	}
	workers := m.allNodes()
	sort.Slice(workers, func(i, j int) bool { return idNum(workers[i].ID) < idNum(workers[j].ID) })
	m.st.Role, m.st.Term, m.st.Nodes, m.st.Mesh = "master", term, workers, nil
	m.st.VotedTerm, m.st.VotedFor = term, m.st.Self.ID
	if m.st.Self.Endpoint != "" {
		m.st.Endpoint = m.st.Self.Endpoint
	} else {
		m.st.Endpoint = ""
	}
	m.quorumAt = m.now()
	m.restoreStandby()
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if m.Log != nil {
		m.Log.Warn("this server is the cluster master now", "term", term, "why", why, "old_master", old)
	}
	if err := m.applyWG(ctx); err != nil && m.Log != nil {
		m.Log.Warn("tunnel after takeover", "err", err)
	}
	m.pushBook(ctx)
	if m.OnRoleChange != nil {
		m.OnRoleChange("master")
	}
	return nil
}

// restoreStandby puts the old master's settings in place (caller holds mu).
func (m *Manager) restoreStandby() {
	ents, err := os.ReadDir(m.standbyDir())
	if err != nil || m.DataDir == "" {
		return
	}
	for _, e := range ents {
		name := strings.ReplaceAll(e.Name(), "__", "/")
		if !okBookPath(name) || strings.HasSuffix(name, ".tmp") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.standbyDir(), e.Name()))
		if err != nil {
			continue
		}
		dst := filepath.Join(m.DataDir, name)
		_ = os.MkdirAll(filepath.Dir(dst), 0o700)
		if os.WriteFile(dst+".tmp", data, 0o600) == nil {
			_ = os.Rename(dst+".tmp", dst)
		}
	}
}

// demote: this master learned that another server leads a newer term.
func (m *Manager) demote(ctx context.Context, term int, why string) {
	m.mu.Lock()
	if m.st.Role != "master" || term <= m.st.Term {
		m.mu.Unlock()
		return
	}
	m.st.Role, m.st.Term = "worker", term
	m.st.Mesh = append(m.st.Mesh, m.st.Nodes...) // until the new master's book says who is who
	m.st.Nodes = nil
	m.lastMaster = time.Time{}
	_ = m.save()
	m.mu.Unlock()
	if m.Log != nil {
		m.Log.Warn("no longer the cluster master", "why", why, "term", term)
	}
	if m.OnRoleChange != nil {
		m.OnRoleChange("worker")
	}
}

// ClusterSize is how many servers the cluster has (this one included).
func (m *Manager) ClusterSize() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	if m.st.Role == "" {
		return 1
	}
	return len(m.st.Nodes) + len(m.st.Mesh) + 1
}

// ReachesMajority says whether this server reaches more than half of the
// cluster (itself included).
func (m *Manager) ReachesMajority(ctx context.Context) bool {
	m.mu.Lock()
	_ = m.load()
	nodes := m.allNodes()
	wit, total := m.witness(), m.voters()
	m.mu.Unlock()
	reached := 1
	if wit != nil {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		if _, err := wit.Ping(cctx); err == nil {
			reached++
		}
		cancel()
	}
	for _, n := range nodes {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		var p PeerInfo
		if m.call(cctx, n, "GET", "/cluster/peer", nil, &p) == nil {
			reached++
		}
		cancel()
	}
	return reached*2 > total
}
