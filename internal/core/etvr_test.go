package core

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func newTestParser(t *testing.T) ETVRParser {
	t.Helper()
	p, err := NewETVRParser(nil, 0)
	if err != nil {
		t.Fatalf("NewETVRParser: %v", err)
	}
	return p
}

func TestETVRParseConsecutivePackets(t *testing.T) {
	p := newTestParser(t)
	a := encodeJPEG(t, 16, 16)
	b := encodeJPEG(t, 24, 8)

	var stream []byte
	for _, payload := range [][]byte{a, b} {
		pkt, err := p.EncodePacket(payload)
		if err != nil {
			t.Fatalf("EncodePacket: %v", err)
		}
		stream = append(stream, pkt...)
	}

	frames, rest := p.Parse(stream)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0], a) || !bytes.Equal(frames[1], b) {
		t.Error("payloads do not round-trip through EncodePacket/Parse")
	}
	if len(rest) != 0 {
		t.Errorf("got %d leftover bytes, want none", len(rest))
	}
}

// A serial read can end anywhere, including in the middle of the preamble or
// the length field. The parser must hold those bytes rather than drop them.
func TestETVRParseHandlesSplitAtEveryOffset(t *testing.T) {
	p := newTestParser(t)
	payload := encodeJPEG(t, 16, 16)
	pkt, err := p.EncodePacket(payload)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}

	for split := 0; split <= len(pkt); split++ {
		var buf []byte
		var got [][]byte

		frames, rest := p.Parse(pkt[:split])
		got = append(got, frames...)
		buf = append(buf[:0], rest...)

		buf = append(buf, pkt[split:]...)
		frames, rest = p.Parse(buf)
		got = append(got, frames...)

		if len(got) != 1 {
			t.Fatalf("split at %d: got %d frames, want 1", split, len(got))
		}
		if !bytes.Equal(got[0], payload) {
			t.Fatalf("split at %d: payload mismatch", split)
		}
		if len(rest) != 0 {
			t.Fatalf("split at %d: got %d leftover bytes, want none", split, len(rest))
		}
	}
}

// Joining a stream that is already running means the first bytes seen are the
// tail of some earlier packet; they must be skipped, not misparsed.
func TestETVRParseSkipsLeadingGarbage(t *testing.T) {
	p := newTestParser(t)
	payload := encodeJPEG(t, 16, 16)
	pkt, err := p.EncodePacket(payload)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	stream := append([]byte{0x12, 0xFF, 0xA0, 0x34, 0x00, 0x99}, pkt...)

	frames, _ := p.Parse(stream)
	if len(frames) != 1 || !bytes.Equal(frames[0], payload) {
		t.Fatalf("got %d frames, want the single valid packet", len(frames))
	}
}

func TestETVRParseRejectsImplausibleLength(t *testing.T) {
	p, err := NewETVRParser(nil, 4096)
	if err != nil {
		t.Fatalf("NewETVRParser: %v", err)
	}
	payload := encodeJPEG(t, 16, 16)
	good, err := p.EncodePacket(payload)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}

	bad := append([]byte{}, DefaultETVRHeader...)
	bad = binary.LittleEndian.AppendUint16(bad, 60000) // beyond maxPayload
	bad = append(bad, 0xAA, 0xBB)

	frames, _ := p.Parse(append(bad, good...))
	if len(frames) != 1 || !bytes.Equal(frames[0], payload) {
		t.Fatalf("got %d frames, want only the packet with a sane length", len(frames))
	}
}

func TestETVRParseDropsCorruptPayload(t *testing.T) {
	p := newTestParser(t)
	payload := encodeJPEG(t, 16, 16)

	corrupt := bytes.Clone(payload)
	corrupt[1] = 0x00 // destroy the SOI so validation fails
	badPkt, err := p.EncodePacket(corrupt)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	goodPkt, err := p.EncodePacket(payload)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}

	frames, _ := p.Parse(append(badPkt, goodPkt...))
	if len(frames) != 1 || !bytes.Equal(frames[0], payload) {
		t.Fatalf("got %d frames, want only the valid one", len(frames))
	}
}

// Firmware revisions differ in the preamble, so it has to be overridable.
func TestETVRParseHonoursCustomHeader(t *testing.T) {
	p, err := NewETVRParser([]byte{0xAB, 0xCD}, 0)
	if err != nil {
		t.Fatalf("NewETVRParser: %v", err)
	}
	payload := encodeJPEG(t, 16, 16)
	pkt, err := p.EncodePacket(payload)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	if !bytes.HasPrefix(pkt, []byte{0xAB, 0xCD}) {
		t.Fatal("custom header was not written to the packet")
	}
	frames, _ := p.Parse(pkt)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
}

func TestNewETVRParserRejectsEmptyHeader(t *testing.T) {
	if _, err := NewETVRParser([]byte{}, 0); err == nil {
		t.Fatal("expected an error for an empty header")
	}
}

// The buffer a driver carries between reads must not grow without bound when
// the stream contains no headers at all.
func TestETVRParseBoundsLeftoverBytes(t *testing.T) {
	p := newTestParser(t)
	frames, rest := p.Parse(bytes.Repeat([]byte{0x01}, 8192))
	if len(frames) != 0 {
		t.Fatalf("got %d frames, want none", len(frames))
	}
	if len(rest) >= p.HeaderLen() {
		t.Fatalf("kept %d bytes, want fewer than the header length %d", len(rest), p.HeaderLen())
	}
}
