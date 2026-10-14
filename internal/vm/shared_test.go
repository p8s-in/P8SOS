package vm

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"p8sos/internal/virt"
)

// cephPool is the pool both fake servers see: image -> servers that have it open.
type cephPool struct{ imgs map[string]map[string]bool }

// cephDisks is one server's storage: ZFS fakes plus the shared pool.
type cephDisks struct {
	*repDisks
	server  string
	pool    *cephPool
	placed  map[string]bool
	created []string
}

func newCeph(server string, pool *cephPool, d *fakeDisks) *cephDisks {
	return &cephDisks{repDisks: newRep(&memDisks{fakeDisks: d, data: map[string][]byte{}}), server: server, pool: pool, placed: map[string]bool{}}
}

func (c *cephDisks) img(vm string, i int) string { return fmt.Sprintf("%s-disk%d", vm, i) }
func (c *cephDisks) dev(vm string, i int) string { return "/dev/rbd/p8s-vms/" + c.img(vm, i) }

func (c *cephDisks) Shared(vm string) bool {
	_, ok := c.pool.imgs[c.img(vm, 0)]
	return c.placed[vm] || ok
}
func (c *cephDisks) mapped(vm string) bool { return c.pool.imgs[c.img(vm, 0)][c.server] }
func (c *cephDisks) Release(_ context.Context, vm string) error {
	for img, who := range c.pool.imgs {
		if strings.HasPrefix(img, vm+"-disk") {
			delete(who, c.server)
		}
	}
	return nil
}
func (c *cephDisks) open(vm string, i int) string {
	if c.pool.imgs[c.img(vm, i)] == nil {
		c.pool.imgs[c.img(vm, i)] = map[string]bool{}
	}
	c.pool.imgs[c.img(vm, i)][c.server] = true
	return c.dev(vm, i)
}
func (c *cephDisks) Place(vm, st string) error {
	c.placed[vm] = st == "ceph"
	return nil
}
func (c *cephDisks) StorageOf(vm string) string {
	if c.Shared(vm) {
		return "ceph"
	}
	return "local"
}
func (c *cephDisks) Storages() []string { return []string{"local", "ceph"} }
func (c *cephDisks) WaitDevice(_ context.Context, vm string) (string, error) {
	if _, ok := c.pool.imgs[c.img(vm, 0)]; !ok && c.Shared(vm) {
		return "", fmt.Errorf("no image %s", c.img(vm, 0))
	}
	return c.open(vm, 0), nil
}
func (c *cephDisks) WaitExtra(_ context.Context, vm string, i int) (string, error) {
	return c.open(vm, i), nil
}
func (c *cephDisks) CloneDisk(ctx context.Context, snap, vm string, gb int) (string, error) {
	if !c.placed[vm] {
		return c.repDisks.CloneDisk(ctx, snap, vm, gb)
	}
	if _, ok := c.pool.imgs[c.img(vm, 0)]; ok {
		return "", fmt.Errorf("a Ceph disk %s already exists", c.img(vm, 0))
	}
	c.created = append(c.created, vm)
	return c.open(vm, 0), nil
}
func (c *cephDisks) DestroyDisk(ctx context.Context, vm string) error {
	if !c.Shared(vm) {
		return c.repDisks.DestroyDisk(ctx, vm)
	}
	_ = c.Release(ctx, vm)
	for img, who := range c.pool.imgs {
		if strings.HasPrefix(img, vm+"-disk") && len(who) == 0 {
			delete(c.pool.imgs, img)
		}
	}
	delete(c.placed, vm)
	return nil
}
func (c *cephDisks) DiskBytes(context.Context, string, int) (int64, error) { return 10 << 30, nil }
func (c *cephDisks) CreateSized(_ context.Context, vm string, i int, b int64) (string, error) {
	return "", fmt.Errorf("CreateSized(%s, %d) on shared storage", vm, i)
}

