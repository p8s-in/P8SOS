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

func TestUserDataRootPassword(t *testing.T) {
	h := HashPassword("longenough")
	ud := UserData(Spec{Hostname: "web1", RootPasswordHash: h, RootSSHPassword: true})
	for _, want := range []string{"disable_root: false", `password: "` + h + `", type: hash`, "ssh_pwauth: true",
		"PermitRootLogin yes", "reload-or-restart sshd", "expire: false"} {
		if !strings.Contains(ud, want) {
			t.Errorf("missing %q in:\n%s", want, ud)
		}
	}
	if strings.Contains(ud, "longenough") {
		t.Fatal("plaintext password in user-data")
	}
	ud = UserData(Spec{Hostname: "web1", RootPasswordHash: h})
	if strings.Contains(ud, "PermitRootLogin") || strings.Contains(ud, "ssh_pwauth") {
		t.Fatalf("SSH password login must be opt-in:\n%s", ud)
	}
	if ud = UserData(Spec{Hostname: "web1"}); strings.Contains(ud, "chpasswd") || strings.Contains(ud, "disable_root") {
		t.Fatalf("no password -> root untouched:\n%s", ud)
	}
}
