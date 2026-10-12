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

func TestParseIfList(t *testing.T) {
	out := ` Interface   Type     Source   Model    MAC
-------------------------------------------------------
 vnet3       bridge   br0      virtio   52:54:00:12:34:56

`
	if p := ParseIfList(out); len(p) != 1 || p[0] != "vnet3" {
		t.Fatalf("ports %v", p)
	}
	stopped := ` Interface   Type     Source   Model    MAC
-------------------------------------------------------
 -           bridge   br0      virtio   52:54:00:12:34:56
`
	if p := ParseIfList(stopped); len(p) != 0 {
		t.Fatalf("stopped VM ports %v", p)
	}
}
