// Package witness is the tie-breaker for clusters of two servers (like
// Proxmox's QDevice): a tiny service on any always-on Linux box (a Raspberry
// Pi, a NAS, a small VPS) that has one vote. With it, two servers and the
// witness are three voters, so when one server dies the other can take over
// by itself — and a server cut off from both never does.
//
// The master tells the witness it is alive (heartbeat) every poll. A server
// asking for its vote gets it only for a newer term and only when the
// witness hasn't heard the master recently — the same rule the servers use.
package witness

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// MasterTimeout matches the cluster's: a master not heard from this long may be replaced.
const MasterTimeout = 45 * time.Second

// Cluster is what the witness remembers about one cluster.
type Cluster struct {
	Term       int    `json:"term"`        // newest term seen
	VotedTerm  int    `json:"voted_term"`  // last term it voted in
	VotedFor   string `json:"voted_for"`   // and for whom
	Master     string `json:"master"`      // node id of the master that sent the last heartbeat
	MasterSeen string `json:"master_seen"` // RFC 3339
}

// Heartbeat is the master's "I am alive".
type Heartbeat struct {
	Cluster string `json:"cluster"`
	Term    int    `json:"term"`
	Master  string `json:"master"`
}

// VoteRequest asks for the witness's vote.
type VoteRequest struct {
	Cluster   string `json:"cluster"`
	Term      int    `json:"term"`
	Candidate string `json:"candidate"`
}

// Reply answers both.
type Reply struct {
	OK         bool   `json:"ok"`
	Term       int    `json:"term"`
	Why        string `json:"why,omitempty"`
	MasterSeen int    `json:"master_seen_s"` // seconds since the master's last heartbeat (-1 = never)
}

// Server is the witness service.
type Server struct {
	File  string // state (JSON)
	Token string // the shared secret the cluster calls with
	Now   func() time.Time

	mu sync.Mutex
	m  map[string]*Cluster
}

var reID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) load() {
	if s.m != nil {
		return
	}
	s.m = map[string]*Cluster{}
	if b, err := os.ReadFile(s.File); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
}

func (s *Server) save() error {
	b, _ := json.MarshalIndent(s.m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.File), 0o700); err != nil {
		return err
	}
	tmp := s.File + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.File)
}

func (s *Server) seen(c *Cluster) int {
	t, err := time.Parse(time.RFC3339, c.MasterSeen)
	if err != nil {
		return -1
	}
	return int(s.now().Sub(t).Seconds())
}

// Beat records the master's heartbeat. A heartbeat of an older term is
// refused (that master was replaced; the reply tells it the newer term).
func (s *Server) Beat(h Heartbeat) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	c := s.m[h.Cluster]
	if c == nil {
		c = &Cluster{}
		s.m[h.Cluster] = c
	}
	if h.Term < c.Term {
		return Reply{Term: c.Term, Why: fmt.Sprintf("term %d is older than %d", h.Term, c.Term), MasterSeen: s.seen(c)}
	}
	c.Term, c.Master, c.MasterSeen = h.Term, h.Master, s.now().UTC().Format(time.RFC3339)
	_ = s.save()
	return Reply{OK: true, Term: c.Term, MasterSeen: 0}
}

// Vote grants its vote for a newer term when the master has gone quiet.
func (s *Server) Vote(v VoteRequest) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	c := s.m[v.Cluster]
	if c == nil {
		c = &Cluster{}
		s.m[v.Cluster] = c
	}
	r := Reply{Term: c.Term, MasterSeen: s.seen(c)}
	switch {
	case v.Term <= c.Term:
		r.Why = fmt.Sprintf("term %d is not newer than %d", v.Term, c.Term)
	case v.Term <= c.VotedTerm && c.VotedFor != v.Candidate:
		r.Why = "already voted for " + c.VotedFor + " in this term"
	case r.MasterSeen >= 0 && time.Duration(r.MasterSeen)*time.Second < MasterTimeout/2 && c.Master != v.Candidate:
		r.Why = "the master is still alive (it sent a heartbeat " + fmt.Sprint(r.MasterSeen) + " s ago)"
	default:
		c.VotedTerm, c.VotedFor = v.Term, v.Candidate
		_ = s.save()
		r.OK = true
	}
	return r
}

// Status is a cluster's view (for ping).
func (s *Server) Status(cluster string) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	c := s.m[cluster]
	if c == nil {
		return Reply{OK: true, MasterSeen: -1}
	}
	return Reply{OK: true, Term: c.Term, MasterSeen: s.seen(c)}
}

// Handler serves /witness/{ping,heartbeat,vote}.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(f func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("Authorization")
			want := "Bearer " + s.Token
			if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				http.Error(w, `{"ok":false,"why":"wrong token"}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			f(w, r)
		}
	}
	read := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(v); err != nil {
			http.Error(w, `{"ok":false,"why":"bad request"}`, http.StatusBadRequest)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /witness/ping", auth(func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cluster")
		if c != "" && !reID.MatchString(c) {
			http.Error(w, `{"ok":false}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(s.Status(c))
	}))
	mux.HandleFunc("POST /witness/heartbeat", auth(func(w http.ResponseWriter, r *http.Request) {
		var h Heartbeat
		if !read(w, r, &h) || !reID.MatchString(h.Cluster) || !reID.MatchString(h.Master) {
			return
		}
		_ = json.NewEncoder(w).Encode(s.Beat(h))
	}))
	mux.HandleFunc("POST /witness/vote", auth(func(w http.ResponseWriter, r *http.Request) {
		var v VoteRequest
		if !read(w, r, &v) || !reID.MatchString(v.Cluster) || !reID.MatchString(v.Candidate) {
			return
		}
		_ = json.NewEncoder(w).Encode(s.Vote(v))
	}))
	return mux
}

// ErrUnreachable means the witness didn't answer.
var ErrUnreachable = errors.New("the witness didn't answer")
