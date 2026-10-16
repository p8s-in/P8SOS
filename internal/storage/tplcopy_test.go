package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestTemplateCopy(t *testing.T) {
	dir := t.TempDir()
	calls := fakeStreams(t, dir)
	ctx := context.Background()
	a := &ZFS{Run: &testfake.Runner{Out: map[string]string{}}, Dataset: "tank/vms"}
	var buf bytes.Buffer
	if err := a.SendTemplate(ctx, []string{"tank/p8s/templates/db@p8s-tpl-1", "tank/p8s/templates/db-disk1@p8s-tpl-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	if err := a.SendTemplate(ctx, []string{"tank/p8s/templates/db"}, &buf); err == nil {
		t.Fatal("a dataset isn't a snapshot")
	}
	r := &testfake.Runner{Out: map[string]string{}, Fail: map[string]string{
		"zfs list -H -o name data/tpl/db":       "dataset does not exist",
		"zfs list -H -o name data/tpl/db-disk1": "dataset does not exist",
	}}
	b := &ZFS{Run: r, Dataset: "data/vms"}
	if err := b.RecvTemplate(ctx, "data/tpl/db", []int{1}, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	b0, _ := os.ReadFile(filepath.Join(dir, "data_tpl_db"))
	b1, _ := os.ReadFile(filepath.Join(dir, "data_tpl_db-disk1"))
	if string(b0) != "data of tank/p8s/templates/db" || len(b1) != 3000000 {
		t.Fatalf("%q %d", b0, len(b1))
	}
	if c := strings.Join(*calls, "\n"); !strings.Contains(c, "zfs receive -o volmode=dev data/tpl/db-disk1") {
		t.Fatal(c)
	}
	if !strings.Contains(strings.Join(r.Calls, "\n"), "zfs create -p -o canmount=off data/tpl") {
		t.Fatal(r.Calls)
	}
	// it exists already: nothing is received, nothing deleted
	r2 := &testfake.Runner{Out: map[string]string{}}
	c := &ZFS{Run: r2, Dataset: "data/vms"}
	if err := c.RecvTemplate(ctx, "data/tpl/db", nil, bytes.NewReader(buf.Bytes())); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(r2.Calls, "\n"), "destroy") {
		t.Fatal(r2.Calls)
	}
}
