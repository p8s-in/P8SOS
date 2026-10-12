package virt

import "testing"

func TestParseVNCDisplay(t *testing.T) {
	for in, want := range map[string]string{
		"vnc://localhost:0\n": "127.0.0.1:5900",
		"vnc://127.0.0.1:3":   "127.0.0.1:5903",
	} {
		if got, err := ParseVNCDisplay(in); err != nil || got != want {
			t.Errorf("%q -> %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "spice://localhost:0", "vnc://0.0.0.0:0", "vnc://localhost:x"} {
		if _, err := ParseVNCDisplay(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
