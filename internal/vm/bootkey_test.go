package vm

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"p8sos/internal/isos"
)

type keyHV struct {
	*fakeHV
	keys atomic.Int32
}

func (k *keyHV) SendKey(_ context.Context, _ string, keys ...string) error {
	if len(keys) == 1 && keys[0] == "KEY_ENTER" {
		k.keys.Add(1)
	}
	return nil
}

func TestBootKey(t *testing.T) {
	oldW, oldE, oldB := BootKeyWindow, bootKeyEvery, blankDisk
	defer func() { BootKeyWindow, bootKeyEvery, blankDisk = oldW, oldE, oldB }()
	BootKeyWindow, bootKeyEvery = 100*time.Millisecond, 10*time.Millisecond
	blank := true
	blankDisk = func(string) bool { return blank }

	ctx := context.Background()
	svc, hv, _, _ := newSvc()
	k := &keyHV{fakeHV: hv}
	svc.HV = k
	svc.ISOs = &isos.Library{Dir: t.TempDir()}
	if _, err := svc.SaveISO("win.iso", bytes.NewReader(bootISO())); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "w1", ISO: "win.iso", OSType: "windows", DiskGB: 64, MemoryMiB: 4096}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := k.keys.Load(); n < 3 {
		t.Fatalf("blank disk + CD: pressed %d times", n)
	}
	// installed: the disk has a partition table, nothing is pressed
	blank = false
	k.keys.Store(0)
	if _, err := svc.Stop(ctx, "w1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := k.keys.Load(); n != 0 {
		t.Fatalf("installed disk: pressed %d times", n)
	}
}
