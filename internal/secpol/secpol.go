// Package secpol keeps the server's security settings (Settings → Security):
// which networks may open the dashboard and API, the password policy, and
// whether staff must use two-factor sign-in.
package secpol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"p8sos/internal/users"
)

var ErrInvalid = errors.New("invalid")

// Settings is data_dir/security.json.
type Settings struct {
	// AllowNets: addresses or networks (CIDR) that may reach the dashboard and
	// API. Empty = everyone. This server itself and the cluster tunnel always may.
	AllowNets []string     `json:"allow_nets"`
	Staff2FA  bool         `json:"staff_2fa"` // admins, operators and auditors must turn on 2FA
	Password  users.Policy `json:"password"`
}

// Store loads and saves the settings.
type Store struct {
	Dir string

	mu   sync.RWMutex
	set  *Settings
	nets []*net.IPNet
}

func (s *Store) file() string { return filepath.Join(s.Dir, "security.json") }

// Load reads the settings (and applies the password policy).
func (s *Store) Load() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() Settings {
	if s.set == nil {
		st := &Settings{}
		if b, err := os.ReadFile(s.file()); err == nil {
			_ = json.Unmarshal(b, st)
		}
		if st.Password.MinLength == 0 {
			st.Password.MinLength = 10
		}
		s.set, s.nets = st, parseNets(st.AllowNets)
		users.SetPolicy(st.Password)
	}
	return *s.set
}

func parseNets(l []string) []*net.IPNet {
	var out []*net.IPNet
	for _, a := range l {
		if n, err := toNet(a); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func toNet(a string) (*net.IPNet, error) {
	a = strings.TrimSpace(a)
	if !strings.Contains(a, "/") {
		ip := net.ParseIP(a)
		if ip == nil {
			return nil, fmt.Errorf("%w: %q is not an address or network", ErrInvalid, a)
		}
		if ip.To4() != nil {
			a += "/32"
		} else {
			a += "/128"
		}
	}
	_, n, err := net.ParseCIDR(a)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not an address or network", ErrInvalid, a)
	}
	return n, nil
}

// Save validates and stores. from is the address of whoever saves: a list
// that would lock them out is refused.
func (s *Store) Save(st Settings, from string) (Settings, error) {
	clean := []string{}
	for _, a := range st.AllowNets {
		if strings.TrimSpace(a) == "" {
			continue
		}
		n, err := toNet(a)
		if err != nil {
			return Settings{}, err
		}
		clean = append(clean, n.String())
	}
	st.AllowNets = clean
	if len(clean) > 0 && !allowed(parseNets(clean), from) {
		return Settings{}, fmt.Errorf("%w: your own address (%s) isn't in the list; you would lock yourself out", ErrInvalid, from)
	}
	if st.Password.MinLength < 10 || st.Password.MinLength > 64 {
		return Settings{}, fmt.Errorf("%w: minimum password length is 10 to 64", ErrInvalid)
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Settings{}, err
	}
	if err := os.WriteFile(s.file()+".tmp", b, 0o600); err != nil {
		return Settings{}, err
	}
	if err := os.Rename(s.file()+".tmp", s.file()); err != nil {
		return Settings{}, err
	}
	s.mu.Lock()
	s.set, s.nets = &st, parseNets(clean)
	s.mu.Unlock()
	users.SetPolicy(st.Password)
	return st, nil
}

// cluster tunnel addresses (servers of the same cluster)
var tunnel = &net.IPNet{IP: net.IPv4(10, 250, 0, 0), Mask: net.CIDRMask(24, 32)}

func allowed(nets []*net.IPNet, addr string) bool {
	if len(nets) == 0 {
		return true
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || tunnel.Contains(ip) {
		return true
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Allowed says whether addr may use the dashboard and API.
func (s *Store) Allowed(addr string) bool {
	s.mu.Lock()
	s.loadLocked()
	nets := s.nets
	s.mu.Unlock()
	return allowed(nets, addr)
}
