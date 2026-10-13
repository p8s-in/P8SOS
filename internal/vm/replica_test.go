package vm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"p8sos/internal/virt"
)

// repDisks: memDisks that can keep copies. A "send" is base + "|" + data.
type repDisks struct {
	*memDisks
	n        int
	snaps    map[string][]string // disk snapshots (ours)
	rep      map[string][]byte
	repSnaps map[string][]string
}

func newRep(m *memDisks) *repDisks {
	return &repDisks{memDisks: m, snaps: map[string][]string{}, rep: map[string][]byte{}, repSnaps: map[string][]string{}}
}

func (d *repDisks) SendReplica(_ context.Context, vm, base string, name func(string) error, w io.Writer) error {
	var keep []string
	for _, s := range d.snaps[vm] {
		if s == base {
			keep = append(keep, s)
		}
	}
	d.n++
	snap := fmt.Sprintf("p8s-rep-%d", 1791900000000000000+d.n)
	d.snaps[vm] = append(keep, snap)
	if err := name(snap); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s|%s", base, d.data[vm])
	return err
}
func (d *repDisks) RecvReplica(_ context.Context, vm, base string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	got, data, _ := strings.Cut(string(b), "|")
	if got != base {
		return fmt.Errorf("base %q in stream, %q in header", got, base)
	}
	if base != "" {
		ok := false
		for _, s := range d.repSnaps[vm] {
			ok = ok || s == base
		}
		if !ok {
			return fmt.Errorf("no base %s", base)
		}
	}
	d.rep[vm] = []byte(data)
	// the header line said which snapshot this is; the fake takes it from the sender's list
	d.repSnaps[vm] = []string{pendingSnap}
	return nil
}

var pendingSnap string

func (d *repDisks) ReplicaSnaps(_ context.Context, vm string) ([]string, error) {
	return d.repSnaps[vm], nil
}
func (d *repDisks) DiskRepSnaps(_ context.Context, vm string) ([]string, error) {
	return d.snaps[vm], nil
}
func (d *repDisks) HasReplica(_ context.Context, vm string) bool    { _, ok := d.rep[vm]; return ok }
func (d *repDisks) ReplicaBytes(_ context.Context, vm string) int64 { return int64(len(d.rep[vm])) }
func (d *repDisks) DropReplica(_ context.Context, vm string) error {
	delete(d.rep, vm)
	delete(d.repSnaps, vm)
	return nil
}
func (d *repDisks) PromoteReplica(_ context.Context, vm string) (string, error) {
	d.data[vm], d.live[vm], d.snaps[vm] = d.rep[vm], true, d.repSnaps[vm]
	delete(d.rep, vm)
	delete(d.repSnaps, vm)
	return "/dev/zvol/tank/vms/" + vm + "-disk0", nil
}
func (d *repDisks) RetireDisk(_ context.Context, vm string) error {
	d.rep[vm], d.repSnaps[vm] = d.data[vm], d.snaps[vm]
	delete(d.data, vm)
	delete(d.live, vm)
	delete(d.snaps, vm)
	return nil
}

