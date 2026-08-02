// Package core holds the side-effect free logic of PaperBridge: JPEG
// validation, wire protocol parsing, multipart encoding and image transforms.
//
// Nothing in this package may touch a socket, a serial port, a child process
// or the filesystem. That restriction is what makes the protocol handling
// testable without a camera attached.
package core

import (
	"errors"
	"fmt"
	"time"
)

// JPEG marker bytes used by the structural scanner.
const (
	markerPrefix = 0xFF
	markerSOI    = 0xD8
	markerEOI    = 0xD9
	markerSOS    = 0xDA
	markerTEM    = 0x01
	markerRST0   = 0xD0
	markerRST7   = 0xD7
)

// DefaultMaxFrameSize bounds a single JPEG frame. Mouth tracking cameras emit
// small images (240x240 is typical), so anything past a few MiB means the
// stream has desynchronised rather than that a huge frame really arrived.
const DefaultMaxFrameSize = 4 << 20

// MinJPEGSize is SOI + EOI; anything shorter cannot be a JPEG at all.
const MinJPEGSize = 4

// Errors reported by ValidateJPEG.
var (
	ErrFrameTooSmall = errors.New("frame too small to be a JPEG")
	ErrFrameTooLarge = errors.New("frame exceeds the maximum frame size")
	ErrMissingSOI    = errors.New("frame does not start with a JPEG SOI marker")
	ErrMissingEOI    = errors.New("frame does not end with a JPEG EOI marker")
)

// Frame is one validated JPEG image. Data is treated as immutable once the
// frame has been published: producers must hand over a buffer they no longer
// write to, because every subscriber shares the same backing array.
type Frame struct {
	Data     []byte
	Seq      uint64
	RecvedAt time.Time
}

// Size reports the encoded length of the frame in bytes.
func (f Frame) Size() int { return len(f.Data) }

// ValidateJPEG checks that data looks like a complete standalone JPEG.
// A maxSize of zero selects DefaultMaxFrameSize.
func ValidateJPEG(data []byte, maxSize int) error {
	if maxSize <= 0 {
		maxSize = DefaultMaxFrameSize
	}
	if len(data) < MinJPEGSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooSmall, len(data))
	}
	if len(data) > maxSize {
		return fmt.Errorf("%w: %d > %d bytes", ErrFrameTooLarge, len(data), maxSize)
	}
	if data[0] != markerPrefix || data[1] != markerSOI {
		return ErrMissingSOI
	}
	if data[len(data)-2] != markerPrefix || data[len(data)-1] != markerEOI {
		return ErrMissingEOI
	}
	return nil
}

// IsJPEG reports whether data passes ValidateJPEG with the default bound.
func IsJPEG(data []byte) bool { return ValidateJPEG(data, 0) == nil }
