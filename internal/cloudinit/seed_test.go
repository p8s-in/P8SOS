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

func TestNetworkConfig(t *testing.T) {
	nc := NetworkConfig(Static{MAC: "52:54:00:AB:CD:EF", IP: "203.0.113.10", Prefix: 29, Gateway: "203.0.113.9", DNS: []string{"1.1.1.1", "8.8.8.8"}})
	want := `version: 2
ethernets:
  eth0:
    match:
      macaddress: "52:54:00:ab:cd:ef"
    set-name: eth0
    addresses: ["203.0.113.10/29"]
    routes:
      - to: 0.0.0.0/0
        via: 203.0.113.9
    nameservers:
      addresses: ["1.1.1.1", "8.8.8.8"]
`
	if nc != want {
		t.Fatalf("got\n%s\nwant\n%s", nc, want)
	}
	if nc := NetworkConfig(Static{MAC: "52:54:00:00:00:01", IP: "198.51.100.7", Prefix: 32, Gateway: "172.31.1.1", OnLink: true}); !strings.Contains(nc, "on-link: true") {
		t.Fatalf("routed /32 needs an on-link route:\n%s", nc)
	}
}

func TestDHCPConfig(t *testing.T) {
	c := DHCPConfig("52:54:00:AA:BB:CC")
	if !strings.Contains(c, `macaddress: "52:54:00:aa:bb:cc"`) || !strings.Contains(c, "dhcp-identifier: mac") || !strings.Contains(c, "dhcp4: true") {
		t.Fatal(c)
	}
}
