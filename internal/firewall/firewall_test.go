package firewall

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	fw, err := Validate(Settings{Enabled: true, Rules: []Rule{
		{Proto: "TCP", Ports: " 22 "}, {Proto: "tcp", Ports: "80, 443,8000-8100"},
		{Proto: "udp", Ports: "53", Source: "192.168.1.7"}, {Proto: "icmp", Ports: "99", Source: "10.0.0.0/8"},
		{Proto: "tcp", Ports: "5432", Source: "2001:db8::/32", Comment: "db (office)"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := fw.Rules
	if r[0].Proto != "tcp" || r[0].Ports != "22" || r[1].Ports != "80,443,8000-8100" || r[2].Source != "192.168.1.7/32" || r[3].Ports != "" {
		t.Fatalf("normalized: %+v", r)
	}
	for _, bad := range []Rule{
		{Proto: "gre"}, {Proto: "tcp"}, {Proto: "tcp", Ports: "0"}, {Proto: "tcp", Ports: "70000"},
		{Proto: "tcp", Ports: "90-80"}, {Proto: "tcp", Ports: "22;drop"}, {Proto: "udp", Ports: "1", Source: "nope"},
		{Proto: "tcp", Ports: "22", Comment: `x" accept`}, {Proto: "tcp", Ports: "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16"},
	} {
		if _, err := Validate(Settings{Rules: []Rule{bad}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %+v", bad)
		}
	}
	many := make([]Rule, MaxRules+1)
	for i := range many {
		many[i] = Rule{Proto: "icmp"}
	}
	if _, err := Validate(Settings{Rules: many}); err == nil {
		t.Error("too many rules accepted")
	}
}

func TestRules(t *testing.T) {
	s, err := Rules([]VM{
		{Name: "web1", MAC: "52:54:00:AA:BB:CC", FW: Settings{Enabled: true, Rules: []Rule{
			{Proto: "tcp", Ports: "22"}, {Proto: "tcp", Ports: "80,443"}, {Proto: "icmp"},
			{Proto: "udp", Ports: "51820", Source: "203.0.113.0/24"}}}},
		{Name: "open", MAC: "52:54:00:00:00:01", FW: Settings{Enabled: false, Rules: []Rule{{Proto: "tcp", Ports: "1"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"delete table bridge p8s_fw",
		"ct state established,related accept",
		"meta l4proto tcp th dport 22 accept",
		"meta l4proto tcp th dport { 80, 443 } accept",
		"icmp type echo-request accept",
		"ip saddr 203.0.113.0/24 meta l4proto udp th dport 51820 accept",
		`counter drop comment "firewall of web1"`,
		"type filter hook forward", "type filter hook output",
		"ether daddr 52:54:00:aa:bb:cc jump vm1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "52:54:00:00:00:01") {
		t.Error("VM with the firewall off must not be filtered")
	}
	if _, err := Rules([]VM{{Name: "x", MAC: "evil; flush ruleset", FW: Settings{Enabled: true}}}); err == nil {
		t.Error("bad MAC accepted")
	}
	// nothing enabled: still a valid (empty) table, so turning the last one off clears the rules
	s, _ = Rules(nil)
	if !strings.Contains(s, "hook forward") || strings.Contains(s, "jump") {
		t.Errorf("empty table: %s", s)
	}
}
