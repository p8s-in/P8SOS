package firewall

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// The datacenter firewall (Proxmox's cluster-wide firewall + IP sets): set
// once on the master, it applies on every server of the cluster.
//
//   - IP sets: named lists of addresses and networks ("office", "monitoring"),
//     usable as a rule's source "+office" in any VM's firewall.
//   - Block: addresses or sets no VM may talk to or hear from (an attacker,
//     a country's ranges you don't serve).
//   - Allow: rules every VM with its firewall on gets before its own
//     (monitoring, backups from the office).

// Global is the datacenter firewall.
type Global struct {
	Sets  map[string][]string `json:"sets"`
	Block []string            `json:"block"` // CIDRs / addresses / +set
	Allow []Rule              `json:"allow"`
}

var reSet = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}$`)

// MaxSetSize keeps sets in a size nftables handles well with intervals.
const MaxSetSize = 4096

// ValidateGlobal checks and tidies the datacenter firewall.
func ValidateGlobal(g Global) (Global, error) {
	out := Global{Sets: map[string][]string{}, Block: []string{}, Allow: []Rule{}}
	if len(g.Sets) > 64 {
		return out, fmt.Errorf("%w: at most 64 IP sets", ErrInvalid)
	}
	for name, l := range g.Sets {
		if !reSet.MatchString(name) {
			return out, fmt.Errorf("%w: IP set name %q: lowercase letters, digits, _ and -, starting with a letter", ErrInvalid, name)
		}
		if len(l) > MaxSetSize {
			return out, fmt.Errorf("%w: IP set %s: at most %d entries", ErrInvalid, name, MaxSetSize)
		}
		seen := map[string]bool{}
		var clean []string
		for _, a := range l {
			if strings.TrimSpace(a) == "" {
				continue
			}
			n, err := normSource(a)
			if err != nil || n == "" {
				return out, fmt.Errorf("%w: IP set %s: %q isn't an address or network", ErrInvalid, name, a)
			}
			if !seen[n] {
				seen[n] = true
				clean = append(clean, n)
			}
		}
		sort.Strings(clean)
		out.Sets[name] = clean
	}
	src := func(s string) (string, error) {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "+") {
			if _, ok := out.Sets[s[1:]]; !ok {
				return "", fmt.Errorf("there is no IP set %q", s[1:])
			}
			return s, nil
		}
		n, err := normSource(s)
		if err == nil && n == "" {
			return "", errors.New("blocking everything would cut off every VM")
		}
		return n, err
	}
	if len(g.Block) > 256 {
		return out, fmt.Errorf("%w: at most 256 blocked entries (use an IP set for more)", ErrInvalid)
	}
	for _, b := range g.Block {
		if strings.TrimSpace(b) == "" {
			continue
		}
		n, err := src(b)
		if err != nil {
			return out, fmt.Errorf("%w: block %q: %v", ErrInvalid, b, err)
		}
		out.Block = append(out.Block, n)
	}
	allow, err := validateRules(g.Allow, out.Sets)
	if err != nil {
		return out, err
	}
	out.Allow = allow
	return out, nil
}

// validateRules is Validate's rule part with IP sets allowed as sources.
func validateRules(rules []Rule, sets map[string][]string) ([]Rule, error) {
	plain := make([]Rule, len(rules))
	copy(plain, rules)
	setOf := map[int]string{}
	for i, r := range plain {
		if s := strings.TrimSpace(r.Source); strings.HasPrefix(s, "+") {
			if sets != nil {
				if _, ok := sets[s[1:]]; !ok {
					return nil, fmt.Errorf("%w: rule %d: there is no IP set %q", ErrInvalid, i+1, s[1:])
				}
			}
			if !reSet.MatchString(s[1:]) {
				return nil, fmt.Errorf("%w: rule %d: %q isn't an IP set name", ErrInvalid, i+1, s)
			}
			setOf[i], plain[i].Source = s, ""
		}
	}
	v, err := Validate(Settings{Rules: plain})
	if err != nil {
		return nil, err
	}
	for i, s := range setOf {
		v.Rules[i].Source = s
	}
	return v.Rules, nil
}

// setName is the nftables set for an IP set (one per address family).
func setName(name string, v6 bool) string {
	if v6 {
		return "ipset_" + name + "_6"
	}
	return "ipset_" + name + "_4"
}

func renderSets(b *strings.Builder, sets map[string][]string) {
	names := make([]string, 0, len(sets))
	for n := range sets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		var v4, v6 []string
		for _, a := range sets[n] {
			if strings.Contains(a, ":") {
				v6 = append(v6, a)
			} else {
				v4 = append(v4, a)
			}
		}
		for _, f := range []struct {
			v6   bool
			typ  string
			list []string
		}{{false, "ipv4_addr", v4}, {true, "ipv6_addr", v6}} {
			fmt.Fprintf(b, "\tset %s {\n\t\ttype %s\n\t\tflags interval\n", setName(n, f.v6), f.typ)
			if len(f.list) > 0 {
				fmt.Fprintf(b, "\t\tauto-merge\n\t\telements = { %s }\n", strings.Join(f.list, ", "))
			}
			b.WriteString("\t}\n")
		}
	}
}

// matches is the nftables match(es) for a source ("" = anywhere): one line
// per address family for an IP set.
func matches(src string) []string {
	switch {
	case src == "":
		return []string{""}
	case strings.HasPrefix(src, "+"):
		return []string{"ip saddr @" + setName(src[1:], false) + " ", "ip6 saddr @" + setName(src[1:], true) + " "}
	case strings.Contains(src, ":"):
		return []string{"ip6 saddr " + src + " "}
	}
	return []string{"ip saddr " + src + " "}
}

// GlobalStore keeps the datacenter firewall in a JSON file.
type GlobalStore struct {
	Path string
	mu   sync.Mutex
}

// Load reads it (empty when there is none).
func (s *GlobalStore) Load() Global {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := Global{Sets: map[string][]string{}, Block: []string{}, Allow: []Rule{}}
	if s == nil || s.Path == "" {
		return g
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return g
	}
	if json.Unmarshal(b, &g) != nil {
		return Global{Sets: map[string][]string{}, Block: []string{}, Allow: []Rule{}}
	}
	if v, err := ValidateGlobal(g); err == nil {
		return v
	}
	return Global{Sets: map[string][]string{}, Block: []string{}, Allow: []Rule{}}
}

// Save validates and writes it.
func (s *GlobalStore) Save(g Global) (Global, error) {
	v, err := ValidateGlobal(g)
	if err != nil {
		return v, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return v, err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return v, err
	}
	return v, os.Rename(tmp, s.Path)
}
