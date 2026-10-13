package isos

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBoot(t *testing.T) {
	for name, want := range map[string]string{"both": "bios+uefi", "bios": "bios", "uefi": "uefi", "data": "none"} {
		in, err := os.Open(filepath.Join("testdata", name+".iso.gz"))
		if err != nil {
			t.Fatal(err)
		}
		zr, _ := gzip.NewReader(in)
		p := filepath.Join(t.TempDir(), name+".iso")
		out, _ := os.Create(p)
		io.Copy(out, zr)
		out.Close()
		in.Close()
		if got := Boot(p); got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
	if Boot("/nonexistent.iso") != "none" {
		t.Error("missing file")
	}
}
