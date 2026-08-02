package core

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// encodeJPEG builds a small real JPEG so tests exercise the same marker
// structure a camera would produce.
func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 11), B: 0x40, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// injectAPP1 splices an APP1 segment carrying payload in right after the SOI,
// which is how an EXIF thumbnail smuggles a second SOI/EOI pair into a file.
func injectAPP1(t *testing.T, jpg, payload []byte) []byte {
	t.Helper()
	if len(jpg) < 2 {
		t.Fatalf("fixture too short")
	}
	segLen := len(payload) + 2
	out := make([]byte, 0, len(jpg)+segLen+2)
	out = append(out, jpg[:2]...)
	out = append(out, 0xFF, 0xE1, byte(segLen>>8), byte(segLen))
	out = append(out, payload...)
	out = append(out, jpg[2:]...)
	return out
}
