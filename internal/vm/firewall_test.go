package vm

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"p8sos/internal/firewall"
)

func TestFirewall(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newSvc()
	svc.FirewallFile = filepath.Join(t.TempDir(), "firewall.json")
	var applied []firewall.VM
	fail := false
	svc.Firewall = func(_ context.Context, vms []firewall.VM) error {
		if fail {
			return errors.New("nf_conntrack_bridge missing")
		}
		applied = vms
		return nil
	}
	v, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", SSHKeys: []string{"ssh-ed25519 AAAA test"}})
	if err != nil {
		t.Fatal(err)
	}
	if fw, err := svc.GetFirewall(ctx, "web1"); err != nil || fw.Enabled || fw.Rules == nil {
		t.Fatalf("default firewall %+v %v", fw, err)
	}
	if _, err := svc.GetFirewall(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal("firewall of a missing VM")
	}
	if _, err := svc.SetFirewall(ctx, "web1", firewall.Settings{Enabled: true, Rules: []firewall.Rule{{Proto: "tcp", Ports: "x"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad rule: %v", err)
	}
	fw, err := svc.SetFirewall(ctx, "web1", firewall.Settings{Enabled: true, Rules: []firewall.Rule{{Proto: "tcp", Ports: "22"}}})
	if err != nil || !fw.Enabled {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].MAC != v.MAC || applied[0].FW.Rules[0].Ports != "22" {
		t.Fatalf("applied %+v", applied)
	}
	// rules that can't be loaded change nothing
	fail = true
	if _, err := svc.SetFirewall(ctx, "web1", firewall.Settings{Enabled: true, Rules: []firewall.Rule{{Proto: "tcp", Ports: "80"}}}); err == nil {
		t.Fatal("apply error not reported")
	}
	if fw, _ := svc.GetFirewall(ctx, "web1"); len(fw.Rules) != 1 || fw.Rules[0].Ports != "22" {
		t.Fatalf("failed change was kept: %+v", fw)
	}
	fail = false
	// a fresh service (p8sd restart) reads the file
	svc2, _, _, _ := newSvc()
	svc2.HV, svc2.Disks, svc2.Seeds, svc2.FirewallFile = svc.HV, svc.Disks, svc.Seeds, svc.FirewallFile
	svc2.Firewall = svc.Firewall
	applied = nil
	if err := svc2.ApplyFirewalls(ctx); err != nil || len(applied) != 1 {
		t.Fatalf("re-apply after restart: %v %+v", err, applied)
	}
	// deleting the VM removes its rules
	if err := svc.Delete(ctx, "web1"); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("rules left after delete: %+v", applied)
	}
}
