package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/execx"
)

// cephFake plays zfs/rbd: longest matching prefix wins; mapped devices appear.
type cephFake struct {
	mu    sync.Mutex
	calls []string
	out   map[string]string
	fail  map[string]string
	devs  map[string]bool
}

func (f *cephFake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if name == "rbd" && len(args) > 3 && args[0] == "device" && args[1] == "map" {
		f.devs["/dev/rbd/"+args[len(args)-1]] = true
	}
	if name == "rbd" && len(args) > 2 && args[0] == "device" && args[1] == "unmap" {
		delete(f.devs, args[2])
	}
	if name == "zfs" && len(args) > 0 && args[0] == "clone" {
		f.devs["/dev/zvol/"+args[len(args)-1]] = true
	}
	best, bestOut, isFail := "", "", false
	for p, e := range f.fail {
		if strings.HasPrefix(line, p) && len(p) > len(best) {
			best, bestOut, isFail = p, e, true
		}
	}
	for p, o := range f.out {
		if strings.HasPrefix(line, p) && len(p) > len(best) {
			best, bestOut, isFail = p, o, false
		}
	}
	if isFail {
		return nil, &execx.Error{Cmd: name, Args: args, Stderr: bestOut, Err: errors.New("exit 1")}
	}
	return []byte(bestOut), nil
}

func (f *cephFake) exists(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.devs[p]
}

func (f *cephFake) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

func cephZFS(t *testing.T, f *cephFake) *ZFS {
	t.Helper()
	maps := filepath.Join(t.TempDir(), "rbd-maps")
	r := &RBD{Run: f, Pool: "p8s-vms", MapsFile: maps, Exists: f.exists, Wait: time.Second,
		Ready: func(context.Context) bool { return true }}
	return &ZFS{Run: f, Dataset: "tank/p8s/vms", Exists: f.exists, Wait: time.Second, BusyRetry: time.Millisecond, Ceph: r}
}

func TestCephPlacement(t *testing.T) {
	f := &cephFake{devs: map[string]bool{"/dev/zvol/tank/p8s/vms/p8s-local1-disk0": true},
		out: map[string]string{"rbd ls --pool p8s-vms": "p8s-web1-disk0\np8s-web1-disk1\np8s-web1-disk3\np8s-tpl-debian-13-abc\n"}}
	z := cephZFS(t, f)
	if !z.Shared("p8s-web1") || z.StorageOf("p8s-web1") != CephStorage {
		t.Fatal("web1 isn't on Ceph")
	}
	if z.Shared("p8s-local1") || z.StorageOf("p8s-local1") != LocalStorage || z.DevicePath("p8s-local1") != "/dev/zvol/tank/p8s/vms/p8s-local1-disk0" {
		t.Fatal("a local VM moved to Ceph")
	}
	if z.DevicePath("p8s-web1") != "/dev/rbd/p8s-vms/p8s-web1-disk0" || z.ExtraPath("p8s-web1", 3) != "/dev/rbd/p8s-vms/p8s-web1-disk3" {
		t.Fatal(z.DevicePath("p8s-web1"))
	}
	if got := z.Extras(context.Background(), "p8s-web1"); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("extras %v", got)
	}
	if l := z.Storages(); strings.Join(l, ",") != "local,ceph" {
		t.Fatalf("storages %v", l)
	}
	if err := z.Place("p8s-new", CephStorage); err != nil || !z.Shared("p8s-new") {
		t.Fatalf("place: %v", err)
	}
	z.Ceph.Ready = func(context.Context) bool { return false }
	z.Ceph.readyAt = time.Time{}
	if err := z.Place("p8s-new2", CephStorage); err == nil {
		t.Fatal("placed on Ceph that isn't ready")
	}
}