// copyVM runs one replication from a to b like the cluster master does.
func copyVM(t *testing.T, a, b *Service, da *repDisks, name string) Replica {
	t.Helper()
	ctx := context.Background()
	have, err := b.ReplicaSnaps(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := a.SendReplica(ctx, name, have, "server-a", &buf); err != nil {
		t.Fatal(err)
	}
	pendingSnap = da.snaps[Prefix+name][len(da.snaps[Prefix+name])-1]
	r, err := b.RecvReplica(ctx, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReplicaFailover(t *testing.T) {
	ctx := context.Background()
	a, hvA, dA, _ := newSvc()
	b, hvB, dB, _ := newSvc()
	ra := newRep(&memDisks{fakeDisks: dA, data: map[string][]byte{}})
	rb := newRep(&memDisks{fakeDisks: dB, data: map[string][]byte{}})
	a.Disks, b.Disks = ra, rb
	a.ReplicaDir, b.ReplicaDir = t.TempDir(), t.TempDir()
	a.NVRAMDir, b.NVRAMDir = t.TempDir(), t.TempDir()
	a.TPMDir, b.TPMDir = t.TempDir(), t.TempDir()
	if _, err := a.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	ra.data["p8s-web1"] = []byte("v1")
	src, _ := virt.ParseDomainXML(hvA.doms["p8s-web1"])
	tpm := a.tpmFile(src.UUID)
	if err := a.writeTPM(src.UUID, []byte("tpm-state")); err != nil {
		t.Fatal(err)
	}
	_ = tpm

	// while running: first copy is full, the next one only the change
	r := copyVM(t, a, b, ra, "web1")
	if r.Name != "web1" || r.From != "server-a" || string(rb.rep["p8s-web1"]) != "v1" || r.At == "" {
		t.Fatalf("first copy %+v %q", r, rb.rep["p8s-web1"])
	}
	ra.data["p8s-web1"] = []byte("v2")
	copyVM(t, a, b, ra, "web1")
	if string(rb.rep["p8s-web1"]) != "v2" {
		t.Fatal("second copy")
	}
	if l := b.Replicas(ctx); len(l) != 1 || l[0].MemoryMiB != 2048 {
		t.Fatalf("replicas %+v", l)
	}
	// a server can't keep a copy of a VM it runs
	if _, err := a.ReplicaSnaps(ctx, "web1"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer

	// server A dies: B starts the copy
	v, note, err := b.PromoteReplica(ctx, "web1")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := virt.ParseDomainXML(hvB.doms["p8s-web1"])
	if v.Name != "web1" || sp.UUID != src.UUID || sp.MAC != src.MAC || string(rb.data["p8s-web1"]) != "v2" || !strings.Contains(note, "from the copy") {
		t.Fatalf("promoted %+v %q %+v", v, note, sp)
	}
	if b, err := readFile(b.tpmFile(src.UUID)); err != nil || b != "tpm-state" {
		t.Fatalf("tpm %q %v", b, err)
	}
	if len(b.Replicas(ctx)) != 0 {
		t.Fatal("copy still listed")
	}
	if _, _, err := b.PromoteReplica(ctx, "web1"); err == nil {
		t.Fatal("promoted twice")
	}
	rb.data["p8s-web1"] = []byte("v3 on b")

	// A comes back: its old VM becomes the copy, copying goes B → A, from the common snapshot
	if err := a.RetireVM(ctx, "web1", "server-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(ctx, "web1"); err == nil {
		t.Fatal("old VM still defined on A")
	}
	have, _ := a.ReplicaSnaps(ctx, "web1")
	buf.Reset()
	if err := b.SendReplica(ctx, "web1", have, "server-b", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"base":"p8s-rep-`) {
		t.Fatalf("not incremental: %.300s", buf.String())
	}
	pendingSnap = rb.snaps["p8s-web1"][len(rb.snaps["p8s-web1"])-1]
	if _, err := a.RecvReplica(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if string(ra.rep["p8s-web1"]) != "v3 on b" {
		t.Fatalf("reverse copy %q", ra.rep["p8s-web1"])
	}
	// fencing: only running VMs are switched off
	b.Start(ctx, "web1")
	if got := b.Fence(ctx, []string{"web1", "nope"}); len(got) != 1 || got[0] != "web1" {
		t.Fatalf("fenced %v", got)
	}
	if st, _ := hvB.State(ctx, "p8s-web1"); st != virt.Stopped {
		t.Fatalf("state %s", st)
	}
	if err := a.DropReplica(ctx, "web1"); err != nil || len(a.Replicas(ctx)) != 0 {
		t.Fatal("drop")
	}
	buf.Reset()
	b.SendReplica(ctx, "web1", nil, "", &buf)
	if _, err := b.RecvReplica(ctx, &buf); err == nil {
		t.Fatal("copy of its own VM accepted")
	}
	_ = hvA
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
