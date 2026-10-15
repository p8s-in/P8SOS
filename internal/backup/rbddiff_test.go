package backup

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"p8sos/internal/testfake"
)

// rbdDiff builds an rbd diff v1 stream: writes (offset → data), zeroed ranges, final size.
func rbdDiff(from, to string, size int64, writes map[int64][]byte, zeros [][2]int64) []byte {
	var b bytes.Buffer
	b.WriteString("rbd diff v1\n")
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v int64) { binary.Write(&b, binary.LittleEndian, uint64(v)) }
	b.WriteByte('f')
	u32(uint32(len(from)))
	b.WriteString(from)
	b.WriteByte('t')
	u32(uint32(len(to)))
	b.WriteString(to)
	b.WriteByte('s')
	u64(size)
	for off, d := range writes {
		b.WriteByte('w')
		u64(off)
		u64(int64(len(d)))
		b.Write(d)
	}
	for _, z := range zeros {
		b.WriteByte('z')
		u64(z[0])
		u64(z[1])
	}
	b.WriteByte('e')
	return b.Bytes()
}

func TestApplyRBDDiff(t *testing.T) {
	f := filepath.Join(t.TempDir(), "disk")
	os.WriteFile(f, bytes.Repeat([]byte("a"), 100), 0o600)
	d := rbdDiff("s1", "s2", 100, map[int64][]byte{10: []byte("HELLO")}, [][2]int64{{50, 10}})
	fh, _ := os.OpenFile(f, os.O_WRONLY, 0)
	info, err := ApplyRBDDiff(bytes.NewReader(d), fh, nil)
	fh.Close()
	if err != nil || info.From != "s1" || info.To != "s2" || info.Size != 100 || info.Written != 5 {
		t.Fatalf("%v %+v", err, info)
	}
	b, _ := os.ReadFile(f)
	want := bytes.Repeat([]byte("a"), 100)
	copy(want[10:], "HELLO")
	copy(want[50:60], make([]byte, 10))
	if !bytes.Equal(b, want) {
		t.Fatalf("got %q", b)
	}
	// cut short / not a diff
	if _, err := ApplyRBDDiff(bytes.NewReader(d[:len(d)-1]), nil, nil); err == nil {
		t.Fatal("truncated stream passed")
	}
	if _, err := ApplyRBDDiff(bytes.NewReader([]byte("hello world, not a diff")), nil, nil); !errors.Is(err, errDiffFormat) {
		t.Fatalf("got %v", err)
	}
	// a change past the end
	if _, err := ApplyRBDDiff(bytes.NewReader(rbdDiff("a", "b", 10, map[int64][]byte{8: []byte("xyz")}, nil)), nil, nil); err == nil {
		t.Fatal("change past the end passed")
	}
}

func TestRawIncremental(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	img := bytes.Repeat([]byte("root-disk-"), 50000) // 500 KB
	os.WriteFile(filepath.Join(dir, "full"), img, 0o600)
	// second backup: a few bytes changed, a range zeroed, the disk grew
	grown := int64(len(img) + 4096)
	os.WriteFile(filepath.Join(dir, "diff1"), rbdDiff("p8s-backup-1", "p8s-backup-2", grown,
		map[int64][]byte{0: []byte("CHANGED"), int64(len(img)): []byte("tail")}, [][2]int64{{1000, 2000}}), 0o600)
	os.WriteFile(filepath.Join(dir, "diff2"), rbdDiff("p8s-backup-2", "p8s-backup-3", grown,
		map[int64][]byte{100: []byte("third")}, nil), 0o600)
	s := &Store{Dir: filepath.Join(dir, "backups"), Run: &testfake.Runner{}, Compressor: "gzip"}
	src := func(file string) func(ctx context.Context, i int) *exec.Cmd {
		return func(ctx context.Context, i int) *exec.Cmd {
			return exec.CommandContext(ctx, "cat", filepath.Join(dir, file))
		}
	}
	if _, err := s.SaveRaw(ctx, Meta{ID: "20261015-100000", VM: "cvm"}, map[int]int64{0: int64(len(img))}, src("full")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRaw(ctx, Meta{ID: "20261015-110000", VM: "cvm", Base: "20261015-100000"}, map[int]int64{0: grown}, src("diff1")); err != nil {
		t.Fatal(err)
	}
	m3, err := s.SaveRaw(ctx, Meta{ID: "20261015-120000", VM: "cvm", Base: "20261015-110000"}, map[int]int64{0: grown}, src("diff2"))
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := s.Files(m3); filepath.Ext(filepath.Base(data[:len(data)-3])) != ".rdiff" {
		t.Fatalf("file %s", data)
	}
	want := make([]byte, grown)
	copy(want, img)
	copy(want, "CHANGED")
	copy(want[len(img):], "tail")
	copy(want[1000:3000], make([]byte, 2000))
	copy(want[100:], "third")

	if v, err := s.Verify(ctx, m3); err != nil || v.VerifiedAt == "" {
		t.Fatalf("verify: %v", err)
	}
	dev := filepath.Join(dir, "dev")
	os.WriteFile(dev, bytes.Repeat([]byte("X"), int(grown)), 0o600)
	if err := s.RestoreRaw(ctx, m3, map[int]string{0: dev}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dev); !bytes.Equal(b, want) {
		t.Fatal("restored disk differs")
	}
	sparse := filepath.Join(dir, "browse.img")
	if err := s.RawToFile(ctx, m3, 0, sparse); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sparse); !bytes.Equal(b, want) {
		t.Fatalf("browse file differs (%d bytes)", len(b))
	}
	// the backups it builds on can't go first
	if err := s.Delete("cvm", "20261015-100000"); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete base: %v", err)
	}
	// a damaged diff fails the restore test
	data, _ := s.Files(m3)
	os.WriteFile(data, []byte("broken"), 0o600)
	if _, err := s.Verify(ctx, m3); err == nil {
		t.Fatal("damaged diff passed")
	}
}
