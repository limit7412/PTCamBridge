package core

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
)

// DefaultQuality is used when a transform is active but no explicit JPEG
// quality was configured.
const DefaultQuality = 85

// DefaultMaxPixels bounds how large an image may be before it is decoded.
//
// A JPEG states its dimensions in a header a few bytes long, so
// source.max_frame_size -- which counts compressed bytes -- says nothing about
// what decoding will ask for: a structurally valid frame of well under a
// kilobyte can declare 65535x65535 and have the decoder allocate the buffers
// for it up front. A tracking camera sends 240x240, so this ceiling is far
// above anything real while keeping one frame from a hostile or broken
// upstream out of memory-exhaustion range.
const DefaultMaxPixels = 16 << 20

// Transform describes the optional geometry and re-encode step applied between
// the source and the stream. The zero value is a no-op, and a no-op transform
// forwards the input bytes untouched rather than decoding and re-encoding
// them, which avoids both the CPU cost and the generation loss.
type Transform struct {
	// Rotate is clockwise and must be 0, 90, 180 or 270.
	Rotate int
	// FlipH mirrors horizontally, FlipV vertically. Both are applied after
	// cropping and rotation.
	FlipH bool
	FlipV bool
	// CropSquare takes the largest centred square before rotating.
	CropSquare bool
	// Quality is the JPEG quality for the re-encode, 1-100. Zero means "do not
	// re-encode unless a geometry change forces it".
	Quality int
	// MaxPixels rejects an image larger than this before decoding it. Zero
	// selects DefaultMaxPixels.
	MaxPixels int
}

// Validate reports whether the transform can be applied.
func (t Transform) Validate() error {
	switch t.Rotate {
	case 0, 90, 180, 270:
	default:
		return fmt.Errorf("rotate must be 0, 90, 180 or 270, got %d", t.Rotate)
	}
	if t.Quality < 0 || t.Quality > 100 {
		return fmt.Errorf("quality must be between 0 and 100, got %d", t.Quality)
	}
	return nil
}

// IsNoop reports whether Apply would return its input unchanged.
func (t Transform) IsNoop() bool {
	return t.Rotate == 0 && !t.FlipH && !t.FlipV && !t.CropSquare && t.Quality == 0
}

// Apply runs the transform over an encoded JPEG and returns an encoded JPEG.
// A no-op transform returns the input slice itself.
func (t Transform) Apply(src []byte) ([]byte, error) {
	if t.IsNoop() {
		return src, nil
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	// Read the header first: the decoder sizes its buffers from the declared
	// dimensions, so anything over the ceiling has to be turned away before
	// Decode is allowed to allocate for it.
	if err := t.checkSize(src); err != nil {
		return nil, err
	}
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if t.CropSquare {
		img = cropCentredSquare(img)
	}
	if t.Rotate != 0 {
		img = rotate(img, t.Rotate)
	}
	if t.FlipH || t.FlipV {
		img = flip(img, t.FlipH, t.FlipV)
	}
	quality := t.Quality
	if quality == 0 {
		quality = DefaultQuality
	}
	var out bytes.Buffer
	out.Grow(len(src))
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return out.Bytes(), nil
}

// checkSize reads only the JPEG header and reports whether the image it
// describes is small enough to decode.
func (t Transform) checkSize(src []byte) error {
	limit := t.MaxPixels
	if limit <= 0 {
		limit = DefaultMaxPixels
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return fmt.Errorf("read the image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("image declares unusable dimensions %dx%d", cfg.Width, cfg.Height)
	}
	// Divide rather than multiply: the product of two declared dimensions can
	// overflow before it is ever compared.
	if cfg.Width > limit/cfg.Height {
		return fmt.Errorf("image is %dx%d, over the %d pixel limit", cfg.Width, cfg.Height, limit)
	}
	return nil
}

// remap builds a dstW x dstH image by pulling each destination pixel from the
// source coordinate returned by at. Every geometry operation here is a pure
// coordinate permutation, so one helper covers all of them.
func remap(src image.Image, dstW, dstH int, at func(dx, dy int) (int, int)) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	b := src.Bounds()
	for dy := 0; dy < dstH; dy++ {
		for dx := 0; dx < dstW; dx++ {
			sx, sy := at(dx, dy)
			dst.Set(dx, dy, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

// cropCentredSquare returns the largest centred square of img.
func cropCentredSquare(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == h {
		return img
	}
	size := w
	if h < size {
		size = h
	}
	offX, offY := (w-size)/2, (h-size)/2
	return remap(img, size, size, func(dx, dy int) (int, int) {
		return offX + dx, offY + dy
	})
}

// rotate turns img clockwise by 90, 180 or 270 degrees.
func rotate(img image.Image, degrees int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	switch degrees {
	case 90:
		return remap(img, h, w, func(dx, dy int) (int, int) { return dy, h - 1 - dx })
	case 180:
		return remap(img, w, h, func(dx, dy int) (int, int) { return w - 1 - dx, h - 1 - dy })
	case 270:
		return remap(img, h, w, func(dx, dy int) (int, int) { return w - 1 - dy, dx })
	default:
		return img
	}
}

// flip mirrors img on either or both axes.
func flip(img image.Image, horizontal, vertical bool) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	return remap(img, w, h, func(dx, dy int) (int, int) {
		sx, sy := dx, dy
		if horizontal {
			sx = w - 1 - dx
		}
		if vertical {
			sy = h - 1 - dy
		}
		return sx, sy
	})
}
