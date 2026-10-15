package vm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

func TestRestoreToStorage(t *testing.T) {
	ctx := context.Background()
	svc, hv, d, _ := newSvc()
	withBackups(t, svc)
	rd := &relocDisks{fakeDisks: d, where: map[string]string{}}
	svc.Disks = rd
	if _, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	j, _ := svc.StartBackup(ctx, "web1")
	waitJob(t, svc, j)
	b := mustBackups(t, svc, "web1")[0]
	if _, err := svc.StartRestoreTo(ctx, "web1", b.ID, "copy1", "nas"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown storage: %v", err)
	}
	svc.Stop(ctx, "web1", true)
	if _, err := svc.StartRestoreTo(ctx, "web1", b.ID, "", "ceph"); !errors.Is(err, ErrConflict) {
		t.Fatalf("in place with a storage: %v", err)
	}
	j, err := svc.StartRestoreTo(ctx, "web1", b.ID, "copy1", "ceph")
	if err != nil {
		t.Fatal(err)
	}
	if done := waitJob(t, svc, j); done.State != jobs.Done {
		t.Fatalf("%+v", done)
	}
	sp, _ := virt.ParseDomainXML(hv.doms["p8s-copy1"])
	if rd.where["p8s-copy1"] != "ceph" || !strings.HasPrefix(sp.DiskPath, "/dev/rbd/") || strings.Join(rd.called, ",") != "p8s-copy1 ceph" {
		t.Fatalf("not on ceph: %v %s %v", rd.where, sp.DiskPath, rd.called)
	}
}

func mustBackups(t *testing.T, svc *Service, name string) []Backup {
	t.Helper()
	l, err := svc.ListBackups(name)
	if err != nil || len(l) == 0 {
		t.Fatalf("backups: %v %v", l, err)
	}
	return l
}
