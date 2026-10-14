package backup

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"p8sos/internal/testfake"
)

func TestRawBackup(t *testing.T) {
	dir := t.TempDir()
	img0 := bytes.Repeat([]byte("root-disk-"), 100000) // 1 MB
	img1 := bytes.Repeat([]byte{0}, 300000)
	os.WriteFile(filepath.Join(dir, "img0"), img0, 0o600)
	os.WriteFile(filepath.Join(dir, "img1"), img1, 0o600)
	s := &Store{Dir: filepath.Join(dir, "backups"), Run: &testfake.Runner{}, Compressor: "gzip"}
	m, err := s.SaveRaw(context.Background(), Meta{ID: "20261014-100000", VM: "cephvm", Extra: []ExtraDisk{{Index: 1, SizeGB: 1}}},
		map[int]int64{0: int64(len(img0)), 1: int64(len(img1))},
		func(ctx context.Context, i int) *exec.Cmd {
			return exec.CommandContext(ctx, "cat", filepath.Join(dir, map[int]string{0: "img0", 1: "img1"}[i]))
		})
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != FormatRaw || m.ImageBytes != int64(len(img0)) || m.Extra[0].ImageBytes != 300000 {
		t.Fatalf("meta %+v", m)
	}
	l, _ := s.List("cephvm")
	if len(l) != 1 || l[0].Format != FormatRaw {
		t.Fatalf("list %+v", l)
	}
	if v, err := s.Verify(context.Background(), l[0]); err != nil || v.VerifiedAt == "" {
		t.Fatalf("verify: %v %+v", err, v)
	}
	// restore onto "devices" (files) that hold old data
	d0, d1 := filepath.Join(dir, "dev0"), filepath.Join(dir, "dev1")
	os.WriteFile(d0, bytes.Repeat([]byte("X"), len(img0)), 0o600)
	os.WriteFile(d1, bytes.Repeat([]byte("Y"), 300000), 0o600)
	if err := s.RestoreRaw(context.Background(), l[0], map[int]string{0: d0, 1: d1}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(d0); !bytes.Equal(b, img0) {
		t.Fatal("root disk differs")
	}
	if b, _ := os.ReadFile(d1); !bytes.Equal(b, img1) {
		t.Fatal("old data left in zero blocks")
	}
	// a damaged file fails the restore test
	data, _ := s.Files(l[0])
	os.WriteFile(data, []byte("not gzip"), 0o600)
	if _, err := s.Verify(context.Background(), l[0]); err == nil {
		t.Fatal("damaged backup passed")
	}
	if err := s.Delete("cephvm", m.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List("cephvm"); len(l) != 0 {
		t.Fatal("deleted")
	}
}
