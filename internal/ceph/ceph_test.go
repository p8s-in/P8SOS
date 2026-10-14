package ceph

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/execx"
	"p8sos/internal/installer"
)

// fakeRun answers by the longest matching command prefix and can create files.
type fakeRun struct {
	mu    sync.Mutex
	calls []string
	out   map[string]string
	fail  map[string]string
	hook  func(name string, args []string)
}

func (f *fakeRun) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if f.hook != nil {
		f.hook(name, args)
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

func (f *fakeRun) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

const monDump = `{"epoch":2,"fsid":"11111111-2222-4333-8444-555555555555","mons":[
 {"rank":0,"name":"n1","public_addrs":{"addrvec":[{"type":"v2","addr":"192.168.1.11:3300","nonce":0},{"type":"v1","addr":"192.168.1.11:6789","nonce":0}]},"addr":"192.168.1.11:6789/0"},
 {"rank":1,"name":"n2","public_addrs":{"addrvec":[{"type":"v2","addr":"192.168.1.12:3300","nonce":0}]},"addr":"192.168.1.12:6789/0"}]}`

const statusJSON = `{"fsid":"11111111-2222-4333-8444-555555555555",
 "health":{"status":"HEALTH_WARN","checks":{"OSD_DOWN":{"severity":"HEALTH_WARN","summary":{"message":"1 osds down","count":1}},
   "MON_DOWN":{"severity":"HEALTH_ERR","summary":{"message":"1/3 mons down"}}}},
 "quorum_names":["n1","n2"],
 "monmap":{"epoch":3,"num_mons":3},
 "osdmap":{"epoch":20,"num_osds":3,"num_up_osds":2,"num_in_osds":3},
 "pgmap":{"pgs_by_state":[{"state_name":"active+clean","count":30},{"state_name":"active+undersized","count":3}],
   "num_pgs":33,"bytes_used":3000,"bytes_avail":7000,"bytes_total":10000},
 "mgrmap":{"available":true,"num_standbys":1}}`

const osdTree = `{"nodes":[{"id":-1,"name":"default","type":"root","children":[-3,-5]},
 {"id":-3,"name":"n1","type":"host","children":[0]},{"id":-5,"name":"n2","type":"host","children":[1]},
 {"id":0,"name":"osd.0","type":"osd","status":"up","reweight":1,"device_class":"ssd","crush_weight":0.45},
 {"id":1,"name":"osd.1","type":"osd","status":"down","reweight":0,"device_class":"hdd","crush_weight":0.9}],
 "stray":[{"id":7,"status":"down","reweight":0}]}`

const dfJSON = `{"pools":[{"name":".mgr","stats":{"stored":1}},{"name":"p8s-vms","stats":{"stored":5000,"max_avail":90000,"percent_used":0.05}}]}`

func TestParsers(t *testing.T) {
	mons, fsid, err := ParseMonDump([]byte(monDump))
	if err != nil || fsid != "11111111-2222-4333-8444-555555555555" || len(mons) != 2 || mons[0] != (Mon{"n1", "192.168.1.11"}) || mons[1].IP != "192.168.1.12" {
		t.Fatalf("mons %v %s %v", mons, fsid, err)
	}
	st, err := ParseStatus([]byte(statusJSON))
	if err != nil {
		t.Fatal(err)
	}
	if st.Health != "HEALTH_WARN" || st.Mons != 3 || len(st.Quorum) != 2 || !st.MgrUp || st.MgrSpare != 1 ||
		st.OSDCount != 3 || st.OSDUp != 2 || st.PGs != 33 || st.SizeBytes != 10000 {
		t.Fatalf("status %+v", st)
	}
	if strings.Join(st.Problems, "|") != "error: 1/3 mons down|warning: 1 osds down" {
		t.Fatalf("problems %q", st.Problems)
	}
	osds := ParseOSDTree([]byte(osdTree))
	if len(osds) != 3 || osds[0] != (OSD{0, "n1", true, true, "ssd", 0.45}) || osds[1].Host != "n2" || osds[1].Up || osds[1].In || osds[2].ID != 7 {
		t.Fatalf("osds %+v", osds)
	}
	pools := ParseDF([]byte(dfJSON))
	if len(pools) != 2 || pools[1].Name != Pool || pools[1].MaxAvail != 90000 {
		t.Fatalf("df %+v", pools)
	}
	addrs := ParseAddrs([]byte(`[{"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},
	 {"ifname":"vmbr0","addr_info":[{"family":"inet","local":"192.168.1.11","prefixlen":24}]},
	 {"ifname":"p8s-wg","addr_info":[{"family":"inet","local":"10.250.0.1","prefixlen":24}]},
	 {"ifname":"virbr0","addr_info":[{"family":"inet","local":"192.168.122.1","prefixlen":24}]}]`))
	if len(addrs) != 1 || addrs[0].String() != "192.168.1.11/24" {
		t.Fatalf("addrs %v", addrs)
	}
}

func TestConf(t *testing.T) {
	n := netip.MustParsePrefix("192.168.1.0/24")
	c := Conf("abc", n, []Mon{{"n2", "192.168.1.12"}, {"n1", "192.168.1.11"}})
	for _, want := range []string{"fsid = abc\n", "mon_initial_members = n1,n2\n",
		"mon_host = [v2:192.168.1.11:3300,v1:192.168.1.11:6789] [v2:192.168.1.12:3300,v1:192.168.1.12:6789]\n",
		"public_network = 192.168.1.0/24\n", "ms_service_mode = secure\n", "auth_client_required = cephx\n"} {
		if !strings.Contains(c, want) {
			t.Errorf("conf misses %q:\n%s", want, c)
		}
	}
	if got, err := NetworkOf(c); err != nil || got != n {
		t.Fatalf("network %v %v", got, err)
	}
	if FSIDOf(c) != "abc" {
		t.Fatal("fsid")
	}
	if _, err := NetworkOf("[global]\n"); err == nil {
		t.Fatal("no network accepted")
	}
}

func newMgr(t *testing.T, r *fakeRun) *Manager {
	t.Helper()
	d := t.TempDir()
	return &Manager{Run: r, Etc: filepath.Join(d, "etc"), Lib: filepath.Join(d, "lib"), Name: "n1",
		Exists: func(p string) bool {
			if strings.HasPrefix(p, "/usr/") {
				return true // Ceph installed
			}
			_, err := os.Stat(p)
			return err == nil
		},
		Addrs: func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("192.168.1.11/24")} },
		Disks: func(context.Context) ([]installer.Disk, error) {
			return []installer.Disk{
				{Name: "sda", Path: "/dev/sda", Size: 1e12, Parts: 2, Contents: []string{"ntfs"}},
				{Name: "sdb", Path: "/dev/sdb", Size: 5e11, Contents: []string{}},
				{Name: "sdc", Path: "/dev/sdc", Size: 5e11, Contents: []string{}, Removable: true},
			}, nil
		},
		Settle: 50 * time.Millisecond, Tick: time.Millisecond}
}

