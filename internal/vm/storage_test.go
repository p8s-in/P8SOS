package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type placeDisks struct {
	*fakeDisks
	where map[string]string
}

func (p *placeDisks) Place(vm, st string) error {
	if st != "" && st != "local" && st != "san" {
		return fmt.Errorf("no storage %q", st)
	}
	p.where[vm] = st
	return nil
}
func (p *placeDisks) StorageOf(vm string) string {
	if p.where[vm] == "" {
		return "local"
	}
	return p.where[vm]
}
func (p *placeDisks) Storages() []string { return []string{"local", "san"} }

func TestStoragePlacement(t *testing.T) {
	ctx := context.Background()
	svc, _, d, _ := newSvc()
	pd := &placeDisks{fakeDisks: d, where: map[string]string{}}
	svc.Disks = pd
	if _, err := svc.Create(ctx, CreateRequest{Name: "db1", Template: "debian-13", Storage: "nas9"}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "nas9") {
		t.Fatalf("unknown storage: %v", err)
	}
	v, err := svc.Create(ctx, CreateRequest{Name: "db1", Template: "debian-13", Storage: "san"})
	if err != nil || v.Storage != "san" {
		t.Fatalf("on san: %v %+v", err, v)
	}
	w, _ := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13"})
	if w.Storage != "" {
		t.Fatalf("local VM shows storage %q", w.Storage)
	}
	if l := svc.Storages(); len(l) != 2 {
		t.Fatalf("storages %v", l)
	}
}
