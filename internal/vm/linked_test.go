package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// linkFake: cloneDisksFake that also makes linked clones (in memory).
type linkFake struct {
	*cloneDisksFake
	links    map[string]string // clone dom -> source dom
	unlinked []string
}

func (d *linkFake) LinkDisks(_ context.Context, vm, snap, newVM string, idx []int) error {
	if !strings.HasPrefix(snap, "p8s-link-") {
		return errors.New("bad snapshot " + snap)
	}
	d.links[newVM] = vm
	d.live[newVM] = true
	return nil
}
func (d *linkFake) LinkedClones(_ context.Context, vm string) []string {
	var l []string
	for c, s := range d.links {
		if s == vm {
			l = append(l, c)
		}
	}
	return l
}
func (d *linkFake) LinkedTo(_ context.Context, vm string) string { return d.links[vm] }
func (d *linkFake) Unlink(_ context.Context, vm string, _ func(string)) error {
	delete(d.links, vm)
	d.unlinked = append(d.unlinked, vm)
	return nil
}
func (d *linkFake) DestroyDisk(ctx context.Context, vm string) error {
	delete(d.links, vm)
	return d.cloneDisksFake.DestroyDisk(ctx, vm)
}

func TestLinkedClone(t *testing.T) {
	ctx := context.Background()
	svc, _, cd, _ := cloneSvc(t)
	lf := &linkFake{cloneDisksFake: cd, links: map[string]string{}}
	svc.Disks = lf
	if _, err := svc.Create(ctx, CreateRequest{Name: "base", Template: "debian-13"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartClone(ctx, "base", CloneRequest{Name: "c1", Linked: true, Storage: "fast"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("linked onto another storage: %v", err)
	}
	j, err := svc.StartClone(ctx, "base", CloneRequest{Name: "c1", Linked: true, Start: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, svc, j); got.State != "done" || !strings.Contains(got.Message, "linked clone of base") {
		t.Fatalf("job %+v", got)
	}
	if len(cd.copies) != 0 {
		t.Fatalf("copied instead of linked: %v", cd.copies)
	}
	b, _ := svc.Get(ctx, "base")
	c, _ := svc.Get(ctx, "c1")
	if len(b.LinkedClones) != 1 || b.LinkedClones[0] != "c1" || c.LinkedTo != "base" {
		t.Fatalf("base %v c1 %v", b.LinkedClones, c.LinkedTo)
	}
	// the source can't go while the clone uses its disks
	if err := svc.Delete(ctx, "base"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "c1") {
		t.Fatalf("delete source: %v", err)
	}
	if _, err := svc.Get(ctx, "base"); err != nil {
		t.Fatal("source lost after a refused delete")
	}
	svc.Stop(ctx, "base", true)
	// make it independent: only when off
	if _, err := svc.StartUnlink(ctx, "c1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlink while running: %v", err)
	}
	if _, err := svc.StartUnlink(ctx, "base"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlink of a non-clone: %v", err)
	}
	svc.Stop(ctx, "c1", true)
	j, err = svc.StartUnlink(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, svc, j); got.State != "done" || !strings.Contains(got.Message, "independent") {
		t.Fatalf("unlink job %+v", got)
	}
	if err := svc.Delete(ctx, "base"); err != nil {
		t.Fatalf("delete after unlink: %v", err)
	}
}