// keyrings makes ceph-authtool --create-keyring write a file, like the real one.
func keyrings(name string, args []string) {
	if name == "ceph-authtool" && len(args) > 1 && args[0] == "--create-keyring" {
		_ = os.MkdirAll(filepath.Dir(args[1]), 0o755)
		_ = os.WriteFile(args[1], []byte("[client.x]\n\tkey = QUJDRA==\n"), 0o600)
	}
	if name == "install" && len(args) > 0 && args[0] == "-d" {
		_ = os.MkdirAll(args[len(args)-1], 0o755)
	}
}

func TestInit(t *testing.T) {
	r := &fakeRun{hook: keyrings, out: map[string]string{}}
	m := newMgr(t, r)
	if _, _, err := m.CheckInit("10.0.0.0/8"); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("no address in the network: %v", err)
	}
	if _, _, err := m.CheckInit("192.168.1.0"); err == nil {
		t.Fatal("network without size accepted")
	}
	if err := m.Init(context.Background(), "192.168.1.5/24"); err != nil {
		t.Fatal(err)
	}
	all := r.all()
	for _, want := range []string{
		"ceph-authtool --create-keyring " + m.AdminKeyring() + " --gen-key -n client.admin",
		"--import-keyring " + m.BootstrapKeyring(),
		"monmaptool --create --addv n1 [v2:192.168.1.11:3300,v1:192.168.1.11:6789] --fsid ",
		"ceph-mon --cluster ceph --mkfs -i n1",
		"systemctl enable --now ceph-mon@n1",
		"ceph --connect-timeout 20 mon enable-msgr2",
		"ceph --connect-timeout 20 auth get-or-create mgr.n1",
		"systemctl enable --now ceph-mgr@n1",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	conf, _ := os.ReadFile(m.ConfFile())
	if !strings.Contains(string(conf), "public_network = 192.168.1.0/24") || !strings.Contains(string(conf), "mon_host = [v2:192.168.1.11:3300") {
		t.Fatalf("conf:\n%s", conf)
	}
	if !m.Configured() {
		t.Fatal("not configured after init")
	}
	if _, _, err := m.CheckInit("192.168.1.0/24"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second init: %v", err)
	}
	b, err := m.Bundle()
	if err != nil || b.Hash() == "" {
		t.Fatalf("bundle %v", err)
	}
}

