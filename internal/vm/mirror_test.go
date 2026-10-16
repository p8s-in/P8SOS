package vm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"p8sos/internal/storage"
)

// mirDisks: repDisks that can keep a mirror (the copy is the mirrored data).
type mirDisks struct {
	*repDisks
	good     map[string][]byte // "last good" snapshot
	served   map[string]int    // vm -> port
	hidden   map[string]bool
	prepared int
}

func (d *mirDisks) DiskBytes(context.Context, string, int) (int64, error) { return 10 << 30, nil }
func (d *mirDisks) PrepareMirror(_ context.Context, vm string, disks []storage.MirrorDisk) ([]string, error) {
	d.prepared++
	if c, ok := d.rep[vm]; ok {
		d.good[vm] = append([]byte(nil), c...)
	} else {
		d.rep[vm] = []byte{}
	}
	d.hidden[vm] = false
	return []string{"/dev/zvol/tank/vms/replica/" + vm + "-disk0"}, nil
}
func (d *mirDisks) MirrorSynced(_ context.Context, vm string) { delete(d.good, vm) }
func (d *mirDisks) MirrorRollback(_ context.Context, vm string) (bool, error) {
	g, ok := d.good[vm]
	if ok {
		d.rep[vm] = g
		delete(d.good, vm)
	}
	return ok, nil
}
func (d *mirDisks) StartMirrorExport(_ context.Context, vm, host string, port int, _ []storage.MirrorDisk, _ []string) error {
	d.served[vm] = port
	return nil
}
func (d *mirDisks) StopMirrorExport(_ context.Context, vm string) { delete(d.served, vm) }
func (d *mirDisks) MirrorExportUp(_ context.Context, vm string) bool {
	_, ok := d.served[vm]
	return ok
}
func (d *mirDisks) HideMirror(_ context.Context, vm string) { d.hidden[vm] = true }

func TestMirrorTargetSide(t *testing.T) {
	ctx := context.Background()
	b, _, dB, _ := newSvc()
	md := &mirDisks{repDisks: newRep(&memDisks{fakeDisks: dB, data: map[string][]byte{}}), good: map[string][]byte{}, served: map[string]int{}, hidden: map[string]bool{}}
	b.Disks, b.ReplicaDir = md, t.TempDir()
	b.NVRAMDir, b.TPMDir = t.TempDir(), t.TempDir()
	plan := MirrorPlan{Header: MoveHeader{Name: "db", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10, MAC: "52:54:00:12:34:56", Network: NetNAT},
		Disks: []storage.MirrorDisk{{Index: 0, Bytes: 10 << 30}}}
	if _, err := b.MirrorPrepare(ctx, plan, "srv-a", "192.168.0.5"); err == nil {
		t.Fatal("served outside the cluster tunnel")
	}
	tg, err := b.MirrorPrepare(ctx, plan, "srv-a", "10.250.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Host != "10.250.0.2" || tg.Port != MirrorPortBase || len(tg.Disks) != 1 || md.served["p8s-db"] != MirrorPortBase {
		t.Fatalf("%+v %+v", tg, md.served)
	}
	// the same VM keeps its port; another VM gets the next one
	if tg2, _ := b.MirrorPrepare(ctx, plan, "srv-a", "10.250.0.2"); tg2.Port != MirrorPortBase {
		t.Fatalf("port changed: %d", tg2.Port)
	}
	p2 := plan
	p2.Header.Name = "web"
	if tg3, _ := b.MirrorPrepare(ctx, p2, "srv-a", "10.250.0.2"); tg3.Port != MirrorPortBase+1 {
		t.Fatalf("second VM port %d", tg3.Port)
	}
	// the mirror caught up once, then a new sync started and was cut short
	md.rep["p8s-db"] = []byte("complete copy")
	if err := b.MirrorSynced(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.MirrorPrepare(ctx, plan, "srv-a", "10.250.0.2"); err != nil { // VM restarted: a new first sync
		t.Fatal(err)
	}
	md.rep["p8s-db"] = []byte("half new, half old")
	// the VM's server dies: the copy goes back to its last complete state and is used
	v, note, err := b.PromoteReplica(ctx, "db")
	if err != nil {
		t.Fatal(err)
	}
	if string(md.data["p8s-db"]) != "complete copy" || !strings.Contains(note, "last complete state") || v.Name != "db" {
		t.Fatalf("%q %q", md.data["p8s-db"], note)
	}
	if _, ok := md.served["p8s-db"]; ok {
		t.Fatal("still served after promote")
	}
	// a VM running here can't keep its own mirror
	if _, err := b.MirrorPrepare(ctx, plan, "srv-a", "10.250.0.2"); !errors.Is(err, ErrExists) {
		t.Fatalf("mirror of a VM running here: %v", err)
	}
	// deleting the other copy stops serving it
	if err := b.DropReplica(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, ok := md.served["p8s-web"]; ok {
		t.Fatal("still served after delete")
	}
}
