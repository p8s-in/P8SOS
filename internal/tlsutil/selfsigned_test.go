package tlsutil

import (
	"crypto/tls"
	"testing"
)

func TestEnsureCreatesUsablePairOnce(t *testing.T) {
	dir := t.TempDir()
	c1, k1, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(c1, k1); err != nil {
		t.Fatalf("pair not loadable: %v", err)
	}
	c2, _, err := Ensure(dir) // second call reuses
	if err != nil || c2 != c1 {
		t.Fatalf("reuse: %v %s", err, c2)
	}
}