func TestCephCloneAndDestroy(t *testing.T) {
	f := &cephFake{devs: map[string]bool{}, out: map[string]string{
		"rbd ls --pool p8s-vms":           "",
		"zfs get -H -o value guid":        "1234567890123456789\n",
		"rbd snap ls --format json":       "[]",
		"rbd info --format json":          `{"size": 2147483648}`,
		"rbd status --format json":        `{"watchers":[]}`,
		"rbd info p8s-vms/p8s-tpl-debian": "",
	}, fail: map[string]string{"rbd info p8s-vms/p8s-tpl-debian": "No such file or directory"}}
	z := cephZFS(t, f)
	ctx := context.Background()
	_ = z.Place("p8s-web1", CephStorage)
	dev, err := z.CloneDisk(ctx, "tank/p8s/templates/debian-13@base", "p8s-web1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if dev != "/dev/rbd/p8s-vms/p8s-web1-disk0" {
		t.Fatal(dev)
	}
	all := f.all()
	for _, want := range []string{
		"zfs clone -o volmode=dev tank/p8s/templates/debian-13@base tank/p8s/vms/p8s-ceph-import",
		"rbd import --no-progress --image-format 2 /dev/zvol/tank/p8s/vms/p8s-ceph-import p8s-vms/p8s-tpl-debian-13-base-123456789012",
		"rbd snap protect p8s-vms/p8s-tpl-debian-13-base-123456789012@base",
		"rbd clone p8s-vms/p8s-tpl-debian-13-base-123456789012@base p8s-vms/p8s-web1-disk0",
		"rbd resize --no-progress --size 20G p8s-vms/p8s-web1-disk0",
		"rbd device map --options ms_mode=secure p8s-vms/p8s-web1-disk0",
		"zfs destroy -r tank/p8s/vms/p8s-ceph-import",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	maps, _ := os.ReadFile(z.Ceph.MapsFile)
	if !strings.Contains(string(maps), "p8s-vms/p8s-web1-disk0\n") {
		t.Fatalf("maps file:\n%s", maps)
	}

	// a second VM: the template copy is there already
	f.mu.Lock()
	f.calls = nil
	f.out["rbd snap ls --format json p8s-vms/p8s-tpl"] = `[{"id":4,"name":"base","size":2147483648,"protected":"true","timestamp":"Mon Oct 12 10:00:00 2026"}]`
	f.mu.Unlock()
	_ = z.Place("p8s-web2", CephStorage)
	if _, err := z.CloneDisk(ctx, "tank/p8s/templates/debian-13@base", "p8s-web2", 20); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "rbd import") {
		t.Fatal("template copied twice")
	}

	// web1 is open on another server: deleting it here only closes it
	f.mu.Lock()
	f.calls = nil
	f.out["rbd status --format json p8s-vms/p8s-web1-disk0"] = `{"watchers":[{"address":"192.168.1.12:0/1"}]}`
	f.mu.Unlock()
	if err := z.DestroyDisk(ctx, "p8s-web1"); err != nil {
		t.Fatal(err)
	}
	all = f.all()
	if !strings.Contains(all, "rbd device unmap /dev/rbd/p8s-vms/p8s-web1-disk0") || strings.Contains(all, "rbd rm") {
		t.Fatalf("in use elsewhere:\n%s", all)
	}
	maps, _ = os.ReadFile(z.Ceph.MapsFile)
	if strings.Contains(string(maps), "web1") {
		t.Fatal("still in the maps file")
	}
	// web2 isn't: it goes
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if err := z.DestroyDisk(ctx, "p8s-web2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "rbd snap purge p8s-vms/p8s-web2-disk0") || !strings.Contains(f.all(), "rbd rm --no-progress p8s-vms/p8s-web2-disk0") {
		t.Fatalf("not deleted:\n%s", f.all())
	}
}

func TestCephSnapshots(t *testing.T) {
	f := &cephFake{devs: map[string]bool{"/dev/rbd/p8s-vms/p8s-db-disk0": true, "/dev/rbd/p8s-vms/p8s-db-disk1": true},
		out: map[string]string{
			"rbd ls --pool p8s-vms": "p8s-db-disk0\np8s-db-disk1\n",
			"rbd snap ls --format json p8s-vms/p8s-db-disk0": `[{"name":"a","timestamp":"Mon Oct 12 10:00:00 2026"},
				{"name":"p8s-rep-1","timestamp":"Mon Oct 12 10:30:00 2026"},{"name":"b","timestamp":"Mon Oct 12 11:00:00 2026"}]`,
			"rbd snap ls --format json p8s-vms/p8s-db-disk1": `[{"name":"b","timestamp":"Mon Oct 12 11:00:00 2026"}]`,
		}}
	z := cephZFS(t, f)
	ctx := context.Background()
	l, err := z.Snapshots(ctx, "p8s-db")
	if err != nil || len(l) != 2 || l[0].Name != "a" || l[1].Name != "b" || l[0].CreatedAt == "" {
		t.Fatalf("snapshots %+v %v", l, err)
	}
	if err := z.Snapshot(ctx, "p8s-db", "c"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "rbd snap create p8s-vms/p8s-db-disk0@c") || !strings.Contains(f.all(), "rbd snap create p8s-vms/p8s-db-disk1@c") {
		t.Fatal(f.all())
	}
	if !z.HasSnapshot(ctx, "p8s-db", "b") || z.HasSnapshot(ctx, "p8s-db", "a") {
		t.Fatal("HasSnapshot: a is on disk0 only")
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if err := z.Rollback(ctx, "p8s-db", "a", true); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	iUnmap := strings.Index(all, "rbd device unmap")
	iRoll := strings.Index(all, "rbd snap rollback --no-progress p8s-vms/p8s-db-disk0@a")
	iMap := strings.LastIndex(all, "rbd device map")
	if iUnmap < 0 || iRoll < iUnmap || iMap < iRoll {
		t.Fatalf("rollback order (close, roll back, open):\n%s", all)
	}
	if strings.Contains(all, "p8s-db-disk1@a") {
		t.Fatal("rolled back a disk that has no such snapshot")
	}
	if !strings.Contains(all, "rbd snap rm --no-progress p8s-vms/p8s-db-disk0@b") {
		t.Fatalf("newer snapshot b not removed:\n%s", all)
	}
	if ParseRBDTime("Mon Oct 12 10:00:00 2026").IsZero() {
		t.Fatal("time")
	}
	if err := z.Release(ctx, "p8s-db"); err != nil || f.exists("/dev/rbd/p8s-vms/p8s-db-disk1") {
		t.Fatalf("release: %v", err)
	}
	if err := z.RetireDisk(ctx, "p8s-db"); err != nil || strings.Contains(f.all(), "zfs rename") {
		t.Fatalf("retire must only close Ceph disks: %v", err)
	}
}