func TestInitMonitorFails(t *testing.T) {
	r := &fakeRun{hook: keyrings, fail: map[string]string{"ceph-mon": "mkfs failed"}}
	m := newMgr(t, r)
	if err := m.Init(context.Background(), "192.168.1.0/24"); err == nil {
		t.Fatal("init succeeded")
	}
	if m.Configured() {
		t.Fatal("a failed init left the configuration behind")
	}
}

func TestBundleAndDisks(t *testing.T) {
	r := &fakeRun{}
	m := newMgr(t, r)
	key := "[client.admin]\n\tkey = QUJDRA==\n"
	if err := m.SetBundle(Bundle{Conf: "nothing", AdminKeyring: key, BootstrapOSD: key}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad conf accepted: %v", err)
	}
	conf := Conf("abc", netip.MustParsePrefix("192.168.1.0/24"), []Mon{{"n1", "192.168.1.11"}})
	if err := m.SetBundle(Bundle{Conf: conf, AdminKeyring: "rm -rf /", BootstrapOSD: key}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad keyring accepted: %v", err)
	}
	b := Bundle{Conf: conf, AdminKeyring: key, BootstrapOSD: "[client.bootstrap-osd]\n\tkey = QUJDRA==\n"}
	if err := m.SetBundle(b); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Bundle()
	if got != b {
		t.Fatalf("bundle %+v", got)
	}
	if fi, _ := os.Stat(m.AdminKeyring()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("admin key mode %v", fi.Mode())
	}
	in := m.Info(context.Background())
	if !in.Installed || !in.Configured || in.Hash != b.Hash() || len(in.Disks) != 1 || in.Disks[0].Name != "sdb" {
		t.Fatalf("info %+v", in)
	}
	if err := m.CheckOSD(context.Background(), "/dev/sda"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("disk with data accepted: %v", err)
	}
	if err := m.CheckOSD(context.Background(), "/dev/sdc"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("USB disk accepted: %v", err)
	}
	if err := m.CreateOSD(context.Background(), "sdb"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.all(), "ceph-volume lvm create --data /dev/sdb") {
		t.Fatal(r.all())
	}
}

