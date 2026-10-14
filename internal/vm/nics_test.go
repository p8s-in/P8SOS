package vm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"p8sos/internal/virt"
)

type nicHV struct {
	*fakeHV
	plugged, unplugged []string
}

func (a *nicHV) AttachDisk(_ context.Context, _ string, x []byte) error {
	a.plugged = append(a.plugged, string(x))
	return nil
}
func (a *nicHV) DetachDevice(_ context.Context, _ string, x []byte) error {
	a.unplugged = append(a.unplugged, string(x))
	return nil
}

func TestNetworkCards(t *testing.T) {
	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	nhv := &nicHV{fakeHV: hv}
	svc.HV = nhv
	if _, err := svc.Create(ctx, CreateRequest{Name: "fw1", Template: "debian-13", VCPUs: 1, MemoryMiB: 1024, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	// home network isn't set up here
	if _, _, err := svc.AddNIC(ctx, "fw1", "lan", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("lan without bridge: %v", err)
	}
	if _, _, err := svc.AddNIC(ctx, "fw1", "public", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("public: %v", err)
	}
	v, note, err := svc.AddNIC(ctx, "fw1", "nat", 0)
	if err != nil || len(v.NICs) != 1 || v.NICs[0].Network != "nat" || !strings.Contains(note, "plugged in") {
		t.Fatalf("add: %v %q %+v", err, note, v.NICs)
	}
	if len(nhv.plugged) != 1 || !strings.Contains(nhv.plugged[0], "<interface") || !strings.Contains(nhv.plugged[0], v.NICs[0].MAC) {
		t.Fatalf("plug: %v", nhv.plugged)
	}
	x, _ := hv.XML(ctx, "p8s-fw1")
	sp, _ := virt.ParseDomainXML(x)
	if len(sp.NICs) != 1 || sp.NICs[0].MAC != v.NICs[0].MAC || sp.MAC == sp.NICs[0].MAC {
		t.Fatalf("definition: %+v", sp)
	}
	// the most
	for i := 1; i < virt.MaxExtraNICs; i++ {
		if _, _, err := svc.AddNIC(ctx, "fw1", "nat", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.AddNIC(ctx, "fw1", "nat", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("too many: %v", err)
	}
	// remove: not the first card; live unplug
	if _, err := svc.RemoveNIC(ctx, "fw1", sp.MAC); !errors.Is(err, ErrInvalid) {
		t.Fatalf("first card removed: %v", err)
	}
	if _, err := svc.RemoveNIC(ctx, "fw1", "52:54:00:00:00:01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown card: %v", err)
	}
	v, err = svc.RemoveNIC(ctx, "fw1", strings.ToUpper(v.NICs[0].MAC))
	if err != nil || len(v.NICs) != virt.MaxExtraNICs-1 || len(nhv.unplugged) != 1 {
		t.Fatalf("remove: %v %d %d", err, len(v.NICs), len(nhv.unplugged))
	}
	// a clone gets its own MACs on every card
	h, err := svc.moveHeader(ctx, "fw1")
	if err != nil || len(h.NICs) != virt.MaxExtraNICs-1 {
		t.Fatalf("move header: %v %+v", err, h.NICs)
	}
}
