package vm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/jobs"
	"p8sos/internal/testfake"
)

type relocDisks struct {
	*fakeDisks
	where  map[string]string
	called []string
}

func (r *relocDisks) Storages() []string        { return []string{"local", "ceph"} }
func (r *relocDisks) Place(vm, st string) error { r.where[vm] = st; return nil }
func (r *relocDisks) StorageOf(vm string) string {
	if w := r.where[vm]; w != "" {
		return w
	}
	return "local"
}
func (r *relocDisks) Relocate(_ context.Context, vm, target string, idx []int, p func(string)) (map[int]string, error) {
	r.called = append(r.called, vm+" "+target)
	p("disk 0: copying")
	r.where[vm] = target
	return map[int]string{0: "/dev/rbd/p8s-vms/" + vm + "-disk0"}, nil
}

func TestMoveStorage(t *testing.T) {
	ctx := context.Background()
	svc, _, d, _ := newSvc()
	rd := &relocDisks{fakeDisks: d, where: map[string]string{}}
	svc.Disks = rd
	svc.Jobs = &jobs.Manager{}
	if _, err := svc.Create(ctx, CreateRequest{Name: "web1", Template: "debian-13", DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MoveStorage(ctx, "web1", "ceph", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("running: %v", err)
	}
	_, _ = svc.Stop(ctx, "web1", true)
	if _, err := svc.MoveStorage(ctx, "web1", "nas", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown storage: %v", err)
	}
	if _, err := svc.MoveStorage(ctx, "web1", "local", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("same storage: %v", err)
	}
	_ = d.Snapshot(ctx, "p8s-web1", "before")
	if _, err := svc.MoveStorage(ctx, "web1", "ceph", false); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "drop_snapshots") {
		t.Fatalf("snapshots: %v", err)
	}
	j, err := svc.MoveStorage(ctx, "web1", "ceph", true)
	if err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, svc, j)
	if got.State != jobs.Done || !strings.Contains(got.Message, "on ceph now") {
		t.Fatalf("job %+v", got)
	}
	sp, _ := svc.spec(ctx, "p8s-web1")
	if sp.DiskPath != "/dev/rbd/p8s-vms/p8s-web1-disk0" || len(rd.called) != 1 {
		t.Fatalf("disk path %s %v", sp.DiskPath, rd.called)
	}
}

type rawCephDisks struct {
	*relocDisks
	img     string // file standing in for every image
	created map[string]string
}

func (c *rawCephDisks) DiskDataset(vm string, i int) string {
	return "p8s-vms/" + vm + "-disk" + string(rune('0'+i))
}
func (c *rawCephDisks) DiskBytes(context.Context, string, int) (int64, error) {
	st, err := os.Stat(c.img)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
func (c *rawCephDisks) CreateSized(_ context.Context, vm string, i int, n int64) (string, error) {
	p := filepath.Join(filepath.Dir(c.img), vm+"-new"+string(rune('0'+i)))
	if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
		return "", err
	}
	c.created[vm] = p
	return p, nil
}

func (c *rawCephDisks) HasSnapshot(_ context.Context, vm, name string) bool {
	return snapDisksFake{c.fakeDisks}.HasSnapshot(context.Background(), vm, name)
}

// cephDiff is an rbd diff v1 stream writing data at off (image size n).
func cephDiff(n, off int64, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("rbd diff v1\n")
	b.WriteByte('s')
	binary.Write(&b, binary.LittleEndian, uint64(n))
	b.WriteByte('w')
	binary.Write(&b, binary.LittleEndian, uint64(off))
	binary.Write(&b, binary.LittleEndian, uint64(len(data)))
	b.Write(data)
	b.WriteByte('e')
	return b.Bytes()
}

func TestCephBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	svc, _, d, _ := newSvc()
	dir := t.TempDir()
	img := filepath.Join(dir, "image")
	os.WriteFile(img, bytes.Repeat([]byte("ceph"), 50000), 0o600)
	cd := &rawCephDisks{relocDisks: &relocDisks{fakeDisks: d, where: map[string]string{"p8s-db": "ceph"}}, img: img, created: map[string]string{}}
	svc.Disks = cd
	svc.Jobs = &jobs.Manager{}
	tick := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	svc.Backups = &backup.Store{Dir: filepath.Join(dir, "backups"), Run: &testfake.Runner{}, Compressor: "gzip",
		Now: func() time.Time { tick = tick.Add(time.Minute); return tick }}
	diff := filepath.Join(dir, "diff")
	os.WriteFile(diff, cephDiff(200000, 0, []byte("NEW!")), 0o600)
	var cmds []string
	old := imageCmd
	imageCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		if name == "rbd" && args[0] == "export-diff" {
			return exec.CommandContext(ctx, "cat", diff)
		}
		if name != "rbd" || args[0] != "export" || !strings.Contains(args[2], "@p8s-backup-") {
			t.Errorf("unexpected %s %v", name, args)
		}
		return exec.CommandContext(ctx, "cat", img)
	}
	defer func() { imageCmd = old }()
	if _, err := svc.Create(ctx, CreateRequest{Name: "db", Template: "debian-13", DiskGB: 10}); err != nil {
		t.Fatal(err)
	}
	cd.where["p8s-db"] = SharedStorage // it lives on Ceph
	j, err := svc.StartBackup(ctx, "db")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, svc, j); got.State != jobs.Done || !strings.Contains(got.Message, "from Ceph") {
		t.Fatalf("backup job %+v", got)
	}
	l, _ := svc.ListBackups("db")
	if len(l) != 1 || l[0].Format != backup.FormatRaw {
		t.Fatalf("backups %+v", l)
	}
	if snaps, _ := d.Snapshots(ctx, "p8s-db"); len(snaps) != 1 || snaps[0].Name != "p8s-backup-"+l[0].ID {
		t.Fatalf("the backup's snapshot should stay for the next one: %v", snaps)
	}
	// the next backup copies only the changes (rbd export-diff from that snapshot)
	j, err = svc.StartBackup(ctx, "db")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, svc, j); got.State != jobs.Done || !strings.Contains(got.Message, "changes since "+l[0].ID) {
		t.Fatalf("2nd backup job %+v", got)
	}
	if last := cmds[len(cmds)-1]; !strings.HasPrefix(last, "rbd export-diff --no-progress --from-snap p8s-backup-"+l[0].ID+" p8s-vms/p8s-db-disk0@p8s-backup-") {
		t.Fatalf("command %s", last)
	}
	l, _ = svc.ListBackups("db")
	if len(l) != 2 || l[0].Base != l[1].ID {
		t.Fatalf("backups %+v", l)
	}
	if snaps, _ := d.Snapshots(ctx, "p8s-db"); len(snaps) != 1 || snaps[0].Name != "p8s-backup-"+l[0].ID {
		t.Fatalf("only the newest backup's snapshot stays: %v", snaps)
	}
	// restore the incremental one as a new VM: full image + the changes
	j, err = svc.StartRestore(ctx, "db", l[0].ID, "db-copy")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, svc, j); got.State != jobs.Done {
		t.Fatalf("restore job %+v", got)
	}
	b, _ := os.ReadFile(cd.created["p8s-db-copy"])
	if want := append([]byte("NEW!"), bytes.Repeat([]byte("ceph"), 49999)...); !bytes.Equal(b, want) {
		t.Fatal("restored disk differs")
	}
	if v, err := svc.Get(ctx, "db-copy"); err != nil || v.State != "running" {
		t.Fatalf("restored vm %v %+v", err, v)
	}
}