func TestRemoveOSDAndPool(t *testing.T) {
	r := &fakeRun{fail: map[string]string{"ceph --connect-timeout 20 osd safe-to-destroy": "not safe"}}
	m := newMgr(t, r)
	key := "[client.admin]\n\tkey = QUJDRA==\n"
	_ = m.SetBundle(Bundle{Conf: Conf("abc", netip.MustParsePrefix("192.168.1.0/24"), []Mon{{"n1", "192.168.1.11"}}), AdminKeyring: key, BootstrapOSD: key})
	done, err := m.RemoveOSD(context.Background(), 3)
	if err != nil || done || !strings.Contains(r.all(), "osd out 3") || strings.Contains(r.all(), "purge") {
		t.Fatalf("not safe: done=%v err=%v\n%s", done, err, r.all())
	}
	r.fail = nil
	done, err = m.RemoveOSD(context.Background(), 3)
	if err != nil || !done || !strings.Contains(r.all(), "osd purge 3 --yes-i-really-mean-it") || !strings.Contains(r.all(), "ceph-volume lvm zap --osd-id 3 --destroy") {
		t.Fatalf("safe: %v %v\n%s", done, err, r.all())
	}

	r.calls = nil
	if err := m.CreatePool(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	all := r.all()
	for _, want := range []string{"mon_allow_pool_size_one true", "osd pool create p8s-vms 32", "pool set p8s-vms size 1", "min_size 1",
		"application enable p8s-vms rbd", "rbd pool init p8s-vms"} {
		if !strings.Contains(all, want) {
			t.Errorf("pool: missing %q", want)
		}
	}
	r.out = map[string]string{"ceph --connect-timeout 20 osd pool ls": ".mgr\np8s-vms\n"}
	if err := m.CreatePool(context.Background(), 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("second pool: %v", err)
	}
	if !m.PoolReady(context.Background()) {
		t.Fatal("pool not ready")
	}
}

func TestRefreshConf(t *testing.T) {
	r := &fakeRun{out: map[string]string{"ceph --connect-timeout 20 mon dump": monDump}}
	m := newMgr(t, r)
	key := "[client.admin]\n\tkey = QUJDRA==\n"
	_ = m.SetBundle(Bundle{Conf: Conf("11111111-2222-4333-8444-555555555555", netip.MustParsePrefix("192.168.1.0/24"),
		[]Mon{{"n1", "192.168.1.11"}}), AdminKeyring: key, BootstrapOSD: key})
	changed, err := m.RefreshConf(context.Background())
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	c, _ := os.ReadFile(m.ConfFile())
	if !strings.Contains(string(c), "192.168.1.12:3300") {
		t.Fatalf("second monitor missing:\n%s", c)
	}
	if changed, _ := m.RefreshConf(context.Background()); changed {
		t.Fatal("changed twice")
	}
}

type fakePeers struct {
	mu    sync.Mutex
	peers []Peer
	infos map[string]NodeInfo
	calls []string
	got   map[string]Bundle
}

func (f *fakePeers) List() []Peer { return f.peers }

func (f *fakePeers) Call(_ context.Context, id, method, path string, in, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id+" "+method+" "+path)
	switch {
	case path == "/ceph/node" && method == "GET":
		*(out.(*NodeInfo)) = f.infos[id]
	case path == "/ceph/node/config":
		f.got[id] = in.(Bundle)
	}
	return nil
}

