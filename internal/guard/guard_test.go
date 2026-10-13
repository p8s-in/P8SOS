package guard

import (
	"context"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestRules(t *testing.T) {
	inet, bridge, err := Rules(Config{NATBridge: "virbr0", Allow: []string{"192.168.0.50", "10.9.0.0/16"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"delete table inet p8s_guard",
		`iifname "virbr0" udp dport { 53, 67, 547 } accept`,
		`iifname "virbr0" counter drop`,
		`iifname "virbr0" ip daddr { 192.168.0.50, 10.9.0.0/16 } accept`,
		`iifname "virbr0" ip daddr @private4 counter drop`,
	} {
		if !strings.Contains(inet, want) {
			t.Errorf("missing %q in\n%s", want, inet)
		}
	}
	// allow-list must come before the drop
	if strings.Index(inet, "10.9.0.0/16 } accept") > strings.Index(inet, "@private4 counter drop") {
		t.Error("allow rule after drop")
	}
	if bridge != "" {
		t.Fatal("no LAN bridge -> no bridge rules")
	}
	_, bridge, _ = Rules(Config{NATBridge: "virbr0", LANBridge: "br0"})
	if !strings.Contains(bridge, `meta ibrname "br0" iifname "vnet*" counter drop`) {
		t.Fatalf("bridge rules:\n%s", bridge)
	}
	for _, bad := range []Config{{NATBridge: "vir br0"}, {NATBridge: "virbr0", LANBridge: "x;y"}, {NATBridge: "virbr0", Allow: []string{"0.0.0.0/0 } accept; drop {"}}} {
		if _, _, err := Rules(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestApply(t *testing.T) {
	r := &testfake.Runner{}
	if err := Apply(context.Background(), r, Config{NATBridge: "virbr0", LANBridge: "br0"}); err != nil {
		t.Fatal(err)
	}
	c := strings.Join(r.Calls, "\n")
	if strings.Count(c, "nft -f ") != 2 || !strings.Contains(c, "modprobe nf_conntrack_bridge") {
		t.Fatalf("calls:\n%s", c)
	}
	r = &testfake.Runner{Fail: map[string]string{"nft -f": "Error: Could not process rule"}}
	if err := Apply(context.Background(), r, Config{NATBridge: "virbr0"}); err == nil {
		t.Fatal("nft failure must be reported")
	}
}
