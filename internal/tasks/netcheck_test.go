package tasks

import "testing"

func TestParseNetCheck(t *testing.T) {
	out := `==> Host internet
  ✔ default route: 192.168.0.1 via br0
  ✔ the server reaches the internet
==> NAT network (virbr0, used by NAT VMs and containers)
  ✔ libvirt network 'default' is active
  ✘ the NAT rule for 192.168.122.0/24 is missing — guests get no internet
    usually a firewall reload removed libvirt's rules
==> From outside
  · the internet sees this server as 203.0.113.5 (your router's address)
  · nothing is set up to reach it from outside

1 problem(s) found. Click Repair.
`
	r := ParseNetCheck(out)
	if len(r.Sections) != 3 || r.Problems != 1 || r.Summary != "1 problem(s) found. Click Repair." {
		t.Fatalf("%+v", r)
	}
	nat := r.Sections[1].Items[1]
	if nat.State != "bad" || len(nat.Hints) != 1 || nat.Text[:7] != "the NAT" {
		t.Fatalf("%+v", nat)
	}
	if r.Sections[2].Items[0].State != "info" || r.Sections[0].Items[0].State != "ok" {
		t.Fatalf("%+v", r.Sections)
	}
}
