package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"p8sos/internal/storage"
	"p8sos/internal/virt"
)

// The mirror against a real QEMU (TCG) and qemu-storage-daemon: the QMP
// commands P8S Mirror sends, the first sync, a write landing on both sides,
// and stopping. Runs only with P8S_QEMU_TEST=1 (it starts QEMU).

type qmpHV struct {
	*fakeHV
	mu   sync.Mutex
	sock string
}

func (h *qmpHV) State(context.Context, string) (virt.State, error) { return virt.Running, nil }

func (h *qmpHV) Monitor(_ context.Context, _ string, cmd []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := net.Dial("unix", h.sock)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	r := bufio.NewReader(c)
	read := func() ([]byte, error) {
		for {
			l, err := r.ReadBytes('\n')
			if err != nil {
				return nil, err
			}
			if !bytes.Contains(l, []byte(`"event"`)) {
				return l, nil
			}
		}
	}
	if _, err := read(); err != nil { // greeting
		return nil, err
	}
	fmt.Fprintln(c, `{"execute":"qmp_capabilities"}`)
	if _, err := read(); err != nil {
		return nil, err
	}
	c.Write(append(cmd, '\n'))
	out, err := read()
	if err != nil {
		return nil, err
	}
	return virt.QMPReturn(out)
}

type pathDisks struct {
	*fakeDisks
	root string
}

func (d pathDisks) DevicePath(string) string     { return d.root }
func (d pathDisks) ExtraPath(string, int) string { return "/nonexistent" }