func TestClusterOverviewAndRun(t *testing.T) {
	r := &fakeRun{out: map[string]string{
		"ceph --connect-timeout 20 status": statusJSON, "ceph --connect-timeout 20 osd tree": osdTree,
		"ceph --connect-timeout 20 df": dfJSON, "ceph --connect-timeout 20 mon dump": monDump}}
	m := newMgr(t, r)
	p := &fakePeers{peers: []Peer{{ID: "n1", Name: "n1", Self: true, Online: true}, {ID: "n2", Name: "n2", Online: true},
		{ID: "n3", Name: "n3", Online: false}, {ID: "n4", Name: "n4", Online: true}},
		infos: map[string]NodeInfo{"n2": {Name: "n2", Installed: true, Hash: "old"}, "n4": {Name: "n4", Installed: false}},
		got:   map[string]Bundle{}}
	c := &Cluster{Local: m, Peers: p}

	o := c.Overview(context.Background())
	if o.Configured || len(o.Nodes) != 4 || o.Status != nil {
		t.Fatalf("before setup %+v", o)
	}
	if err := c.Run(context.Background(), "n2", ActInit, NodeRequest{Network: "192.168.1.0/24"}); err == nil {
		t.Fatal("init on a worker accepted")
	}
	if err := c.Run(context.Background(), "n2", ActMon, NodeRequest{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("monitor before setup: %v", err)
	}
	if err := c.Run(context.Background(), "n2", ActInstall, NodeRequest{}); err != nil || p.calls[len(p.calls)-1] != "n2 POST /ceph/node/install" {
		t.Fatalf("install on n2: %v %v", err, p.calls)
	}

	key := "[client.admin]\n\tkey = QUJDRA==\n"
	_ = m.SetBundle(Bundle{Conf: Conf("11111111-2222-4333-8444-555555555555", netip.MustParsePrefix("192.168.1.0/24"),
		[]Mon{{"n1", "192.168.1.11"}}), AdminKeyring: key, BootstrapOSD: key})
	o = c.Overview(context.Background())
	if !o.Configured || o.Status == nil || !o.PoolReady || o.Network != "192.168.1.0/24" {
		t.Fatalf("overview %+v", o)
	}
	if len(o.Synced) != 1 || o.Synced[0] != "n2" {
		t.Fatalf("synced %v (n4 has no Ceph, n3 is offline)", o.Synced)
	}
	if b := p.got["n2"]; !strings.Contains(b.Conf, "192.168.1.12") {
		t.Fatal("n2 got a configuration without the second monitor")
	}
	if err := c.Run(context.Background(), "n3", ActMon, NodeRequest{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("offline server: %v", err)
	}
	if err := c.Run(context.Background(), "n2", ActOSD, NodeRequest{Disk: "sdb"}); err != nil || p.calls[len(p.calls)-1] != "n2 POST /ceph/node/osd" {
		t.Fatalf("osd on n2: %v %v", err, p.calls)
	}
	if len(o.Advice()) == 0 {
		t.Fatal("no advice with 1 monitor")
	}
	// the local step runs in the background; a second one waits its turn
	if err := c.Run(context.Background(), "n1", ActOSD, NodeRequest{Disk: "/dev/sdb"}); err != nil {
		t.Fatal(err)
	}
	if tk := m.Wait(); tk.Kind != ActOSD || tk.Error != "" {
		t.Fatalf("task %+v", tk)
	}
	if err := c.Run(context.Background(), "", ActOSD, NodeRequest{Disk: "/dev/sda"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("disk with data: %v", err)
	}
	size, err := c.CreatePool(context.Background(), 0)
	if err != nil || size != 1 { // only n1's osd.0 is up and in
		t.Fatalf("pool size %d %v", size, err)
	}
	if _, err := c.CreatePool(context.Background(), 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("3 copies on 1 server: %v", err)
	}
}

func TestBusy(t *testing.T) {
	m := newMgr(t, &fakeRun{})
	block := make(chan struct{})
	if err := m.Start("x", "", func(context.Context) error { <-block; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("y", "", func(context.Context) error { return nil }); !errors.Is(err, ErrBusy) {
		t.Fatalf("second task: %v", err)
	}
	close(block)
	m.Wait()
	if err := m.Do("bogus", NodeRequest{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if !strings.Contains(NewFSID(), "-4") {
		t.Fatal("fsid isn't a v4 uuid")
	}
}

// Install runs apt outside p8sd's unit (systemd-run), finishes an earlier
// broken try first and says which package failed.
func TestInstallRunsAptOutsideP8sd(t *testing.T) {
	f := &fakeRun{fail: map[string]string{"systemd-run --wait --pipe --quiet --collect --setenv=DEBIAN_FRONTEND=noninteractive --setenv=HOME=/root -- apt-get install -y -q -o": "E: Sub-process /usr/bin/dpkg returned an error code (1)"}}
	m := &Manager{Run: f, Etc: t.TempDir(), Lib: t.TempDir(), Exists: func(string) bool { return false }}
	err := m.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dpkg returned an error code") {
		t.Fatalf("err = %v", err)
	}
	all := f.all()
	for _, want := range []string{"-- dpkg --configure -a", "-- apt-get update -qq", "-- apt-get install -f -y -q", "ceph-mon ceph-mgr"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "\nenv ") || strings.HasPrefix(all, "env ") {
		t.Errorf("apt ran inside p8sd:\n%s", all)
	}
	if got := aptErrors([]byte("Setting up foo\ndpkg: error processing package ceph-osd (--configure):\n installed ceph-osd package post-installation script subprocess returned error exit status 1\nok\n")); !strings.Contains(got, "ceph-osd (--configure)") || strings.Contains(got, "Setting up") {
		t.Errorf("aptErrors = %q", got)
	}
}

// The monitor's first files live where the ceph user can read them
// (not in p8sd's private, root-only /tmp).
func TestMonFilesReadableByCeph(t *testing.T) {
	lib := t.TempDir()
	m := &Manager{Lib: lib}
	d, err := m.workDir()
	if err != nil || !strings.HasPrefix(d, filepath.Join(lib, "tmp")+string(filepath.Separator)) {
		t.Fatalf("workDir = %q, %v", d, err)
	}
}
