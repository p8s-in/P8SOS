// Package screen turns QEMU screenshots (PPM) into PNG for the browser.
package screen

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"strconv"
)

// ToPNG converts a binary PPM (P6, 8-bit) to PNG; PNG input is passed through.
func ToPNG(b []byte) ([]byte, error) {
	if bytes.HasPrefix(b, []byte("\x89PNG")) {
		return b, nil
	}
	img, err := decodePPM(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decodePPM(r io.Reader) (image.Image, error) {
	br := bufio.NewReader(r)
	var fields []int
	magic, err := token(br)
	if err != nil || magic != "P6" {
		return nil, errors.New("screenshot is not a P6 PPM")
	}
	for len(fields) < 3 {
		t, err := token(br)
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(t)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("bad PPM header %q", t)
		}
		fields = append(fields, n)
	}
	w, h, max := fields[0], fields[1], fields[2]
	if max != 255 || w > 8192 || h > 8192 {
		return nil, fmt.Errorf("unsupported PPM %dx%d max %d", w, h, max)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	row := make([]byte, w*3)
	for y := 0; y < h; y++ {
		if _, err := io.ReadFull(br, row); err != nil {
			return nil, err
		}
		for x := 0; x < w; x++ {
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = row[3*x], row[3*x+1], row[3*x+2], 255
		}
	}
	return img, nil
}

// token reads the next header token, skipping whitespace and # comments, and
// consumes exactly one whitespace byte after it (the PPM rule before pixels).
func token(br *bufio.Reader) (string, error) {
	var sb []byte
	for {
		c, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		switch {
		case c == '#' && len(sb) == 0:
			if _, err := br.ReadString('\n'); err != nil {
				return "", err
			}
		case c == ' ' || c == '\n' || c == '\r' || c == '\t':
			if len(sb) > 0 {
				return string(sb), nil
			}
		default:
			sb = append(sb, c)
		}
	}
}
