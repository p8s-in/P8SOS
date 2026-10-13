package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/jobs"
)

func TestCrypt(t *testing.T) {
	for _, n := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 12345} {
		plain := make([]byte, n)
		rand.Read(plain)
		r, err := Encrypt(bytes.NewReader(plain), "correct horse battery")
		if err != nil {
			t.Fatal(err)
		}
		enc, _ := io.ReadAll(r)
		if n > 64 && bytes.Contains(enc, plain[:64]) {
			t.Fatal("plaintext visible")
		}
		var out bytes.Buffer
		if err := Decrypt(&out, bytes.NewReader(enc), "correct horse battery"); err != nil || !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("n=%d: %v (got %d bytes)", n, err, out.Len())
		}
		if err := Decrypt(io.Discard, bytes.NewReader(enc), "wrong horse battery"); !errors.Is(err, ErrPassphrase) {
			t.Fatalf("n=%d wrong pass: %v", n, err)
		}
		if n > chunkSize { // cut after the first chunk: must not pass as complete
			cut := enc[:len(magic)+24+4+chunkSize+16]
			if err := Decrypt(io.Discard, bytes.NewReader(cut), "correct horse battery"); err == nil {
				t.Fatalf("n=%d truncated file accepted", n)
			}
		}
	}
}

// localBackup writes a fake local backup like backup.Store would.
func localBackup(t *testing.T, dir, kind, guest, id string, size int) []byte {
	if kind == "ct" {
		dir = filepath.Join(dir, "_ct")
	}
	s := &backup.Store{Dir: dir}
	m := backup.Meta{ID: id, VM: guest, Compression: "zstd", CreatedAt: id, Owner: "alice", Bytes: int64(size)}
	data, meta := s.Files(m)
	os.MkdirAll(filepath.Dir(data), 0o700)
	b := make([]byte, size)
	rand.Read(b)
	os.WriteFile(data, b, 0o600)
	j, _ := json.Marshal(m)
	os.WriteFile(meta, j, 0o600)
	return b
}

