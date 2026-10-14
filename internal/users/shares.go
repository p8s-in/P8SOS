package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Per-VM permissions (like Proxmox's ACL on /vms/ID): an owner or admin
// shares one VM with another user, as a viewer (looks only) or a user (runs
// it: start/stop, console, snapshots, backups — not delete, resize or
// hardware, which stay with the owner and count against the owner's plan).
const (
	ShareViewer = "viewer"
	ShareUser   = "user"
)

// Shares keeps who may use which VM besides its owner.
type Shares struct {
	Path string // JSON file; "" = in memory (tests)
	mu   sync.Mutex
	m    map[string]map[string]string // vm -> user -> level
}

func (s *Shares) load() error {
	if s.m != nil {
		return nil
	}
	s.m = map[string]map[string]string{}
	if s.Path == "" {
		return nil
	}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.m)
}

func (s *Shares) save() error {
	if s.Path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Level is user's access to vm: "", "viewer" or "user".
func (s *Shares) Level(vm, user string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return ""
	}
	return s.m[vm][user]
}

// Share is one entry.
type Share struct {
	User  string `json:"user"`
	Level string `json:"level"`
}

// List is who vm is shared with.
func (s *Shares) List(vm string) []Share {
	out := []Share{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return out
	}
	for u, l := range s.m[vm] {
		out = append(out, Share{u, l})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

// Set gives user level on vm ("" takes it away).
func (s *Shares) Set(vm, user, level string) error {
	switch level {
	case "", ShareViewer, ShareUser:
	default:
		return fmt.Errorf("%w: level must be viewer, user or empty", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	if level == "" {
		delete(s.m[vm], user)
		if len(s.m[vm]) == 0 {
			delete(s.m, vm)
		}
	} else {
		if s.m[vm] == nil {
			s.m[vm] = map[string]string{}
		}
		if len(s.m[vm]) >= 50 && s.m[vm][user] == "" {
			return fmt.Errorf("%w: a VM can be shared with at most 50 users", ErrInvalid)
		}
		s.m[vm][user] = level
	}
	return s.save()
}

// DropVM forgets a deleted VM's shares.
func (s *Shares) DropVM(vm string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() == nil && s.m[vm] != nil {
		delete(s.m, vm)
		_ = s.save()
	}
}
