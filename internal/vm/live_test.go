package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/virt"
)

type liveHVFake struct {
	*fakeHV
	sent   []string
	failed bool
}

func (h *liveHVFake) MigratableXML(ctx context.Context, n string, inactive bool) ([]byte, error) {
	return h.XML(ctx, n)
}
func (h *liveHVFake) Migrate(ctx context.Context, n, ip string, x, px []byte, progress func(int)) error {
	if h.failed {
		return errors.New("live move: unable to connect")
	}
	progress(100)
	h.sent = append(h.sent, n+"@"+ip+":"+string(x))
	return h.Undefine(ctx, n)
}

type liveDisksFake struct {
	*fakeDisks
	created []string
}

func (d *liveDisksFake) DiskBytes(_ context.Context, vm string, i int) (int64, error) {
	return int64(10+i) << 30, nil
}
func (d *liveDisksFake) CreateSized(_ context.Context, vm string, i int, b int64) (string, error) {
	d.created = append(d.created, fmt.Sprintf("%s/%d/%d", vm, i, b))
	if i == 0 {
		d.live[vm] = true
	}
	return fmt.Sprintf("/dev/zvol/other/%s-disk%d", vm, i), nil
}

func liveSvc(t *testing.T) (*Service, *liveHVFake, *liveDisksFake) {
	svc, hv, d, _ := newSvc()
	lh, ld := &liveHVFake{fakeHV: hv}, &liveDisksFake{fakeDisks: d}
	svc.HV, svc.Disks = lh, ld
	return svc, lh, ld
}

func TestLiveMove(t *testing.T) {
	ctx := context.Background()
	ci := filepath.Join(t.TempDir(), "cpuinfo")
	os.WriteFile(ci, []byte("model name\t: Test CPU 9000\nflags\t\t: fpu vmx aes\n"), 0o644)
	CPUInfo = ci
	defer func() { CPUInfo = "/proc/cpuinfo" }()

	src, shv, sd := liveSvc(t)
	dst, _, dd := liveSvc(t)
	if _, err := src.Create(ctx, CreateRequest{Name: "db1", Template: "debian-13", VCPUs: 2, MemoryMiB: 2048, DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.AddDisk(ctx, "db1", 20); err != nil {
		t.Fatal(err)
	}
	if why, _ := src.LiveCheck(ctx, "db1"); why != "" {
		t.Fatalf("check: %q", why)
	}
	h, err := src.LiveHeader(ctx, "db1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Disks) != 2 || h.Disks[1].Index != 1 || h.Disks[1].Bytes != 11<<30 || !strings.HasPrefix(h.CPUSig, "Test CPU 9000 #") || !strings.Contains(h.XML, "p8s-db1-disk1") {
		t.Fatalf("header %+v", h.Disks)
	}
	// a different CPU is refused for a VM on the host's CPU
	other := h
	other.CPUSig = "Other CPU #00"
	if _, err := dst.LivePrepare(ctx, other); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "compatible") {
		t.Fatalf("cpu check: %v", err)
	}
	plan, err := dst.LivePrepare(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(dd.created) != 2 || dd.created[1] != "p8s-db1/1/"+fmt.Sprint(int64(11)<<30) ||
		!strings.Contains(plan.XML, "/dev/zvol/other/p8s-db1-disk1") || strings.Contains(plan.XML, "/dev/zvol/tank/vms/p8s-db1-disk0") {
		t.Fatalf("prepare: %v\n%s", dd.created, plan.XML)
	}
	if err := src.LiveSend(ctx, "db1", "10.250.0.3", plan, func(int) {}); err != nil {
		t.Fatal(err)
	}
	if len(shv.sent) != 1 || !strings.HasPrefix(shv.sent[0], "p8s-db1@10.250.0.3:") {
		t.Fatalf("sent %v", shv.sent)
	}
	if err := src.LiveCleanup(ctx, "db1"); err != nil || sd.live["p8s-db1"] {
		t.Fatalf("cleanup %v %v", err, sd.live)
	}
	// abort on the target removes what prepare made
	if err := dst.LiveAbort(ctx, "db1"); err != nil || dd.live["p8s-db1"] {
		t.Fatalf("abort %v", err)
	}
	// stopped VMs and UEFI VMs aren't moved live
	if _, err := src.Create(ctx, CreateRequest{Name: "w1", Template: "debian-13", Start: new(bool)}); err != nil {
		t.Fatal(err)
	}
	if why, _ := src.LiveCheck(ctx, "w1"); !strings.Contains(why, "isn't running") {
		t.Fatalf("stopped: %q", why)
	}
	// CPU type
	v, err := src.SetCPU(ctx, "w1", virt.CPU{Model: "Skylake-Client", Features: []string{"+aes", "-hle"}})
	if err != nil || v.CPU != "Skylake-Client" {
		t.Fatalf("cpu %v %+v", err, v)
	}
	if _, err := src.SetCPU(ctx, "w1", virt.CPU{Model: "bad model;"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad model: %v", err)
	}
	if v, _ := src.SetCPU(ctx, "w1", virt.CPU{}); v.CPU != "host" {
		t.Fatalf("back to host: %+v", v)
	}
}
