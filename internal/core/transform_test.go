package core

import (
	"bytes"
	"image/jpeg"
	"strings"
	"testing"
)

func decodeSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	return cfg.Width, cfg.Height
}

// A no-op transform must hand back the very same slice: re-encoding an
// untouched frame costs CPU and loses quality for nothing.
func TestTransformNoopForwardsInputUnchanged(t *testing.T) {
	src := encodeJPEG(t, 32, 16)
	var tr Transform

	if !tr.IsNoop() {
		t.Fatal("the zero Transform should be a no-op")
	}
	out, err := tr.Apply(src)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if &out[0] != &src[0] {
		t.Error("Apply copied the frame instead of forwarding it")
	}
}

func TestTransformRotateSwapsDimensions(t *testing.T) {
	src := encodeJPEG(t, 32, 16)
	cases := []struct {
		rotate int
		wantW  int
		wantH  int
	}{
		{90, 16, 32},
		{180, 32, 16},
		{270, 16, 32},
	}
	for _, tc := range cases {
		out, err := Transform{Rotate: tc.rotate}.Apply(src)
		if err != nil {
			t.Fatalf("rotate %d: %v", tc.rotate, err)
		}
		w, h := decodeSize(t, out)
		if w != tc.wantW || h != tc.wantH {
			t.Errorf("rotate %d gave %dx%d, want %dx%d", tc.rotate, w, h, tc.wantW, tc.wantH)
		}
	}
}

// Four 90 degree turns must land back on the original geometry, which catches
// a rotation whose coordinate mapping is transposed rather than rotated.
func TestTransformRotateFourTimesReturnsToStart(t *testing.T) {
	src := encodeJPEG(t, 32, 16)
	out := src
	for i := 0; i < 4; i++ {
		var err error
		out, err = Transform{Rotate: 90, Quality: 95}.Apply(out)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	w, h := decodeSize(t, out)
	if w != 32 || h != 16 {
		t.Fatalf("four turns gave %dx%d, want 32x16", w, h)
	}

	original, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("decode original: %v", err)
	}
	turned, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	// Compare a corner rather than every pixel: JPEG is lossy, but a
	// mis-mapped rotation moves content far enough to show up here.
	or, og, ob, _ := original.At(1, 1).RGBA()
	tr, tg, tb, _ := turned.At(1, 1).RGBA()
	const tolerance = 0x2000
	if absDiff(or, tr) > tolerance || absDiff(og, tg) > tolerance || absDiff(ob, tb) > tolerance {
		t.Errorf("pixel (1,1) moved: original (%d,%d,%d), after four turns (%d,%d,%d)", or, og, ob, tr, tg, tb)
	}
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestTransformCropSquare(t *testing.T) {
	src := encodeJPEG(t, 48, 16)
	out, err := Transform{CropSquare: true}.Apply(src)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if w, h := decodeSize(t, out); w != 16 || h != 16 {
		t.Errorf("crop gave %dx%d, want 16x16", w, h)
	}
}

func TestTransformFlipKeepsDimensions(t *testing.T) {
	src := encodeJPEG(t, 32, 16)
	out, err := Transform{FlipH: true, FlipV: true}.Apply(src)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if w, h := decodeSize(t, out); w != 32 || h != 16 {
		t.Errorf("flip gave %dx%d, want 32x16", w, h)
	}
}

// Quality alone is not a geometry change but still forces a re-encode.
func TestTransformQualityOnlyReencodes(t *testing.T) {
	src := encodeJPEG(t, 64, 64)
	tr := Transform{Quality: 20}
	if tr.IsNoop() {
		t.Fatal("a quality setting should not count as a no-op")
	}
	out, err := tr.Apply(src)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(out) >= len(src) {
		t.Errorf("re-encode at quality 20 produced %d bytes, expected fewer than the %d byte original", len(out), len(src))
	}
}

func TestTransformValidate(t *testing.T) {
	valid := []Transform{{}, {Rotate: 90}, {Rotate: 270, Quality: 100}}
	for _, tr := range valid {
		if err := tr.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", tr, err)
		}
	}
	invalid := []Transform{{Rotate: 45}, {Rotate: -90}, {Quality: 101}, {Quality: -1}}
	for _, tr := range invalid {
		if err := tr.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", tr)
		}
	}
}

func TestTransformRejectsNonJPEG(t *testing.T) {
	if _, err := (Transform{Rotate: 90}).Apply([]byte("not an image")); err == nil {
		t.Fatal("expected a decode error")
	}
}

// A JPEG states its size in a header a few bytes long, so a frame that passes
// the compressed-byte limit can still ask the decoder for gigabytes. The
// header has to be checked before Decode is allowed to allocate.
func TestTransformRejectsAnOversizedImageBeforeDecoding(t *testing.T) {
	// A real, small frame whose SOF is rewritten to claim 65535x65535. The
	// file stays well under any byte limit while telling the decoder to size
	// its buffers for four gigapixels.
	bomb := bytes.Clone(encodeJPEG(t, 16, 16))
	sof := bytes.Index(bomb, []byte{markerPrefix, 0xC0})
	if sof < 0 {
		t.Fatal("fixture has no baseline SOF0 to rewrite")
	}
	// FF C0, length(2), precision(1), height(2), width(2)
	copy(bomb[sof+5:sof+9], []byte{0xFF, 0xFF, 0xFF, 0xFF})

	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(bomb)); err != nil {
		t.Fatalf("the rewritten fixture is not parsable: %v", err)
	} else if cfg.Width != 65535 || cfg.Height != 65535 {
		t.Fatalf("fixture declares %dx%d, want 65535x65535", cfg.Width, cfg.Height)
	}

	tr := Transform{Rotate: 90}
	if _, err := tr.Apply(bomb); err == nil {
		t.Fatal("expected a 65535x65535 frame to be refused")
	} else if !strings.Contains(err.Error(), "pixel limit") {
		t.Errorf("error = %v, want it to name the pixel limit", err)
	}
}

// The ceiling must not get in the way of the images this actually carries.
func TestTransformAcceptsAnOrdinaryFrame(t *testing.T) {
	tr := Transform{Rotate: 90}
	if _, err := tr.Apply(encodeJPEG(t, 240, 240)); err != nil {
		t.Errorf("Apply on a 240x240 frame: %v", err)
	}
}

// MaxPixels is what the check is against, so a low one has to bite.
func TestTransformHonoursAConfiguredPixelLimit(t *testing.T) {
	tr := Transform{Rotate: 90, MaxPixels: 16}
	if _, err := tr.Apply(encodeJPEG(t, 32, 32)); err == nil {
		t.Error("expected a 32x32 frame to exceed a 16 pixel limit")
	}
}