func keys(t *testing.T, root string) []string {
	var out []string
	filepath.Walk(root, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() {
			r, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func newMgr(t *testing.T) (*Manager, string) {
	dir := t.TempDir()
	m := &Manager{File: filepath.Join(dir, "offsite.json"), BackupDir: filepath.Join(dir, "backups"), KeyFile: filepath.Join(dir, "key"),
		Jobs: &jobs.Manager{}}
	return m, dir
}

func TestDirTarget(t *testing.T) {
	ctx := context.Background()
	m, dir := newMgr(t)
	usb := filepath.Join(dir, "usb")
	if _, err := m.Save(Target{Name: "usb", Type: "dir", Path: "relative"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("relative path accepted")
	}
	if _, err := m.Save(Target{Name: "usb", Type: "dir", Path: usb, Keep: 2, Enabled: true, Passphrase: "short"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("short passphrase accepted")
	}
	if _, err := m.Save(Target{Name: "usb", Type: "dir", Path: usb, Keep: 2, Enabled: true, Prefix: "srv1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Test(ctx, "usb"); err == nil {
		t.Fatal("test passed without the disk mounted")
	}
	os.MkdirAll(usb, 0o700)
	if err := m.Test(ctx, "usb"); err != nil {
		t.Fatal(err)
	}
	web := localBackup(t, m.BackupDir, "vm", "web1", "20261001-023000", 5000)
	localBackup(t, m.BackupDir, "vm", "web1", "20261002-023000", 10)
	localBackup(t, m.BackupDir, "vm", "web1", "20261003-023000", 10)
	localBackup(t, m.BackupDir, "ct", "shop", "20261003-023000", 20)

	m.Enqueue("vm", "web1", "20261003-023000")
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { m.Run(cctx, time.Hour); close(done) }()
	waitFor(t, func() bool { return len(keys(t, usb)) == 2 })
	cancel()
	<-done
	if k := keys(t, usb); k[0] != "srv1/vm/web1/web1-20261003-023000.json" || k[1] != "srv1/vm/web1/web1-20261003-023000.zfs.zst" {
		t.Fatalf("after one copy: %v", k)
	}
	// sync: everything missing, but only the newest 2 per guest
	j, err := m.StartSync("usb")
	if err != nil {
		t.Fatal(err)
	}
	m.Jobs.Wait()
	if j, _ = m.Jobs.Get(j.ID); j.State != jobs.Done {
		t.Fatalf("sync %+v", j)
	}
	want := []string{"srv1/ct/shop/shop-20261003-023000.json", "srv1/ct/shop/shop-20261003-023000.zfs.zst",
		"srv1/vm/web1/web1-20261002-023000.json", "srv1/vm/web1/web1-20261002-023000.zfs.zst",
		"srv1/vm/web1/web1-20261003-023000.json", "srv1/vm/web1/web1-20261003-023000.zfs.zst"}
	if k := keys(t, usb); strings.Join(k, ",") != strings.Join(want, ",") {
		t.Fatalf("after sync: %v", k)
	}
	v, _ := m.Targets()
	if v[0].Status.LastOK == "" || v[0].Status.Copies != 3 {
		t.Fatalf("status %+v", v[0].Status)
	}
	// keep=1 prunes there
	m.Save(Target{Name: "usb", Type: "dir", Path: usb, Keep: 1, Enabled: true, Prefix: "srv1"})
	m.StartSync("usb")
	m.Jobs.Wait()
	if k := keys(t, usb); len(k) != 4 || strings.Contains(strings.Join(k, ","), "20261002") {
		t.Fatalf("after prune: %v", k)
	}
	// the oldest one is only local; copy an old one back after the server lost it
	os.MkdirAll(filepath.Join(usb, "srv1/vm/web1"), 0o700)
	os.WriteFile(filepath.Join(usb, "srv1/vm/web1/web1-20261001-023000.zfs.zst"), web, 0o600)
	mj, _ := os.ReadFile(filepath.Join(m.BackupDir, "web1/web1-20261001-023000.json"))
	os.WriteFile(filepath.Join(usb, "srv1/vm/web1/web1-20261001-023000.json"), mj, 0o600)
	os.RemoveAll(filepath.Join(m.BackupDir, "web1"))
	l, err := m.RemoteBackups(ctx, "usb")
	if err != nil || len(l) != 3 || l[2].ID != "20261001-023000" || l[2].Local || l[2].Owner != "alice" {
		t.Fatalf("remote %+v %v", l, err)
	}
	j, _ = m.StartFetch("usb", "vm", "web1", "20261001-023000")
	m.Jobs.Wait()
	if j, _ = m.Jobs.Get(j.ID); j.State != jobs.Done {
		t.Fatalf("fetch %+v", j)
	}
	got, _ := os.ReadFile(filepath.Join(m.BackupDir, "web1/web1-20261001-023000.zfs.zst"))
	if !bytes.Equal(got, web) {
		t.Fatal("fetched data differs")
	}
	if l, _ := (&backup.Store{Dir: m.BackupDir}).List("web1"); len(l) != 1 {
		t.Fatalf("fetched backup not listed: %+v", l)
	}
	// secrets never shown; keeping them on update
	m.Save(Target{Name: "usb", Type: "dir", Path: usb, Keep: 1, Passphrase: "a long passphrase"})
	m.Save(Target{Name: "usb", Type: "dir", Path: usb, Keep: 1})
	v, _ = m.Targets()
	if v[0].Passphrase != "" || !v[0].HasPassphrase {
		t.Fatalf("view %+v", v[0])
	}
}

func waitFor(t *testing.T, f func() bool) {
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestEncryptedTarget(t *testing.T) {
	ctx := context.Background()
	m, dir := newMgr(t)
	usb := filepath.Join(dir, "usb")
	os.MkdirAll(usb, 0o700)
	m.Save(Target{Name: "usb", Type: "dir", Path: usb, Enabled: true, Passphrase: "a long passphrase"})
	plain := localBackup(t, m.BackupDir, "ct", "shop", "20261003-023000", 3*chunkSize)
	m.StartSync("usb")
	m.Jobs.Wait()
	enc, err := os.ReadFile(filepath.Join(usb, "ct/shop/shop-20261003-023000.zfs.zst.p8senc"))
	if err != nil || bytes.Contains(enc, plain[:100]) {
		t.Fatalf("not encrypted: %v", err)
	}
	os.RemoveAll(filepath.Join(m.BackupDir, "_ct"))
	l, _ := m.RemoteBackups(ctx, "usb")
	if len(l) != 1 || !l[0].Encrypted {
		t.Fatalf("%+v", l)
	}
	j, _ := m.StartFetch("usb", "ct", "shop", "20261003-023000")
	m.Jobs.Wait()
	if j, _ = m.Jobs.Get(j.ID); j.State != jobs.Done {
		t.Fatalf("fetch %+v", j)
	}
	got, _ := os.ReadFile(filepath.Join(m.BackupDir, "_ct/shop/shop-20261003-023000.zfs.zst"))
	if !bytes.Equal(got, plain) {
		t.Fatal("decrypted data differs")
	}
}

// fakeS3 keeps objects in memory and supports the calls the backend makes.
type fakeS3 struct {
	mu      sync.Mutex
	objs    map[string][]byte
	uploads map[string]map[int][]byte
	parts   int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") || r.Header.Get("x-amz-date") == "" {
		w.WriteHeader(403)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bkt/")
	q := r.URL.Query()
	switch {
	case r.Method == "POST" && q.Has("uploads"):
		id := "up" + string(rune('a'+len(f.uploads)))
		f.uploads[id] = map[int][]byte{}
		w.Write([]byte("<InitiateMultipartUploadResult><UploadId>" + id + "</UploadId></InitiateMultipartUploadResult>"))
	case r.Method == "PUT" && q.Has("partNumber"):
		b, _ := io.ReadAll(r.Body)
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.uploads[q.Get("uploadId")][n] = b
		f.parts++
		w.Header().Set("ETag", `"e`+q.Get("partNumber")+`"`)
	case r.Method == "POST" && q.Has("uploadId"):
		up := f.uploads[q.Get("uploadId")]
		var all []byte
		for i := 1; i <= len(up); i++ {
			all = append(all, up[i]...)
		}
		f.objs[key] = all
		delete(f.uploads, q.Get("uploadId"))
		w.Write([]byte("<CompleteMultipartUploadResult/>"))
	case r.Method == "GET" && r.URL.Path == "/bkt":
		var b strings.Builder
		b.WriteString("<ListBucketResult>")
		ks := make([]string, 0)
		for k := range f.objs {
			if strings.HasPrefix(k, q.Get("prefix")) {
				ks = append(ks, k)
			}
		}
		sort.Strings(ks)
		for _, k := range ks {
			b.WriteString("<Contents><Key>" + k + "</Key><Size>" + strconv.Itoa(len(f.objs[k])) + "</Size></Contents>")
		}
		b.WriteString("<IsTruncated>false</IsTruncated></ListBucketResult>")
		w.Write([]byte(b.String()))
	case r.Method == "GET":
		o, ok := f.objs[key]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte("<Error><Code>NoSuchKey</Code></Error>"))
			return
		}
		w.Write(o)
	case r.Method == "DELETE":
		delete(f.objs, key)
		w.WriteHeader(204)
	default:
		w.WriteHeader(400)
	}
}

func TestS3Target(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objs: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	m, _ := newMgr(t)
	if _, err := m.Save(Target{Name: "b2", Type: "s3", Endpoint: srv.URL, Bucket: "bkt", AccessKey: "AK"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing secret accepted")
	}
	if _, err := m.Save(Target{Name: "b2", Type: "s3", Endpoint: srv.URL, Bucket: "bkt", AccessKey: "AK", SecretKey: "SK", Enabled: true, Prefix: "p8s"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Test(ctx, "b2"); err != nil {
		t.Fatal(err)
	}
	data := localBackup(t, m.BackupDir, "vm", "web1", "20261003-023000", 150_000)
	// small parts so the upload takes several
	tg, _ := m.target("b2")
	b, _ := newS3(tg, nil)
	b.partSize = 64 << 10
	if err := b.Put(ctx, "p8s/x/big", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if f.parts < 3 || !bytes.Equal(f.objs["p8s/x/big"], data) {
		t.Fatalf("multipart: %d parts", f.parts)
	}
	b.Delete(ctx, "p8s/x/big")
	m.StartSync("b2")
	m.Jobs.Wait()
	if !bytes.Equal(f.objs["p8s/vm/web1/web1-20261003-023000.zfs.zst"], data) || f.objs["p8s/vm/web1/web1-20261003-023000.json"] == nil {
		t.Fatalf("objects %v", len(f.objs))
	}
	l, err := m.RemoteBackups(ctx, "b2")
	if err != nil || len(l) != 1 || !l[0].Local {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestSSHTarget(t *testing.T) {
	ctx := context.Background()
	m, dir := newMgr(t)
	remote := filepath.Join(dir, "remote it's")
	var calls []string
	// "ssh" runs the remote command locally, so the scripts and quoting are exercised
	m.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name)
		if name == "ssh" {
			return exec.CommandContext(ctx, "sh", "-c", args[len(args)-1])
		}
		return exec.CommandContext(ctx, name, args...)
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	if _, err := m.Save(Target{Name: "nas", Type: "ssh", Host: "nas.local; rm -rf /", RemoteDir: "/x"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad host accepted")
	}
	// the quote in the folder name is refused by validation, so test the backend directly with it
	be := &sshBackend{host: "nas", user: "root", port: 22, dir: remote, keyFile: "k", knownHosts: "kh", cmd: m.Cmd}
	if err := be.Put(ctx, "vm/web1/a.json", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := be.Get(ctx, "vm/web1/a.json", &got); err != nil || got.String() != "hello" {
		t.Fatalf("get %q %v", got.String(), err)
	}
	if err := be.Get(ctx, "vm/web1/nope", io.Discard); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	l, err := be.List(ctx, "vm/")
	if err != nil || len(l) != 1 || l[0].Key != "vm/web1/a.json" || l[0].Size != 5 {
		t.Fatalf("list %+v %v", l, err)
	}
	if err := be.Delete(ctx, "vm/web1/a.json"); err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, "../escape", strings.NewReader("x")); err == nil {
		t.Fatal("escaping key accepted")
	}
	if _, err := m.Save(Target{Name: "nas", Type: "ssh", Host: "nas.local", RemoteDir: filepath.Join(dir, "nas"), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pub, err := m.PublicKey(ctx)
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("key %q %v", pub, err)
	}
	localBackup(t, m.BackupDir, "vm", "web1", "20261003-023000", 1000)
	j, _ := m.StartSync("nas")
	m.Jobs.Wait()
	if j, _ = m.Jobs.Get(j.ID); j.State != jobs.Done {
		t.Fatalf("sync %+v", j)
	}
	if _, err := os.Stat(filepath.Join(dir, "nas/vm/web1/web1-20261003-023000.zfs.zst")); err != nil {
		t.Fatal(err)
	}
}

func TestDataDiskFiles(t *testing.T) {
	ctx := context.Background()
	m, dir := newMgr(t)
	usb := filepath.Join(dir, "usb")
	os.MkdirAll(usb, 0o700)
	if _, err := m.Save(Target{Name: "usb", Type: "dir", Path: usb, Keep: 1, Enabled: true, Passphrase: "a long passphrase"}); err != nil {
		t.Fatal(err)
	}
	s := &backup.Store{Dir: m.BackupDir}
	bm := backup.Meta{ID: "20261005-023000", VM: "db1", Compression: "zstd", CreatedAt: "x", Bytes: 7,
		Extra: []backup.ExtraDisk{{Index: 2, SizeGB: 5, Bytes: 9}}}
	data, meta := s.Files(bm)
	os.MkdirAll(filepath.Dir(data), 0o700)
	os.WriteFile(data, []byte("disk0.."), 0o600)
	os.WriteFile(s.ExtraFile(bm, 2), []byte("disk2...."), 0o600)
	j, _ := json.Marshal(bm)
	os.WriteFile(meta, j, 0o600)

	m.StartSync("usb")
	m.Jobs.Wait()
	k := strings.Join(keys(t, usb), ",")
	if !strings.Contains(k, "vm/db1/db1-20261005-023000-d2.zfs.zst.p8senc") || !strings.Contains(k, "vm/db1/db1-20261005-023000.json") {
		t.Fatalf("uploaded %s", k)
	}
	l, err := m.RemoteBackups(ctx, "usb")
	if err != nil || len(l) != 1 || l[0].Bytes < 16 {
		t.Fatalf("remote %+v %v", l, err)
	}
	// the server loses it; bring both disks back
	os.RemoveAll(filepath.Join(m.BackupDir, "db1"))
	jb, _ := m.StartFetch("usb", "vm", "db1", bm.ID)
	m.Jobs.Wait()
	if jb, _ = m.Jobs.Get(jb.ID); jb.State != jobs.Done {
		t.Fatalf("fetch %+v", jb)
	}
	if b, _ := os.ReadFile(s.ExtraFile(bm, 2)); string(b) != "disk2...." {
		t.Fatalf("disk 2 came back as %q", b)
	}
	if b, _ := os.ReadFile(data); string(b) != "disk0.." {
		t.Fatalf("disk 0 came back as %q", b)
	}
	// deleting the local backup removes its data disk file too
	if err := s.Delete("db1", bm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.ExtraFile(bm, 2)); err == nil {
		t.Fatal("disk 2 file left")
	}
}
