package users

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sessions are dashboard sign-ins. Only the SHA-256 of each token is kept,
// in memory and (with Path) in a 0600 file, so a p8sd restart or update
// doesn't sign everyone out.
type Sessions struct {
	Idle time.Duration // signed out after this long unused; 0 = 12h
	Max  time.Duration // and after this long in any case; 0 = 7 days
	Now  func() time.Time
	Path string // e.g. /var/lib/p8s/sessions.json; empty = memory only

	mu     sync.Mutex
	m      map[string]*session
	loaded bool
	dirty  bool // last-use times changed since the last save
}

type savedSession struct {
	Key     string    `json:"key"` // sha256 of the token
	User    string    `json:"user"`
	Created time.Time `json:"created"`
	Last    time.Time `json:"last"`
}

// load reads the saved sessions once (caller holds mu).
func (s *Sessions) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	if s.m == nil {
		s.m = map[string]*session{}
	}
	if s.Path == "" {
		return
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return
	}
	var list []savedSession
	if json.Unmarshal(b, &list) != nil {
		return
	}
	for _, v := range list {
		if len(v.Key) == 64 && v.User != "" {
			s.m[v.Key] = &session{user: v.User, created: v.Created, last: v.Last}
		}
	}
}

// save writes the sessions (caller holds mu). Failure only means sign-ins
// don't survive a restart, so it is not reported.
func (s *Sessions) save() {
	s.dirty = false
	if s.Path == "" {
		return
	}
	list := make([]savedSession, 0, len(s.m))
	for k, v := range s.m {
		list = append(list, savedSession{k, v.user, v.created, v.last})
	}
	b, _ := json.Marshal(list)
	_ = os.MkdirAll(filepath.Dir(s.Path), 0o750)
	tmp := s.Path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.Path)
	}
}

// Flush saves last-use times if they changed (p8sd calls it every minute).
func (s *Sessions) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.save()
	}
}

type session struct {
	user          string
	created, last time.Time
}

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sessions) limits() (idle, max time.Duration) {
	idle, max = s.Idle, s.Max
	if idle == 0 {
		idle = 12 * time.Hour
	}
	if max == 0 {
		max = 7 * 24 * time.Hour
	}
	return
}

func key(tok string) string { h := sha256.Sum256([]byte(tok)); return hex.EncodeToString(h[:]) }

// New starts a session for user and returns its token.
func (s *Sessions) New(user string) (string, time.Duration) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := "p8s_" + hex.EncodeToString(b)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	idle, max := s.limits()
	for k, v := range s.m {
		if now.Sub(v.last) > idle || now.Sub(v.created) > max {
			delete(s.m, k)
		}
	}
	s.m[key(tok)] = &session{user: user, created: now, last: now}
	s.save()
	return tok, idle
}

// User returns who a token belongs to and keeps the session alive.
func (s *Sessions) User(tok string) (string, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	k := key(tok)
	v, ok := s.m[k]
	if !ok {
		return "", false
	}
	idle, max := s.limits()
	if now.Sub(v.last) > idle || now.Sub(v.created) > max {
		delete(s.m, k)
		s.dirty = true
		return "", false
	}
	v.last = now
	s.dirty = true
	return v.user, true
}

// End signs one token out.
func (s *Sessions) End(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	delete(s.m, key(tok))
	s.save()
}

// EndUser signs a user out everywhere (password change, disabled, deleted).
// keep is a token to leave signed in (the caller's own), or "".
func (s *Sessions) EndUser(user, keep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kk := ""
	if keep != "" {
		kk = key(keep)
	}
	s.load()
	for k, v := range s.m {
		if v.user == user && k != kk {
			delete(s.m, k)
		}
	}
	s.save()
}

// Limiter slows down password guessing: after Max failures for a user name
// ("u:" keys), or IPMax for an address ("ip:" keys; offices share one), within
// Window, that key is blocked for Window.
type Limiter struct {
	Max    int           // 0 = 5
	IPMax  int           // 0 = 4 x Max
	Window time.Duration // 0 = 15 min
	Now    func() time.Time

	mu sync.Mutex
	m  map[string]*fails
}

type fails struct {
	n     int
	first time.Time
}

func (l *Limiter) cfg() (int, time.Duration) {
	n, w := l.Max, l.Window
	if n == 0 {
		n = 5
	}
	if w == 0 {
		w = 15 * time.Minute
	}
	return n, w
}

func (l *Limiter) maxFor(key string) int {
	n, _ := l.cfg()
	if strings.HasPrefix(key, "ip:") {
		if l.IPMax > 0 {
			return l.IPMax
		}
		return 4 * n
	}
	return n
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Blocked reports whether any of the keys is currently locked out, and for how long.
func (l *Limiter) Blocked(keys ...string) (time.Duration, bool) {
	_, w := l.cfg()
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait time.Duration
	for _, k := range keys {
		f, ok := l.m[k]
		if !ok {
			continue
		}
		if now.Sub(f.first) > w {
			delete(l.m, k)
			continue
		}
		if f.n >= l.maxFor(k) {
			wait = max(wait, w-now.Sub(f.first))
		}
	}
	return wait, wait > 0
}

// Fail records a failed attempt for each key.
func (l *Limiter) Fail(keys ...string) {
	_, w := l.cfg()
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*fails{}
	}
	for _, k := range keys {
		f, ok := l.m[k]
		if !ok || now.Sub(f.first) > w {
			f = &fails{first: now}
			l.m[k] = f
		}
		f.n++
	}
}

// Reset forgets failures for the keys (after a successful sign-in).
func (l *Limiter) Reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.m, k)
	}
}
