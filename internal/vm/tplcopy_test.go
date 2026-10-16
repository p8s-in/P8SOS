package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func (d *cloneDisksFake) SendTemplate(_ context.Context, snaps []string, w io.Writer) error {
	_, err := fmt.Fprint(w, strings.Join(snaps, ","))
	return err
}
func (d *cloneDisksFake) RecvTemplate(_ context.Context, ds string, idx []int, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	d.templates[ds] = true
	d.copies = append(d.copies, fmt.Sprintf("recv %s %v %s", ds, idx, b))
	return nil
}

func TestTemplateCopyBetweenServers(t *testing.T) {
	ctx := context.Background()
	a, _, _, _ := cloneSvc(t)
	if _, err := a.Create(ctx, CreateRequest{Name: "db-base", Template: "debian-13", DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AddDisk(ctx, "db-base", 30); err != nil {
		t.Fatal(err)
	}
	j, _ := a.StartMakeTemplate(ctx, "db-base", TemplateRequest{Name: "db", Label: "Database"})
	if got := waitJob(t, a, j); got.State != "done" {
		t.Fatalf("job %+v", got)
	}
	if err := a.TemplateCopyCheck("debian-13"); !errors.Is(err, ErrConflict) {
		t.Fatalf("built-in: %v", err)
	}
	if err := a.TemplateCopyCheck("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := a.ExportTemplate(ctx, "db", &buf); err != nil {
		t.Fatal(err)
	}
	stream := buf.Bytes()

	b, hv, cd, _ := cloneSvc(t)
	info, err := b.ImportTemplate(ctx, bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "db" || info.Label != "Database" || info.From != "db-base" || !info.Custom || info.MinDiskGB != 10 {
		t.Fatalf("%+v", info)
	}
	if len(cd.copies) != 1 || !strings.HasPrefix(cd.copies[0], "recv tank/t/db [1] tank/t/db@p8s-tpl-") || !strings.Contains(cd.copies[0], ",tank/t/db-disk1@p8s-tpl-") {
		t.Fatalf("%v", cd.copies)
	}
	c, ok := b.custom("db")
	if !ok || !strings.HasPrefix(c.Snapshot, "tank/t/db@p8s-tpl-") || len(c.Extra) != 1 || !strings.HasPrefix(c.Extra[0].Snapshot, "tank/t/db-disk1@p8s-tpl-") {
		t.Fatalf("%+v", c)
	}
	// a VM from the copy, with its data disk
	v, err := b.Create(ctx, CreateRequest{Name: "db7", Template: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Disks) != 1 || v.Disks[0].SizeGB != 30 || len(hv.doms["p8s-db7"]) == 0 {
		t.Fatalf("%+v", v)
	}
	// twice: refused
	if _, err := b.ImportTemplate(ctx, bytes.NewReader(stream)); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	if _, err := b.ImportTemplate(ctx, strings.NewReader("{\"name\":\"../x\"}\n")); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
