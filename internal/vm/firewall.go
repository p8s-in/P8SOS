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
	"p8sos/internal/virt"
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
	return s.SetGuestFirewall(ctx, name, fw)
}

// GuestFirewall returns the firewall saved under key (a VM name, or
// "ct:NAME" for a container). The caller checks that the guest exists.
func (s *Service) GuestFirewall(key string) (firewall.Settings, error) {
	fwMu.Lock()
	defer fwMu.Unlock()
	m, err := s.loadFirewalls()
	if err != nil {
		return firewall.Settings{}, err
	}
	fw := m[key]
	if fw.Rules == nil {
		fw.Rules = []firewall.Rule{}
	}
	return fw, nil
}

// SetGuestFirewall saves and applies the firewall under key. If the rules
// can't be applied, nothing changes (no firewall that only looks on).
func (s *Service) SetGuestFirewall(ctx context.Context, key string, fw firewall.Settings) (firewall.Settings, error) {
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
	old, had := m[key]
	m[key] = fw
	if err := s.applyFirewalls(ctx, m); err != nil {
		if had {
			m[key] = old
		} else {
			delete(m, key)
		}
		return firewall.Settings{}, fmt.Errorf("the firewall could not be switched on, nothing was changed: %w", err)
	}
	return fw, s.saveFirewalls(m)
}

// DropGuestFirewall forgets a deleted guest's firewall.
func (s *Service) DropGuestFirewall(ctx context.Context, key string) error {
	return s.dropFirewall(ctx, key)
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
		e := firewall.VM{Name: v.Name, MAC: v.MAC, FW: m[v.Name]}
		for _, n := range v.NICs { // every card of the VM is behind its firewall
			e.MACs = append(e.MACs, n.MAC)
		}
		if v.Public != nil && v.State == virt.Running {
			// Pin the public address to this VM's first card's port (ports change
			// on every start; the first card is listed first).
			if ports, err := s.HV.Ports(ctx, Prefix+v.Name); err == nil && len(ports) > 0 {
				e.LockIP, e.Ports = v.Public.IP, ports[:1]
				e.LockMore = v.Public.Extra
				if v.Public.IP6 != "" {
					e.LockIP6 = []string{v.Public.IP6}
				}
			}
		}
		if e.FW.Enabled || e.LockIP != "" {
			list = append(list, e)
		}
	}
	if s.OtherGuests != nil {
		others, err := s.OtherGuests(ctx)
		if err != nil {
			return err
		}
		for _, e := range others {
			e.FW = m[e.Name]
			if e.FW.Enabled || e.LockIP != "" {
				list = append(list, e)
			}
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
