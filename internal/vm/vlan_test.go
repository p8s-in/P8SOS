package vm

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"p8sos/internal/virt"
)

type fakeVLANs struct{ made []int }

func (f *fakeVLANs) Bridge(_ context.Context, vid int) (string, error) {
	f.made = append(f.made, vid)
	return fmt.Sprintf("br0v%d", vid), nil
}
func (f *fakeVLANs) VLANOf(b string) int {
	var n int
	if _, err := fmt.Sscanf(b, "br0v%d", &n); err == nil {
		return n
	}
	return 0
}

func TestVLANVM(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	svc.LANBridge = "br0"
	if _, err := svc.Create(ctx, CreateRequest{Name: "v1", Template: "debian-13", Network: NetLAN, VLAN: 100}); err == nil || !strings.Contains(err.Error(), "VLANs aren't available") {
		t.Fatalf("no VLAN support: %v", err)
	}
	fv := &fakeVLANs{}
	svc.VLANs = fv
	if _, err := svc.Create(ctx, CreateRequest{Name: "v1", Template: "debian-13", VLAN: 100}); err == nil {
		t.Fatal("VLAN on the NAT network accepted")
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "v1", Template: "debian-13", Network: NetLAN, VLAN: 5000}); err == nil {
		t.Fatal("VLAN 5000 accepted")
	}
	v, err := svc.Create(ctx, CreateRequest{Name: "v1", Template: "debian-13", Network: NetLAN, VLAN: 100})
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := virt.ParseDomainXML(hv.doms["p8s-v1"])
	if v.Network != NetLAN || v.VLAN != 100 || sp.Bridge != "br0v100" || len(fv.made) != 1 {
		t.Fatalf("%+v bridge %s", v, sp.Bridge)
	}
	// a move keeps the VLAN
	md := &memDisks{fakeDisks: svc.Disks.(*fakeDisks), data: map[string][]byte{}}
	svc.Disks = md
	svc.Stop(ctx, "v1", true)
	var buf bytes.Buffer
	if err := svc.ExportVM(ctx, "v1", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"vlan":100`) {
		t.Fatalf("header %.200s", buf.String())
	}
	b, _, _, _ := newSvc()
	b.LANBridge, b.VLANs = "br0", &fakeVLANs{}
	b.Disks = &memDisks{fakeDisks: &fakeDisks{live: map[string]bool{}}, data: map[string][]byte{}}
	mv, note, err := b.ImportMovedVM(ctx, &buf)
	if err != nil || mv.VLAN != 100 || note != "" {
		t.Fatalf("%+v %q %v", mv, note, err)
	}
}
