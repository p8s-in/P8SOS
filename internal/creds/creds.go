// Package creds keeps the login details P8S hands out for VMs and containers
// (the root password made at create time, like a VPS panel shows it), so the
// owner can look them up later in the dashboard or over the API.
//
// The file lives in p8sd's data directory (root only, 0600). A password stays
// here until it is changed through P8S or the guest is deleted; a password
// changed inside the guest itself isn't known to P8S (the panel says so).
package creds

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Login is one guest's login.
type Login struct {
	User     string `json:"user"`
	Password string `json:"password"`
	SetAt    string `json:"set_at"`
	// SSH: root may log in over SSH with the password.
	SSH bool `json:"ssh_password"`
}

type Store struct {
	File string
	mu   sync.Mutex
}

func key(kind, name string) string { return kind + ":" + name } // "vm:web1", "ct:db"

func (s *Store) load() map[string]Login {
	m := map[string]Login{}
	if b, err := os.ReadFile(s.File); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (s *Store) save(m map[string]Login) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.File), 0o700); err != nil {
		return err
	}
	tmp := s.File + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.File)
}

// Get returns a guest's login (ok=false: none known).
func (s *Store) Get(kind, name string) (Login, bool) {
	if s == nil {
		return Login{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.load()[key(kind, name)]
	return l, ok
}

// Set remembers a login.
func (s *Store) Set(kind, name string, l Login) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.SetAt == "" {
		l.SetAt = time.Now().UTC().Format(time.RFC3339)
	}
	m := s.load()
	m[key(kind, name)] = l
	return s.save(m)
}

// Delete forgets a guest (it was deleted).
func (s *Store) Delete(kind, name string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	if _, ok := m[key(kind, name)]; !ok {
		return nil
	}
	delete(m, key(kind, name))
	return s.save(m)
}

// Rename moves a login (guest renamed or restored under another name).
func (s *Store) Rename(kind, from, to string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	l, ok := m[key(kind, from)]
	if !ok {
		return nil
	}
	delete(m, key(kind, from))
	m[key(kind, to)] = l
	return s.save(m)
}

// alphabet without look-alikes (0/O, 1/l/I) so it can be typed from the screen
const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// Generate makes a random password of n characters (at least one lower case
// letter, one upper case letter and one digit; ~5.8 bits per character).
func Generate(n int) string {
	n = max(n, 12)
	for {
		b := make([]byte, n)
		for i := range b {
			k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				panic(err)
			}
			b[i] = alphabet[k.Int64()]
		}
		var lo, up, dig bool
		for _, c := range b {
			switch {
			case c >= 'a' && c <= 'z':
				lo = true
			case c >= 'A' && c <= 'Z':
				up = true
			default:
				dig = true
			}
		}
		if lo && up && dig {
			return string(b)
		}
	}
}
