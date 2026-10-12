package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	b, err := os.ReadFile("../../deploy/p8sd.example.json")
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "REPLACE_WITH_OUTPUT_OF_p8sd_token", strings.Repeat("a", 64), 1))
	p := filepath.Join(t.TempDir(), "p8sd.json")
	_ = os.WriteFile(p, b, 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.VMDataset != "tank/p8s/vms" || len(c.Templates) != 1 || c.Listen != "127.0.0.1:8006" {
		t.Fatalf("%+v", c)
	}
}

func TestPlaceholderTokenRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p8sd.json")
	_ = os.WriteFile(p, []byte(`{"vm_dataset":"tank/vms","tokens":[{"name":"a","sha256":"REPLACE"}]}`), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("want sha256 error, got %v", err)
	}
}

func TestLANBridgeMustDiffer(t *testing.T) {
	c := Config{VMDataset: "tank/p8s/vms", Bridge: "virbr0", LANBridge: "virbr0",
		Tokens: []Token{{Name: "a", SHA256: strings.Repeat("a", 64)}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "lan_bridge") {
		t.Fatalf("want lan_bridge error, got %v", err)
	}
	c.LANBridge = "br0"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
