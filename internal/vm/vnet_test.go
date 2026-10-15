package vm

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"p8sos/internal/virt"
)

type fakeVNets struct{ names map[string]int }

func (f *fakeVNets) BridgeOf(_ context.Context, name string) (string, error) {
	if n, ok := f.names[name]; ok {
		return fmt.Sprintf("vxb%d", n), nil
	}
	return "", fmt.Errorf("%w: no vnet %s", ErrInvalid, name)
}
func (f *fakeVNets) NameOf(b string) string {
	for n, v := range f.names {
		if b == fmt.Sprintf("vxb%d", v) {
			return n
		}
	}
	return ""
}
func (f *fakeVNets) Names() []string {
	var l []string
	for n := range f.names {
		l = append(l, n)
	}
	return l
}

func TestVNetVM(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	if _, err := svc.Create(ctx, CreateRequest{Name: "w1", Template: "debian-13", Network: "vnet:web"}); err == nil || !strings.Contains(err.Error(), "aren't available") {
		t.Fatalf("no vnets: %v", err)
	}
	svc.VNets = &fakeVNets{names: map[string]int{"web": 10000, "db": 10001}}
	if _, err := svc.Create(ctx, CreateRequest{Name: "w1", Template: "debian-13", Network: "vnet:nope"}); err == nil || !strings.Contains(err.Error(), `no virtual network "nope"`) {
		t.Fatalf("unknown vnet: %v", err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "w1", Template: "debian-13", Network: "vnet:web", VLAN: 5}); err == nil {
		t.Fatal("VLAN on a vnet accepted")
	}
	v, err := svc.Create(ctx, CreateRequest{Name: "w1", Template: "debian-13", Network: "vnet:web"})
	if err != nil {
		t.Fatal(err)
	}
	x := string(hv.doms["p8s-w1"])
	sp, _ := virt.ParseDomainXML(hv.doms["p8s-w1"])
	if v.Network != "vnet:web" || sp.Bridge != "vxb10000" || !strings.Contains(x, `<mtu size="1230">`) {
		t.Fatalf("%+v bridge %s\n%s", v, sp.Bridge, x)
	}
	// a second card on another vnet, and one on nat (no small MTU there)
	if _, _, err := svc.AddNIC(ctx, "w1", "vnet:db", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddNIC(ctx, "w1", "nat", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddNIC(ctx, "w1", "vnet:nope", 0); err == nil {
		t.Fatal("unknown vnet card")
	}
	g, _ := svc.Get(ctx, "w1")
	if len(g.NICs) != 2 || g.NICs[0].Network != "vnet:db" || g.NICs[1].Network != "nat" {
		t.Fatalf("%+v", g.NICs)
	}
	if n := strings.Count(string(hv.doms["p8s-w1"]), `<mtu size="1230">`); n != 2 {
		t.Fatalf("mtu on %d cards\n%s", n, hv.doms["p8s-w1"])
	}
	// moved to another server: same vnets there
	md := &memDisks{fakeDisks: svc.Disks.(*fakeDisks), data: map[string][]byte{}}
	svc.Disks = md
	svc.Stop(ctx, "w1", true)
	var buf bytes.Buffer
	if err := svc.ExportVM(ctx, "w1", &buf); err != nil {
		t.Fatal(err)
	}
	b, bhv, _, _ := newSvc()
	b.VNets = &fakeVNets{names: map[string]int{"web": 10000, "db": 10001}}
	b.Disks = &memDisks{fakeDisks: &fakeDisks{live: map[string]bool{}}, data: map[string][]byte{}}
	mv, note, err := b.ImportMovedVM(ctx, &buf)
	if err != nil || mv.Network != "vnet:web" || note != "" {
		t.Fatalf("%+v %q %v", mv, note, err)
	}
	if !strings.Contains(string(bhv.doms["p8s-w1"]), `bridge="vxb10001"`) {
		t.Fatalf("second card lost its vnet:\n%s", bhv.doms["p8s-w1"])
	}
}
