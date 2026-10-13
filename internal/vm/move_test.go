package vm

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"p8sos/internal/firewall"
	"p8sos/internal/virt"
)

// memDisks: fakeDisks whose disks have content and can be streamed.
type memDisks struct {
	*fakeDisks
	data map[string][]byte
}

func (d *memDisks) SendDisk(_ context.Context, vm string, w io.Writer) error {
	_, err := w.Write(d.data[vm])
	return err
}
func (d *memDisks) RecvDisk(_ context.Context, vm string, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	d.live[vm], d.data[vm] = true, b
	return "/dev/zvol/tank/vms/" + vm + "-disk0", nil
}

func TestMoveVM(t *testing.T) {
	ctx := context.Background()
	a, hvA, dA, _ := newSvc()
	b, hvB, dB, seedsB := newSvc()
	ma, mb := &memDisks{fakeDisks: dA, data: map[string][]byte{}}, &memDisks{fakeDisks: dB, data: map[string][]byte{}}
	a.Disks, b.Disks = ma, mb
	if _, err := a.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	ma.data["p8s-web1"] = bytes.Repeat([]byte("disk!"), 1000)
	a.SetFirewall(ctx, "web1", firewall.Settings{Enabled: true, Rules: []firewall.Rule{{Proto: "tcp", Ports: "22"}}})
	a.SetSchedule("web1", Schedule{Daily: true, Keep: 3})

	// running: refused
	if why, _ := a.MoveCheck(ctx, "web1"); !strings.Contains(why, "shut it down") {
		t.Fatalf("running: %q", why)
	}
	var buf bytes.Buffer
	if err := a.ExportVM(ctx, "web1", &buf); err == nil {
		t.Fatal("exported a running VM")
	}
	a.Stop(ctx, "web1", true)
	if err := a.ExportVM(ctx, "web1", &buf); err != nil {
		t.Fatal(err)
	}
	srcXML, _ := virt.ParseDomainXML(hvA.doms["p8s-web1"])
	v, note, err := b.ImportMovedVM(ctx, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "web1" || v.VCPUs != 2 || v.MemoryMiB != 2048 || note != "" {
		t.Fatalf("moved %+v %q", v, note)
	}
	sp, _ := virt.ParseDomainXML(hvB.doms["p8s-web1"])
	if sp.MAC != srcXML.MAC || sp.UUID != srcXML.UUID || sp.SeedISO == "" || !seedsB.live["p8s-web1"] {
		t.Fatalf("target spec %+v", sp)
	}
	if !bytes.Equal(mb.data["p8s-web1"], ma.data["p8s-web1"]) {
		t.Fatal("disk differs")
	}
	if fw, _ := b.GetFirewall(ctx, "web1"); !fw.Enabled || len(fw.Rules) != 1 {
		t.Fatalf("firewall %+v", fw)
	}
	if sch, _ := b.GetSchedule("web1"); !sch.Daily || sch.Keep != 3 {
		t.Fatalf("schedule %+v", sch)
	}
	// a second copy on the same server is refused
	buf.Reset()
	a.ExportVM(ctx, "web1", &buf)
	if _, _, err := b.ImportMovedVM(ctx, &buf); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, _, err := b.ImportMovedVM(ctx, strings.NewReader("garbage")); err == nil {
		t.Fatal("garbage accepted")
	}
}
