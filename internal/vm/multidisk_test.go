package vm

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"p8sos/internal/virt"
)

// multiDisks: memDisks that move and copy data disks too. Each disk in the
// stream is a 4-byte length + its size in GB as text.
type multiDisks struct {
	*repDisks
	repExtra map[string]map[int]int // copies of data disks
}

func put(w io.Writer, s string) error {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(s)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

func get(r io.Reader) (string, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return "", err
	}
	b := make([]byte, binary.BigEndian.Uint32(h[:]))
	_, err := io.ReadFull(r, b)
	return string(b), err
}

func (d *multiDisks) SendDisks(_ context.Context, vm string, idx []int, w io.Writer) error {
	if err := put(w, string(d.data[vm])); err != nil {
		return err
	}
	for _, i := range idx {
		if err := put(w, fmt.Sprint(d.extra[vm][i])); err != nil {
			return err
		}
	}
	return nil
}

func (d *multiDisks) RecvDisks(_ context.Context, vm string, idx []int, r io.Reader) (string, map[int]string, error) {
	root, err := get(r)
	if err != nil {
		return "", nil, err
	}
	d.data[vm], d.live[vm] = []byte(root), true
	paths := map[int]string{}
	for _, i := range idx {
		s, err := get(r)
		if err != nil {
			return "", nil, err
		}
		var gb int
		fmt.Sscan(s, &gb)
		if _, err := d.CreateExtra(context.Background(), vm, i, gb); err != nil {
			return "", nil, err
		}
		paths[i] = fmt.Sprintf("/dev/zvol/tank/vms/%s-disk%d", vm, i)
	}
	return "/dev/zvol/tank/vms/" + vm + "-disk0", paths, nil
}

func (d *multiDisks) WaitExtra(_ context.Context, vm string, i int) (string, error) {
	return fmt.Sprintf("/dev/zvol/tank/vms/%s-disk%d", vm, i), nil
}

func (d *multiDisks) SendReplicaDisks(ctx context.Context, vm string, idx []int, base string, header func(string, []string) error, w io.Writer) error {
	return d.repDisks.SendReplica(ctx, vm, base, func(snap string) error {
		bases := make([]string, len(idx)+1)
		for k := range bases {
			bases[k] = base
		}
		if err := header(snap, bases); err != nil {
			return err
		}
		for _, i := range idx {
			if err := put(w, fmt.Sprint(d.extra[vm][i])); err != nil {
				return err
			}
		}
		return nil
	}, w)
}

func (d *multiDisks) RecvReplicaDisks(ctx context.Context, vm string, idx []int, bases []string, r io.Reader) error {
	if d.repExtra == nil {
		d.repExtra = map[string]map[int]int{}
	}
	d.repExtra[vm] = map[int]int{}
	for _, i := range idx {
		s, err := get(r)
		if err != nil {
			return err
		}
		var gb int
		fmt.Sscan(s, &gb)
		d.repExtra[vm][i] = gb
	}
	return d.repDisks.RecvReplica(ctx, vm, bases[0], r)
}

func (d *multiDisks) ReplicaSnapsDisks(ctx context.Context, vm string, idx []int) ([]string, error) {
	return d.repDisks.ReplicaSnaps(ctx, vm)
}

func (d *multiDisks) PromoteReplica(ctx context.Context, vm string) (string, error) {
	for i, gb := range d.repExtra[vm] {
		if _, err := d.CreateExtra(ctx, vm, i, gb); err != nil {
			return "", err
		}
	}
	return d.repDisks.PromoteReplica(ctx, vm)
}

func TestMoveAndCopyDataDisks(t *testing.T) {
	ctx := context.Background()
	a, hvA, dA, _ := newSvc()
	b, hvB, dB, _ := newSvc()
	ma := &multiDisks{repDisks: newRep(&memDisks{fakeDisks: dA, data: map[string][]byte{}})}
	mb := &multiDisks{repDisks: newRep(&memDisks{fakeDisks: dB, data: map[string][]byte{}})}
	a.Disks, b.Disks = ma, mb
	a.ReplicaDir, b.ReplicaDir = t.TempDir(), t.TempDir()
	if _, err := a.Create(ctx, CreateRequest{Name: "db1", Template: "debian-13", DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AddDisk(ctx, "db1", 50); err != nil {
		t.Fatal(err)
	}
	ma.data["p8s-db1"] = []byte("root disk")
	// high availability takes it now
	if why, _ := a.HACheck(ctx, "db1"); why != "" {
		t.Fatalf("HA refused: %s", why)
	}
	r := copyVM(t, a, b, ma.repDisks, "db1")
	if r.Name != "db1" || mb.repExtra["p8s-db1"][1] != 50 {
		t.Fatalf("copy %+v %+v", r, mb.repExtra)
	}
	h, _ := b.loadReplica("db1")
	if len(h.Extra) != 1 || h.Extra[0].Index != 1 || len(h.Bases) != 2 {
		t.Fatalf("header %+v", h)
	}
	// the server dies: b starts it with both disks
	delete(hvA.doms, "p8s-db1")
	v, _, err := b.PromoteReplica(ctx, "db1")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := virt.ParseDomainXML(hvB.doms["p8s-db1"])
	if len(sp.Extra) != 1 || sp.Extra[0].Path != "/dev/zvol/tank/vms/p8s-db1-disk1" || len(v.Disks) != 1 {
		t.Fatalf("failover lost the data disk: %+v / %+v", sp.Extra, v.Disks)
	}

	// an offline move with the data disk
	if why, _ := b.MoveCheck(ctx, "db1"); why != "" {
		t.Fatalf("move refused: %s", why)
	}
	var buf bytes.Buffer
	if err := b.ExportVM(ctx, "db1", &buf); err != nil {
		t.Fatal(err)
	}
	c, hvC, dC, _ := newSvc()
	mc := &multiDisks{repDisks: newRep(&memDisks{fakeDisks: dC, data: map[string][]byte{}})}
	c.Disks = mc
	mv, _, err := c.ImportMovedVM(ctx, &buf)
	if err != nil {
		t.Fatal(err)
	}
	sp, _ = virt.ParseDomainXML(hvC.doms["p8s-db1"])
	if string(mc.data["p8s-db1"]) != "root disk" || dC.extra["p8s-db1"][1] != 50 || len(sp.Extra) != 1 || len(mv.Disks) != 1 {
		t.Fatalf("moved %+v / %+v", sp.Extra, dC.extra)
	}
}
