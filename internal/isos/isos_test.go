package isos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func fakeISO(sig string) []byte {
	b := make([]byte, 40000)
	copy(b[32769:], sig)
	return b
}

func TestSaveListDelete(t *testing.T) {
	l := &Library{Dir: filepath.Join(t.TempDir(), "isos")}
	if _, err := l.Save("debian.iso", bytes.NewReader(fakeISO("CD001"))); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Save("windows-server.iso", bytes.NewReader(fakeISO("BEA01"))); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Save("debian.iso", bytes.NewReader(fakeISO("CD001"))); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := l.Save("notes.iso", bytes.NewReader([]byte("hello"))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("not an ISO: %v", err)
	}
	for _, bad := range []string{"../etc/passwd.iso", "x.img", ".hidden.iso", "a b.iso", ""} {
		if _, err := l.Save(bad, bytes.NewReader(fakeISO("CD001"))); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	list, _ := l.List()
	if len(list) != 2 || list[0].Name != "debian.iso" || list[0].Bytes != 40000 {
		t.Fatalf("list %+v", list)
	}
	if fi, _ := os.Stat(filepath.Join(l.Dir, "debian.iso")); fi.Mode().Perm() != 0o644 {
		t.Fatalf("QEMU must be able to read ISOs: %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(l.Dir, "notes.iso.partial")); err == nil {
		t.Fatal("partial file left")
	}
	if p, err := l.Path("debian.iso"); err != nil || filepath.Base(p) != "debian.iso" {
		t.Fatalf("path %q %v", p, err)
	}
	if err := l.Delete("debian.iso"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Path("debian.iso"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted ISO still found")
	}
}

func TestDownload(t *testing.T) {
	body := fakeISO("CD001")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.iso" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	l := &Library{Dir: t.TempDir()}
	ctx := context.Background()
	var calls int
	iso, err := l.Download(ctx, srv.URL+"/dl/alpine-3.20.iso", "", hex.EncodeToString(sum[:]), func(done, total int64) { calls++ })
	if err != nil || iso.Name != "alpine-3.20.iso" || iso.Bytes != int64(len(body)) || calls == 0 {
		t.Fatalf("download: %v %+v calls=%d", err, iso, calls)
	}
	if _, err := l.Download(ctx, srv.URL+"/x/other.iso", "", hex.EncodeToString(make([]byte, 32)), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad checksum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.Dir, "other.iso")); err == nil {
		t.Fatal("file with a wrong checksum was kept")
	}
	if _, err := l.Download(ctx, srv.URL+"/missing.iso", "", "", nil); err == nil {
		t.Fatal("404 should fail")
	}
	if _, err := l.Download(ctx, "file:///etc/passwd", "x.iso", "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("file url: %v", err)
	}
	if _, err := l.Download(ctx, srv.URL+"/noext", "", "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no name: %v", err)
	}
}
