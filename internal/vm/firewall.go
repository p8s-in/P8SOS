package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"p8sos/internal/firewall"
)

// Firewall settings live in one JSON file (VM name -> settings), like backup
// schedules; the rules themselves are an nftables table rebuilt from it.

var fwMu sync.Mutex

func (s *Service) loadFirewalls() (map[string]firewall.Settings, error) {
	m := map[string]firewall.Settings{}
	if s.FirewallFile == "" {
		s.memFW.Range(func(k, v any) bool { m[k.(string)] = v.(firewall.Settings); return true })
		return m, nil
	}
	b, err := os.ReadFile(s.FirewallFile)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

func (s *Service) saveFirewalls(m map[string]firewall.Settings) error {
	if s.FirewallFile == "" {
		s.memFW.Range(func(k, _ any) bool { s.memFW.Delete(k); return true })
		for k, v := range m {
			s.memFW.Store(k, v)
		}
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.FirewallFile), 0o750); err != nil {
		return err
	}
	tmp := s.FirewallFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.FirewallFile)
}

// GetFirewall returns a VM's firewall (off with no rules when never set).
func (s *Service) GetFirewall(ctx context.Context, name string) (firewall.Settings, error) {
	if _, err := s.state(ctx, Prefix+name); err != nil {
		return firewall.Settings{}, err
	}
	fwMu.Lock()
	defer fwMu.Unlock()
	m, err := s.loadFirewalls()
	if err != nil {
		return firewall.Settings{}, err
	}
	fw := m[name]
	if fw.Rules == nil {
		fw.Rules = []firewall.Rule{}
	}
	return fw, nil
}

// SetFirewall saves a VM's firewall and applies it at once. If the rules
// can't be applied, nothing changes (no firewall that only looks on).
func (s *Service) SetFirewall(ctx context.Context, name string, fw firewall.Settings) (firewall.Settings, error) {
	if _, err := s.state(ctx, Prefix+name); err != nil {
		return firewall.Settings{}, err
	}
	fw, err := firewall.Validate(fw)
	if err != nil {
		return firewall.Settings{}, fmt.Errorf("%w: %v", ErrInvalid, errors.Unwrap(err))
	}
	fwMu.Lock()
	defer fwMu.Unlock()
	m, err := s.loadFirewalls()
	if err != nil {
		return firewall.Settings{}, err
	}
	old, had := m[name]
	m[name] = fw
	if err := s.applyFirewalls(ctx, m); err != nil {
		if had {
			m[name] = old
		} else {
			delete(m, name)
		}
		return firewall.Settings{}, fmt.Errorf("the firewall could not be switched on, nothing was changed: %w", err)
	}
	return fw, s.saveFirewalls(m)
}

// ApplyFirewalls rebuilds the nftables table from the saved settings (p8sd
// start, every few minutes, after a VM is deleted or restored).
func (s *Service) ApplyFirewalls(ctx context.Context) error {
	fwMu.Lock()
	defer fwMu.Unlock()
	m, err := s.loadFirewalls()
	if err != nil {
		return err
	}
	return s.applyFirewalls(ctx, m)
}

func (s *Service) applyFirewalls(ctx context.Context, m map[string]firewall.Settings) error {
	if s.Firewall == nil {
		return nil
	}
	vms, err := s.List(ctx)
	if err != nil {
		return err
	}
	list := []firewall.VM{}
	for _, v := range vms {
		if fw, ok := m[v.Name]; ok && fw.Enabled {
			list = append(list, firewall.VM{Name: v.Name, MAC: v.MAC, FW: fw})
		}
	}
	return s.Firewall(ctx, list)
}

// dropFirewall forgets a deleted VM's firewall.
func (s *Service) dropFirewall(ctx context.Context, name string) error {
	fwMu.Lock()
	defer fwMu.Unlock()
	m, err := s.loadFirewalls()
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return nil
	}
	delete(m, name)
	if err := s.saveFirewalls(m); err != nil {
		return err
	}
	return s.applyFirewalls(ctx, m)
}
