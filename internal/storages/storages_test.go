package storages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func newMgr(t *testing.T, r *testfake.Runner) (*Manager, string) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "by-path"), 0o755)
	os.WriteFile(filepath.Join(dir, "fstab"), []byte("UUID=abc / ext4 defaults 0 1\n"), 0o644)
	return &Manager{Run: r, File: filepath.Join(dir, "storages.json"), MountRoot: filepath.Join(dir, "mnt"),
		Fstab: filepath.Join(dir, "fstab"), Mounts: filepath.Join(dir, "mounts"), ByPath: filepath.Join(dir, "by-path"), Wait: 1}, dir
}

func TestNFS(t *testing.T) {
	ctx := context.Background()
	r := &testfake.Runner{Out: map[string]string{"timeout 5 stat -f": "1000 400 4096\n"}}
	m, dir := newMgr(t, r)
	for _, bad := range []Storage{
		{Name: "Nas", Type: NFS, Server: "10.0.0.5", Export: "/x"},
		{Name: "local", Type: NFS, Server: "10.0.0.5", Export: "/x"},
		{Name: "nas", Type: NFS, Server: "10.0.0.5;rm", Export: "/x"},
		{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "x"},
		{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "/a/../b"},
		{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "/x", Options: "vers=3 ro"},
		{Name: "nas", Type: "smb", Server: "10.0.0.5", Export: "/x"},
	} {
		if _, err := m.Add(ctx, bad, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
	mp := m.MountPoint("nas")
	st, err := m.Add(ctx, Storage{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "/mnt/tank/p8s", Options: "vers=3"}, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(r.Calls, "\n")
	if !strings.Contains(calls, "mount -t nfs -o soft,timeo=150,retrans=3,_netdev,nofail,x-systemd.mount-timeout=30,vers=3 10.0.0.5:/mnt/tank/p8s "+mp) ||
		!strings.Contains(calls, "mkdir -p "+mp+"/p8s/iso") {
		t.Fatalf("calls %s", calls)
	}
	fs, _ := os.ReadFile(filepath.Join(dir, "fstab"))
	if !strings.Contains(string(fs), "UUID=abc") || !strings.Contains(string(fs), "# p8s-storage nas\n10.0.0.5:/mnt/tank/p8s "+mp+" nfs ") {
		t.Fatalf("fstab:\n%s", fs)
	}
	if st.Online || st.Error == "" { // /proc/mounts (fake) doesn't list it
		t.Fatalf("status %+v", st)
	}
	os.WriteFile(m.Mounts, []byte("10.0.0.5:/mnt/tank/p8s "+mp+" nfs4 rw 0 0\n"), 0o644)
	l := m.List(ctx)
	if len(l) != 1 || !l[0].Online || l[0].SizeBytes != 4096000 || l[0].FreeBytes != 1638400 || l[0].Uses == "" {
		t.Fatalf("list %+v", l)
	}
	if sh := m.ISOShelves(); sh["nas"] != mp+"/p8s/iso" {
		t.Fatalf("shelves %v", sh)
	}
	if _, err := m.Add(ctx, Storage{Name: "nas", Type: NFS, Server: "10.0.0.6", Export: "/y"}, false); !errors.Is(err, ErrExists) {
		t.Fatalf("dup: %v", err)
	}
	if err := m.Remove(ctx, "nas"); err != nil {
		t.Fatal(err)
	}
	fs, _ = os.ReadFile(filepath.Join(dir, "fstab"))
	if string(fs) != "UUID=abc / ext4 defaults 0 1\n" || !strings.Contains(strings.Join(r.Calls, "\n"), "umount "+mp) {
		t.Fatalf("after remove fstab %q", fs)
	}
	if len(m.All()) != 0 {
		t.Fatal("still listed")
	}
	// a share we can't write to
	r2 := &testfake.Runner{Fail: map[string]string{"mkdir": "Permission denied"}}
	m2, _ := newMgr(t, r2)
	if _, err := m2.Add(ctx, Storage{Name: "ro", Type: NFS, Server: "nas", Export: "/x"}, false); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("read-only share: %v", err)
	}
	r3 := &testfake.Runner{Fail: map[string]string{"mount": "access denied by server"}}
	m3, _ := newMgr(t, r3)
	if _, err := m3.Add(ctx, Storage{Name: "x1", Type: NFS, Server: "nas", Export: "/x"}, false); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("mount fails: %v", err)
	}
}

func TestISCSI(t *testing.T) {
	ctx := context.Background()
	r := &testfake.Runner{
		Out: map[string]string{
			"iscsiadm -m discovery": "192.168.0.20:3260,1 iqn.2005-10.org.freenas.ctl:p8s\n192.168.0.20:3260,1 iqn.2005-10.org.freenas.ctl:other\n",
			"zpool list -H -p -o":   "107374182400\t53687091200\tONLINE\n",
			"blkid":                 "xfs\n",
		},
		Fail: map[string]string{"zpool import": "no such pool available"},
	}
	m, _ := newMgr(t, r)
	l, err := m.Discover(ctx, "192.168.0.20")
	if err != nil || len(l) != 2 || l[1] != "iqn.2005-10.org.freenas.ctl:p8s" {
		t.Fatalf("discover %v %v", l, err)
	}
	s := Storage{Name: "san", Type: ISCSI, Portal: "192.168.0.20", Target: "iqn.2005-10.org.freenas.ctl:p8s", LUN: 0, CHAPUser: "p8s", CHAPPassword: "secret123456"}
	dev := filepath.Join(m.ByPath, "ip-192.168.0.20:3260-iscsi-iqn.2005-10.org.freenas.ctl:p8s-lun-0")
	os.WriteFile(dev, nil, 0o600)
	// disk with other data: needs format
	if _, err := m.Add(ctx, s, false); !errors.Is(err, ErrConfirm) || !strings.Contains(err.Error(), "xfs") {
		t.Fatalf("no format: %v", err)
	}
	if !strings.Contains(strings.Join(r.Calls, "\n"), "--logout") {
		t.Fatal("should log out again")
	}
	r.Calls = nil
	st, err := m.Add(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(r.Calls, "\n")
	for _, w := range []string{
		"iscsiadm -m node -T iqn.2005-10.org.freenas.ctl:p8s -p 192.168.0.20:3260 -o update -n node.startup -v automatic",
		"-n node.session.auth.authmethod -v CHAP",
		"iscsiadm -m node -T iqn.2005-10.org.freenas.ctl:p8s -p 192.168.0.20:3260 --login",
		"zpool create -f -o cachefile=none -o ashift=12 -O compression=lz4 -O atime=off -m none p8s-san " + dev,
		"zfs create -o canmount=off p8s-san/vms",
	} {
		if !strings.Contains(calls, w) {
			t.Errorf("missing %q in\n%s", w, calls)
		}
	}
	if !st.Online || st.SizeBytes != 107374182400 || st.Path != "p8s-san/vms" || st.CHAPPassword != "" {
		t.Fatalf("status %+v", st)
	}
	if got := m.All(); got[0].CHAPPassword != "secret123456" { // kept for reconnecting
		t.Fatal("CHAP password not saved")
	}
	if p := m.VMPools(); p["san"] != "p8s-san/vms" {
		t.Fatalf("pools %v", p)
	}
	if _, err := m.Add(ctx, Storage{Name: "san2", Type: ISCSI, Portal: "192.168.0.20:3260", Target: s.Target}, true); !errors.Is(err, ErrExists) {
		t.Fatalf("same LUN twice: %v", err)
	}
	for _, bad := range []Storage{
		{Name: "s2", Type: ISCSI, Portal: "192.168.0.20:99999", Target: s.Target},
		{Name: "s2", Type: ISCSI, Portal: "192.168.0.20", Target: "not-an-iqn"},
		{Name: "s2", Type: ISCSI, Portal: "192.168.0.20", Target: s.Target, LUN: 300},
		{Name: "s2", Type: ISCSI, Portal: "192.168.0.20", Target: s.Target, CHAPUser: "x"},
	} {
		if _, err := m.Add(ctx, bad, true); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	r.Calls = nil
	if err := m.Remove(ctx, "san"); err != nil {
		t.Fatal(err)
	}
	if c := strings.Join(r.Calls, "\n"); !strings.Contains(c, "zpool export p8s-san") || !strings.Contains(c, "-o delete") {
		t.Fatalf("remove calls %s", c)
	}
}

func TestISCSITakeOver(t *testing.T) {
	r := &testfake.Runner{}
	m, _ := newMgr(t, r)
	s := Storage{Name: "san", Type: ISCSI, Portal: "10.0.0.9", Target: "iqn.2005-10.org.freenas.ctl:p8s", LUN: 1}
	os.WriteFile(m.Device(Storage{Portal: "10.0.0.9:3260", Target: s.Target, LUN: 1}), nil, 0o600)
	if _, err := m.Add(context.Background(), s, false); err != nil {
		t.Fatalf("existing pool should be taken over without format: %v", err)
	}
	if c := strings.Join(r.Calls, "\n"); strings.Contains(c, "zpool create") || !strings.Contains(c, "zpool import -d") {
		t.Fatalf("calls %s", c)
	}
}

func TestParseTargets(t *testing.T) {
	if l := parseTargets("garbage\n1.2.3.4:3260,1 iqn.2001-04.com.example:storage.disk1\n[::1]:3260,1 iqn.2001-04.com.example:storage.disk1\n"); len(l) != 1 {
		t.Fatalf("%v", l)
	}
}

func TestNFSVMDisks(t *testing.T) {
	ctx := context.Background()
	r := &testfake.Runner{Out: map[string]string{"timeout 10 stat -f": "900000 4096\n", "timeout 5 stat -f": "100000000 90000000 4096\n",
		"zpool list -H -p": "107374182400 100000000000 ONLINE\n"}, Fail: map[string]string{"zpool list -H p8s-nas": "no such pool"}}
	m, _ := newMgr(t, r)
	if _, err := m.Add(ctx, Storage{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "/x", VMDisks: true, SizeGB: 5}, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too small: %v", err)
	}
	// the share has only ~3 GB free
	if _, err := m.Add(ctx, Storage{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "/x", VMDisks: true, SizeGB: 100}, false); err == nil || !strings.Contains(err.Error(), "GB free") {
		t.Fatalf("no room: %v", err)
	}
	r.Out["timeout 10 stat -f"] = "90000000 4096\n"
	mp := m.MountPoint("nas")
	if _, err := m.Add(ctx, Storage{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "/x", VMDisks: true, SizeGB: 100}, false); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(r.Calls, "\n")
	file := mp + "/p8s/vmdisks-nas.zpool"
	for _, want := range []string{"mount -t nfs -o hard,timeo=600", "truncate -s 100G " + file,
		"zpool create -f -o cachefile=none -o ashift=12 -O compression=lz4 -O atime=off -m none p8s-nas " + file,
		"zfs create -o canmount=off p8s-nas/vms"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("no %q in\n%s", want, calls)
		}
	}
	if p := m.VMPools(); p["nas"] != "p8s-nas/vms" {
		t.Fatalf("pools %v", p)
	}
	os.WriteFile(m.Mounts, []byte("10.0.0.5:/x "+mp+" nfs4 rw 0 0\n"), 0o644)
	st := m.List(ctx)[0]
	if !st.Online || st.SizeBytes != 107374182400 || !strings.Contains(st.Uses, "VM disks") || !st.HoldsVMs() {
		t.Fatalf("status %+v", st)
	}
	if err := m.Remove(ctx, "nas"); err != nil {
		t.Fatal(err)
	}
	if calls = strings.Join(r.Calls, "\n"); !strings.Contains(calls, "zpool export p8s-nas") {
		t.Fatalf("pool not released:\n%s", calls)
	}

	// the file is on the share already (server reinstalled): taken over
	r2 := &testfake.Runner{Fail: map[string]string{"zpool list -H p8s-nas": "no such pool"}}
	m2, _ := newMgr(t, r2)
	os.MkdirAll(m2.PoolDir("nas"), 0o755)
	os.WriteFile(m2.PoolFile("nas"), []byte("zfs"), 0o600)
	if _, err := m2.Add(ctx, Storage{Name: "nas", Type: NFS, Server: "10.0.0.5", Export: "/x", VMDisks: true}, false); err != nil {
		t.Fatal(err)
	}
	if calls := strings.Join(r2.Calls, "\n"); !strings.Contains(calls, "zpool import -d "+m2.PoolDir("nas")+" -N -o cachefile=none p8s-nas") || strings.Contains(calls, "zpool create") {
		t.Fatalf("not taken over:\n%s", calls)
	}
}