func cephPair(t *testing.T) (a, b *Service, ha, hb *liveHVFake, ca, cb *cephDisks, pool *cephPool) {
	t.Helper()
	pool = &cephPool{imgs: map[string]map[string]bool{}}
	a, hvA, dA, _ := newSvc()
	b, hvB, dB, _ := newSvc()
	ca, cb = newCeph("a", pool, dA), newCeph("b", pool, dB)
	ha, hb = &liveHVFake{fakeHV: hvA}, &liveHVFake{fakeHV: hvB}
	a.HV, b.HV, a.Disks, b.Disks = ha, hb, ca, cb
	a.ReplicaDir, b.ReplicaDir = t.TempDir(), t.TempDir()
	a.NVRAMDir, b.NVRAMDir = t.TempDir(), t.TempDir()
	a.TPMDir, b.TPMDir = t.TempDir(), t.TempDir()
	return
}

func TestSharedMove(t *testing.T) {
	ctx := context.Background()
	a, b, _, hb, ca, cb, pool := cephPair(t)
	v, err := a.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10, Storage: "ceph"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Storage != "ceph" || !ca.mapped("p8s-web1") {
		t.Fatalf("created %+v", v)
	}
	a.Stop(ctx, "web1", true)
	if why, _ := a.MoveCheck(ctx, "web1"); why != "" {
		t.Fatalf("move check %q", why)
	}
	var buf bytes.Buffer
	if err := a.ExportVM(ctx, "web1", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"shared":"ceph"`) || strings.Count(buf.String(), "\n") != 1 || !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("stream must be the header alone: %.200q", buf.String())
	}
	got, _, err := b.ImportMovedVM(ctx, &buf)
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := virt.ParseDomainXML(hb.doms["p8s-web1"])
	if got.Storage != "ceph" || sp.DiskPath != "/dev/rbd/p8s-vms/p8s-web1-disk0" || !cb.mapped("p8s-web1") {
		t.Fatalf("imported %+v disk %s", got, sp.DiskPath)
	}
	// the cluster deletes the original: the disk stays, B has it open
	if err := a.Delete(ctx, "web1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.imgs["p8s-web1-disk0"]; !ok || ca.mapped("p8s-web1") {
		t.Fatalf("pool %v", pool.imgs)
	}
	// a VM of the same name can't take over the disk
	if _, err := a.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", VCPUs: 1, MemoryMiB: 512, DiskGB: 10, Storage: "ceph"}); err == nil {
		t.Fatal("second web1 created on top of the first one's disk")
	}
	if !cb.mapped("p8s-web1") {
		t.Fatal("the failed create touched the disk")
	}
	if err := b.Delete(ctx, "web1"); err != nil {
		t.Fatal(err)
	}
	if len(pool.imgs) != 0 {
		t.Fatalf("not deleted: %v", pool.imgs)
	}
}

func TestSharedLiveMove(t *testing.T) {
	ctx := context.Background()
	a, b, ha, hb, ca, cb, pool := cephPair(t)
	if _, err := a.Create(ctx, CreateRequest{Name: "db", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10, Storage: "ceph"}); err != nil {
		t.Fatal(err)
	}
	if why, _ := a.LiveCheck(ctx, "db"); why != "" {
		t.Fatalf("live check %q", why)
	}
	h, err := a.LiveHeader(ctx, "db")
	if err != nil || h.Shared != "ceph" {
		t.Fatalf("header %+v %v", h.MoveHeader, err)
	}
	plan, err := b.LivePrepare(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.XML, "/dev/rbd/p8s-vms/p8s-db-disk0") || !cb.mapped("p8s-db") || !ca.mapped("p8s-db") {
		t.Fatalf("plan %s", plan.XML)
	}
	if err := a.LiveSend(ctx, "db", "10.250.0.2", plan, func(int) {}); err != nil {
		t.Fatal(err)
	}
	hb.doms["p8s-db"] = []byte(ha.sent[0][strings.Index(ha.sent[0], ":")+1:]) // libvirt defines it on B
	hb.state["p8s-db"] = virt.Running
	if err := a.LiveCleanup(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if ca.mapped("p8s-db") || !cb.mapped("p8s-db") || len(pool.imgs) != 1 {
		t.Fatalf("after: %v", pool.imgs)
	}
	// a failed move on the target only closes the disk there
	h.Name = "db"
	if err := b.LiveAbort(ctx, "db"); err == nil {
		t.Fatal("abort removed a running VM")
	}
	_ = ha
}

func TestSharedFailover(t *testing.T) {
	ctx := context.Background()
	a, b, _, hb, ca, cb, pool := cephPair(t)
	if _, err := a.Create(ctx, CreateRequest{Name: "app", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10, Storage: "ceph"}); err != nil {
		t.Fatal(err)
	}
	if why, _ := a.HACheck(ctx, "app"); why != "" {
		t.Fatalf("HA check %q", why)
	}
	var buf bytes.Buffer
	if err := a.SendReplica(ctx, "app", nil, "server-a", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), "\n") != 1 || !strings.Contains(buf.String(), `"snap":"p8s-rep-`) {
		t.Fatalf("copy must be the header alone: %.200q", buf.String())
	}
	r, err := b.RecvReplica(ctx, &buf)
	if err != nil || r.At == "" || r.Bytes != 0 {
		t.Fatalf("recv %+v %v", r, err)
	}
	if l := b.Replicas(ctx); len(l) != 1 {
		t.Fatalf("replicas %+v", l)
	}
	if snaps, _ := b.ReplicaSnaps(ctx, "app"); len(snaps) != 1 {
		t.Fatalf("snaps %v", snaps)
	}
	// A dies: B starts the VM from the same disk
	v, note, err := b.PromoteReplica(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := virt.ParseDomainXML(hb.doms["p8s-app"])
	if v.Name != "app" || sp.DiskPath != "/dev/rbd/p8s-vms/p8s-app-disk0" || !cb.mapped("p8s-app") || !strings.Contains(note, "Ceph") {
		t.Fatalf("promoted %+v %q", v, note)
	}
	// A comes back: its old definition goes, the disk stays
	if err := a.RetireVM(ctx, "app", "server-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(ctx, "app"); err == nil || ca.mapped("p8s-app") || len(pool.imgs) != 1 {
		t.Fatalf("retire: %v %v", err, pool.imgs)
	}
	if l := a.Replicas(ctx); len(l) != 1 {
		t.Fatalf("A keeps the description as the copy: %+v", l)
	}
	if err := a.DropReplica(ctx, "app"); err != nil || len(a.Replicas(ctx)) != 0 || len(pool.imgs) != 1 {
		t.Fatalf("drop %v %v", err, pool.imgs)
	}
}

func TestMigrateArgsShared(t *testing.T) {
	a := strings.Join(virt.MigrateArgs("qemu:///system", "p8s-x", "10.250.0.2", "l.xml", "p.xml", true), " ")
	if strings.Contains(a, "copy-storage") || strings.Contains(a, "disks-uri") || !strings.Contains(a, "--migrateuri tcp://10.250.0.2") {
		t.Fatal(a)
	}
	a = strings.Join(virt.MigrateArgs("qemu:///system", "p8s-x", "10.250.0.2", "l.xml", "p.xml", false), " ")
	if !strings.Contains(a, "--copy-storage-all") || !strings.Contains(a, "--disks-uri tcp://10.250.0.2") {
		t.Fatal(a)
	}
	rbd := `<domain><devices><disk type="block" device="disk"><source dev="/dev/rbd/p8s-vms/p8s-x-disk0"/></disk>
	  <disk type="file" device="cdrom"><source file="/var/lib/p8s/seeds/x.iso"/></disk></devices></domain>`
	mixed := strings.Replace(rbd, `<disk type="file"`, `<disk type="block" device="disk"><source dev="/dev/zvol/tank/x"/></disk><disk type="file"`, 1)
	if !virt.SharedDisks([]byte(rbd)) || virt.SharedDisks([]byte(mixed)) || virt.SharedDisks([]byte(`<domain/>`)) {
		t.Fatal("SharedDisks")
	}
}
