package ceph

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const monmapPrint = `monmaptool: monmap file /x/monmap
epoch 3
fsid 11111111-2222-4333-8444-555555555555
last_changed 2026-10-14T21:00:00.000000+0000
created 2026-10-13T10:00:00.000000+0000
min_mon_release 19 (squid)
election_strategy: 1
0: [v2:192.168.1.11:3300/0,v1:192.168.1.11:6789/0] mon.n1
1: [v2:192.168.1.12:3300/0,v1:192.168.1.12:6789/0] mon.p8s-vm1
`

func TestParseMonmap(t *testing.T) {
	if got := ParseMonmap(monmapPrint); strings.Join(got, ",") != "n1,p8s-vm1" {
		t.Fatal(got)
	}
}

func setUp(t *testing.T, m *Manager, mons []Mon) {
	t.Helper()
	key := "[client.admin]\n\tkey = QUJDRA==\n"
	if err := m.SetBundle(Bundle{Conf: Conf("11111111-2222-4333-8444-555555555555", netip.MustParsePrefix("192.168.1.0/24"), mons),
		AdminKeyring: key, BootstrapOSD: key}); err != nil {
		t.Fatal(err)
	}
}

func TestMonAlone(t *testing.T) {
	ctx := context.Background()
	r := &fakeRun{out: map[string]string{"monmaptool --print": monmapPrint, "ceph --connect-timeout 6 status": statusJSON}}
	m := newMgr(t, r)
	setUp(t, m, []Mon{{"n1", "192.168.1.11"}, {"p8s-vm1", "192.168.1.12"}})
	if err := m.CheckMonAlone(); err == nil {
		t.Fatal("no monitor here, yet allowed")
	}
	_ = os.MkdirAll(m.monDir(), 0o750)
	if err := m.MonAlone(ctx); err != nil {
		t.Fatal(err)
	}
	calls := r.all()
	order := []string{"systemctl stop ceph-mon@n1", "ceph-mon -i n1 --extract-monmap", "monmaptool --print",
		"--rm p8s-vm1", "ceph-mon -i n1 --inject-monmap", "systemctl start ceph-mon@n1", "ceph --connect-timeout 20 status"}
	at := 0
	for _, w := range order {
		i := strings.Index(calls[at:], w)
		if i < 0 {
			t.Fatalf("%q missing or out of order in\n%s", w, calls)
		}
		at += i
	}
	if strings.Contains(calls, "--rm n1") {
		t.Fatal("removed itself")
	}
	c, _ := os.ReadFile(m.ConfFile())
	if strings.Contains(string(c), "192.168.1.12") || !strings.Contains(string(c), "192.168.1.11:3300") {
		t.Fatalf("conf:\n%s", c)
	}
}

func TestRemoveCeph(t *testing.T) {
	ctx := context.Background()
	r := &fakeRun{out: map[string]string{"systemctl list-unit-files ceph-volume@*": "ceph-volume@lvm-0-abcd.service enabled enabled\n"}}
	m := newMgr(t, r)
	if err := m.CheckRemove(ctx); err == nil {
		t.Fatal("nothing to remove, yet allowed")
	}
	setUp(t, m, []Mon{{"n1", "192.168.1.11"}})
	_ = os.MkdirAll(m.monDir(), 0o750)
	_ = os.MkdirAll(m.lib("osd/ceph-0"), 0o750)
	m.InUse = func(context.Context) []string { return []string{"web1"} }
	if err := m.CheckRemove(ctx); err == nil || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "web1") {
		t.Fatalf("VMs on Ceph: %v", err)
	}
	m.InUse = nil
	if err := m.Remove(ctx, false); err != nil {
		t.Fatal(err)
	}
	calls := r.all()
	for _, w := range []string{"systemctl disable --now ceph-mon@n1", "systemctl disable --now ceph-osd@0",
		"systemctl disable --now ceph-volume@lvm-0-abcd.service", "umount " + m.lib("osd/ceph-0")} {
		if !strings.Contains(calls, w) {
			t.Fatalf("no %q in\n%s", w, calls)
		}
	}
	if strings.Contains(calls, "zap") {
		t.Fatal("wiped without being asked")
	}
	if m.Configured() || m.exists(m.monDir()) {
		t.Fatal("still set up")
	}
	aside, _ := filepath.Glob(m.lib("p8s-removed-*"))
	if len(aside) != 1 {
		t.Fatalf("files not put aside: %v", aside)
	}
	if ents, _ := os.ReadDir(aside[0]); len(ents) < 3 {
		t.Fatalf("aside: %v", ents)
	}
}