func TestMirrorWithQEMU(t *testing.T) {
	if os.Getenv("P8S_QEMU_TEST") == "" {
		t.Skip("set P8S_QEMU_TEST=1 to run against a real QEMU")
	}
	for _, p := range []string{"qemu-system-x86_64", "qemu-storage-daemon"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip(p + " not installed")
		}
	}
	dir := t.TempDir()
	src, tgt := filepath.Join(dir, "src.raw"), filepath.Join(dir, "tgt.raw")
	data := bytes.Repeat([]byte("p8s mirror test "), 4<<20/16)
	os.WriteFile(src, data, 0o600)
	os.WriteFile(tgt, make([]byte, len(data)), 0o600)
	port := MirrorPortBase + 77
	disks := []storage.MirrorDisk{{Index: 0, Bytes: int64(len(data))}}
	sd := exec.Command("qemu-storage-daemon", storage.MirrorExportArgs("127.0.0.1", port, disks, []string{tgt})...)
	sd.Args = append(sd.Args[:1], append([]string{}, sd.Args[1:]...)...)
	// a file, not a block device, in this test
	for i, a := range sd.Args {
		sd.Args[i] = string(bytes.ReplaceAll([]byte(a), []byte("driver=host_device"), []byte("driver=file")))
	}
	if out, err := sd.StderrPipe(); err == nil {
		go func() {
			b := bufio.NewScanner(out)
			for b.Scan() {
				t.Log("qsd:", b.Text())
			}
		}()
	}
	if err := sd.Start(); err != nil {
		t.Fatal(err)
	}
	defer sd.Process.Kill()
	sock := filepath.Join(dir, "qmp.sock")
	q := exec.Command("qemu-system-x86_64", "-M", "q35", "-accel", "tcg", "-nodefaults", "-display", "none", "-S",
		"-blockdev", "driver=file,filename="+src+",node-name=f0", "-blockdev", "driver=raw,file=f0,node-name=fmt0",
		"-device", "virtio-blk-pci,drive=fmt0,id=vd0", "-qmp", "unix:"+sock+",server=on,wait=off")
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Process.Kill()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	old := mirrorHostOK
	mirrorHostOK = func(string) bool { return true }
	defer func() { mirrorHostOK = old }()
	s, hv, fd, _ := newSvc()
	s.HV = &qmpHV{fakeHV: hv, sock: sock}
	s.Disks = pathDisks{fakeDisks: fd, root: src}
	ctx := context.Background()
	if err := s.MirrorStart(ctx, "web", MirrorTarget{Host: "127.0.0.1", Port: port, Disks: []int{0}}); err != nil {
		t.Fatal(err)
	}
	var st MirrorState
	for i := 0; i < 100; i++ {
		var err error
		if st, err = s.MirrorStatus(ctx, "web"); err != nil {
			t.Fatal(err)
		}
		if st.InSync() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !st.InSync() || st.Progress() != 100 {
		b, _ := json.Marshal(st)
		t.Fatalf("not in sync: %s", b)
	}
	if got, _ := os.ReadFile(tgt); !bytes.Equal(got, data) {
		t.Fatal("first sync: the copy differs")
	}
	// a write by the VM lands on both sides at once
	if _, err := s.qmp(ctx, "p8s-web", "human-monitor-command",
		map[string]any{"command-line": `qemu-io -d /machine/peripheral/vd0/virtio-backend "write -P 0xab 65536 131072"`}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(tgt)
	if !bytes.Equal(a, b) || a[65536] != 0xab {
		t.Fatal("the write didn't reach the mirror")
	}
	// starting again while it runs changes nothing
	if err := s.MirrorStart(ctx, "web", MirrorTarget{Host: "127.0.0.1", Port: port, Disks: []int{0}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MirrorStop(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.MirrorStatus(ctx, "web"); len(st.Disks) != 1 || st.Disks[0].Job {
		t.Fatalf("still mirroring: %+v", st)
	}
	// a catch-up: the copy is made equal outside QEMU (ZFS does it on the
	// servers), then the mirror starts with only new writes (sync "none")
	if _, err := s.qmp(ctx, "p8s-web", "human-monitor-command",
		map[string]any{"command-line": `qemu-io -d /machine/peripheral/vd0/virtio-backend "write -P 0x5a 262144 65536"`}); err != nil {
		t.Fatal(err)
	}
	a, _ = os.ReadFile(src)
	os.WriteFile(tgt, a, 0o600)
	if err := s.MirrorStart(ctx, "web", MirrorTarget{Host: "127.0.0.1", Port: port, Disks: []int{0}, Sync: "none"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if st, _ = s.MirrorStatus(ctx, "web"); st.InSync() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !st.InSync() {
		t.Fatalf("sync none not in sync: %+v", st)
	}
	if _, err := s.qmp(ctx, "p8s-web", "human-monitor-command",
		map[string]any{"command-line": `qemu-io -d /machine/peripheral/vd0/virtio-backend "write -P 0x77 524288 4096"`}); err != nil {
		t.Fatal(err)
	}
	a, _ = os.ReadFile(src)
	b, _ = os.ReadFile(tgt)
	if !bytes.Equal(a, b) || a[524288] != 0x77 {
		t.Fatal("sync none: the new write didn't reach the copy")
	}
	if err := s.MirrorStop(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	// the other server goes away: the VM keeps writing, the mirror is gone (degraded)
	if err := s.MirrorStart(ctx, "web", MirrorTarget{Host: "127.0.0.1", Port: port, Disks: []int{0}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if st, _ = s.MirrorStatus(ctx, "web"); st.InSync() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	sd.Process.Kill()
	sd.Wait()
	for i := 0; i < 3; i++ {
		_, err := s.qmp(ctx, "p8s-web", "human-monitor-command",
			map[string]any{"command-line": fmt.Sprintf(`qemu-io -d /machine/peripheral/vd0/virtio-backend "write -P 0xcd %d 4096"`, 1<<20+i*8192)})
		if err != nil {
			t.Fatal(err)
		}
	}
	gone := false
	for i := 0; i < 100 && !gone; i++ { // after the 3 s reconnect wait
		st, _ = s.MirrorStatus(ctx, "web")
		gone = len(st.Disks) == 1 && !st.Disks[0].Job
		time.Sleep(100 * time.Millisecond)
	}
	if a, _ = os.ReadFile(src); !gone || a[1<<20] != 0xcd {
		t.Fatalf("degraded: gone=%v %+v", gone, st)
	}
}
