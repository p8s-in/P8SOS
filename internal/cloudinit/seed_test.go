package cloudinit

import (
	"strings"
	"testing"
)

func TestUserData(t *testing.T) {
	ud := UserData(Spec{Hostname: "web1", SSHKeys: []string{"ssh-ed25519 AAAA me@laptop\n"}})
	for _, want := range []string{"#cloud-config\n", "hostname: web1", `- "ssh-ed25519 AAAA me@laptop"`, "qemu-guest-agent", "growpart"} {
		if !strings.Contains(ud, want) {
			t.Errorf("missing %q in\n%s", want, ud)
		}
	}
}
