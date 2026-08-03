package core

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// DefaultETVRHeader is the packet preamble emitted by OpenIris / ETVR style
// wired firmware: a 0xFF 0xA0 packet header followed by a 0xFF 0xA1 frame
// marker. Firmware revisions differ here, so the value is configurable rather
// than hard-coded into the parser.
var DefaultETVRHeader = []byte{0xFF, 0xA0, 0xFF, 0xA1}

// etvrLengthBytes is the size of the little-endian payload length field that
// follows the header.
const etvrLengthBytes = 2

// ErrEmptyHeader is returned when an ETVRParser is built without a preamble to
// search for, which would make resynchronisation impossible.
var ErrEmptyHeader = errors.New("etvr: header must not be empty")

// ETVRParser extracts JPEG frames from the wired serial packet format:
//
//	offset 0  len 2  header      (0xFF 0xA0)
//	offset 2  len 2  frame mark  (0xFF 0xA1)
//	offset 4  len 2  payload len (uint16, little-endian)
//	offset 6  len N  JPEG data
//
// The zero value is not usable; build one with NewETVRParser.
type ETVRParser struct {
	header     []byte
	maxPayload int
}

// NewETVRParser returns a parser searching for the given preamble. A nil or
// empty header falls back to DefaultETVRHeader; a maxPayload of zero selects
// DefaultMaxFrameSize.
func NewETVRParser(header []byte, maxPayload int) (ETVRParser, error) {
	if header == nil {
		header = DefaultETVRHeader
	}
	if len(header) == 0 {
		return ETVRParser{}, ErrEmptyHeader
	}
	if maxPayload <= 0 {
		maxPayload = DefaultMaxFrameSize
	}
	h := make([]byte, len(header))
	copy(h, header)
	return ETVRParser{header: h, maxPayload: maxPayload}, nil
}

// HeaderLen reports the length of the preamble this parser searches for.
func (p ETVRParser) HeaderLen() int { return len(p.header) }

// Parse consumes whole packets from buf and returns the JPEG payloads found,
// along with the bytes that could not be consumed yet.
//
// Returned frames are copies and are safe to retain. rest aliases buf, so a
// caller reusing its read buffer must copy or compact it (the usual
// buf = append(buf[:0], rest...) works, since copy handles overlap).
//
// Bytes preceding the first header are discarded, which lets a reader join a
// stream that is already running. A packet whose length field is implausible
// or whose payload is not a valid JPEG is dropped, and the scan restarts one
// byte past that header so a false positive inside image data cannot wedge the
// parser.
func (p ETVRParser) Parse(buf []byte) (frames [][]byte, rest []byte) {
	pos := 0
	for {
		idx := bytes.Index(buf[pos:], p.header)
		if idx < 0 {
			// No header in flight. Keep only the tail that could still be the
			// beginning of a header split across two reads.
			keep := len(p.header) - 1
			if keep > len(buf)-pos {
				keep = len(buf) - pos
			}
			return frames, buf[len(buf)-keep:]
		}
		start := pos + idx
		lenAt := start + len(p.header)
		if lenAt+etvrLengthBytes > len(buf) {
			// Header seen but the length field has not arrived yet.
			return frames, buf[start:]
		}
		payloadLen := int(binary.LittleEndian.Uint16(buf[lenAt : lenAt+etvrLengthBytes]))
		if payloadLen < MinJPEGSize || payloadLen > p.maxPayload {
			pos = start + 1
			continue
		}
		payloadAt := lenAt + etvrLengthBytes
		end := payloadAt + payloadLen
		if end > len(buf) {
			// Payload still in flight; wait for the rest of it.
			return frames, buf[start:]
		}
		payload := buf[payloadAt:end]
		if ValidateJPEG(payload, p.maxPayload) != nil {
			pos = start + 1
			continue
		}
		frames = append(frames, bytes.Clone(payload))
		pos = end
	}
}

// EncodePacket builds a wire packet for the given JPEG payload. It exists so
// tests and fixtures can produce byte-exact input for Parse.
func (p ETVRParser) EncodePacket(payload []byte) ([]byte, error) {
	if len(payload) > 0xFFFF {
		return nil, fmt.Errorf("etvr: payload of %d bytes exceeds the 16-bit length field", len(payload))
	}
	out := make([]byte, 0, len(p.header)+etvrLengthBytes+len(payload))
	out = append(out, p.header...)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(payload)))
	out = append(out, payload...)
	return out, nil
}
