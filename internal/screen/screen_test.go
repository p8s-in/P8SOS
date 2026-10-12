package screen

import (
	"bytes"
	"image/png"
	"testing"
)

func TestToPNG(t *testing.T) {
	ppm := append([]byte("P6\n# qemu\n2 1\n255\n"), 255, 0, 0, 0, 0, 255)
	out, err := ToPNG(ppm)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _, _ := img.At(0, 0).RGBA(); r>>8 != 255 {
		t.Fatal("pixel 0 should be red")
	}
	if _, _, b, _ := img.At(1, 0).RGBA(); b>>8 != 255 {
		t.Fatal("pixel 1 should be blue")
	}
	if same, _ := ToPNG(out); !bytes.Equal(same, out) {
		t.Fatal("PNG should pass through")
	}
	for _, bad := range [][]byte{[]byte("P3\n1 1\n255\n"), []byte("P6\n2 2\n255\nxx"), []byte("P6\n99999 1\n255\n")} {
		if _, err := ToPNG(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
