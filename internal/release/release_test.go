package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleSignExtract(t *testing.T) {
	src := t.TempDir()
	for p, c := range map[string]string{"update.sh": "#!/bin/bash\n", "cmd/a.go": "package a", ".git/HEAD": "x", "bin/p8sd": "x", "a.iso": "x", "scripts/p8": "#!/bin/bash", "VERSION": "old"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755)
		_ = os.WriteFile(filepath.Join(src, p), []byte(c), 0o644)
	}
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	m, err := Bundle(src, "0.6.0", "notes", out)
	if err != nil {
		t.Fatal(err)
	}
	sum, n, _ := FileSHA256(out)
	if sum != m.SHA256 || n != m.Size {
		t.Fatal("checksum", sum, m.SHA256, n, m.Size)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	other, _, _ := ed25519.GenerateKey(nil)
	sig := Sign(priv, m)
	if !Verify([]ed25519.PublicKey{other, pub}, m, sig) || Verify([]ed25519.PublicKey{other}, m, sig) {
		t.Fatal("verify")
	}
	m2 := m
	m2.SHA256 = strings.Repeat("0", 64)
	if Verify([]ed25519.PublicKey{pub}, m2, sig) {
		t.Fatal("tampered manifest verified")
	}
	dest := filepath.Join(t.TempDir(), "x")
	if err := Extract(out, dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "VERSION")); string(b) != "0.6.0\n" {
		t.Fatalf("VERSION %q", b)
	}
	for _, gone := range []string{".git", "bin", "a.iso"} {
		if _, err := os.Stat(filepath.Join(dest, gone)); err == nil {
			t.Fatal(gone, "bundled")
		}
	}
	if st, _ := os.Stat(filepath.Join(dest, "scripts/p8")); st.Mode()&0o111 == 0 {
		t.Fatal("p8 not executable")
	}
	if _, err := Bundle(src, "../x", "", out); err == nil {
		t.Fatal("bad version")
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	for _, h := range []*tar.Header{
		{Name: "../evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		{Name: "/etc/evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		p := filepath.Join(t.TempDir(), "e.tar.gz")
		f, _ := os.Create(p)
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(h)
		if h.Size > 0 {
			_, _ = tw.Write([]byte("x"))
		}
		_ = tw.Close()
		_ = gz.Close()
		_ = f.Close()
		if err := Extract(p, filepath.Join(t.TempDir(), "d")); err == nil {
			t.Fatalf("%s accepted", h.Name)
		}
	}
}
