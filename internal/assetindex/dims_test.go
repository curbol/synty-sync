package assetindex

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tgaHeader builds an 18-byte uncompressed-TGA header carrying w,h at the fixed
// little-endian offsets the format defines.
func tgaHeader(w, h int) []byte {
	b := make([]byte, 18)
	b[2] = 2 // uncompressed true-color
	binary.LittleEndian.PutUint16(b[12:], uint16(w))
	binary.LittleEndian.PutUint16(b[14:], uint16(h))
	b[16] = 32
	return b
}

// psdHeader builds a 26-byte PSD header with h,w at the fixed big-endian offsets.
func psdHeader(w, h int) []byte {
	b := make([]byte, 26)
	copy(b, "8BPS")
	binary.BigEndian.PutUint32(b[14:], uint32(h))
	binary.BigEndian.PutUint32(b[18:], uint32(w))
	return b
}

// bmpHeader builds a BMP header (BITMAPINFOHEADER) with w,h at the fixed
// little-endian offsets.
func bmpHeader(w, h int) []byte {
	b := make([]byte, 26)
	copy(b, "BM")
	binary.LittleEndian.PutUint32(b[18:], uint32(w))
	binary.LittleEndian.PutUint32(b[22:], uint32(h))
	return b
}

func TestImageDims(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		ext  string
		w, h int
	}{
		{"png", encodePNG(t, 3, 7), "png", 3, 7},
		{"jpeg", encodeJPEG(t, 5, 9), "jpeg", 5, 9},
		{"jpg", encodeJPEG(t, 11, 4), "jpg", 11, 4},
		{"gif", encodeGIF(t, 4, 2), "gif", 4, 2},
		{"tga", tgaHeader(6, 8), "tga", 6, 8},
		{"psd", psdHeader(6, 8), "psd", 6, 8},
		{"bmp", bmpHeader(6, 8), "bmp", 6, 8},
		{"non-image ext", encodePNG(t, 3, 7), "fbx", 0, 0},
		{"svg has no raster dims", []byte(`<svg width="10"/>`), "svg", 0, 0},
		{"garbage png bytes", []byte("not a png"), "png", 0, 0},
		{"truncated tga", []byte{0, 0, 2}, "tga", 0, 0},
		{"empty", nil, "png", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := imageDims(c.head, c.ext)
			if w != c.w || h != c.h {
				t.Errorf("imageDims(%s) = %dx%d, want %dx%d", c.ext, w, h, c.w, c.h)
			}
		})
	}
}
