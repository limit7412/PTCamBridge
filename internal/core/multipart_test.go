package core

import (
	"bytes"
	"fmt"
	"mime"
	"strings"
	"testing"
)

// The client finds frame boundaries from the delimiter line and sizes each
// frame from Content-Length, so this layout is checked byte for byte.
func TestEncodePartByteLayout(t *testing.T) {
	enc, err := NewMultipartEncoder("", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	payload := []byte{0xFF, 0xD8, 0x01, 0x02, 0xFF, 0xD9}

	got := enc.EncodePart(payload)
	want := append([]byte("--paperbridge\r\nContent-Type: image/jpeg\r\nContent-Length: 6\r\n\r\n"), payload...)
	want = append(want, "\r\n"...)

	if !bytes.Equal(got, want) {
		t.Errorf("part layout mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestEncodePartContentLengthMatchesPayload(t *testing.T) {
	enc, err := NewMultipartEncoder("bnd", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	for _, size := range []int{4, 100, 65535} {
		payload := make([]byte, size)
		payload[0], payload[1] = 0xFF, 0xD8
		payload[size-2], payload[size-1] = 0xFF, 0xD9

		part := enc.EncodePart(payload)
		header, body, found := bytes.Cut(part, []byte("\r\n\r\n"))
		if !found {
			t.Fatalf("size %d: no blank line separating headers from body", size)
		}
		if !bytes.Contains(append(header, "\r\n"...), []byte(fmt.Sprintf("Content-Length: %d\r\n", size))) {
			t.Errorf("size %d: Content-Length missing or wrong in %q", size, header)
		}
		if len(body) != size+2 {
			t.Errorf("size %d: body is %d bytes, want payload plus a trailing CRLF", size, len(body))
		}
		if !bytes.Equal(body[:size], payload) {
			t.Errorf("size %d: payload was altered", size)
		}
	}
}

func TestEncodePartIncludesExtraHeaders(t *testing.T) {
	enc, err := NewMultipartEncoder("bnd", []StreamHeader{{Name: "X-Timestamp", Value: "0"}})
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	part := string(enc.EncodePart([]byte{0xFF, 0xD8, 0xFF, 0xD9}))
	if !strings.Contains(part, "X-Timestamp: 0\r\n") {
		t.Errorf("extra header missing from %q", part)
	}
	if strings.Index(part, "X-Timestamp") > strings.Index(part, "\r\n\r\n") {
		t.Error("extra header was written after the header block ended")
	}
}

func TestContentType(t *testing.T) {
	enc, err := NewMultipartEncoder("frame", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	if got, want := enc.ContentType(), "multipart/x-mixed-replace; boundary=frame"; got != want {
		t.Errorf("ContentType() = %q, want %q", got, want)
	}
}

func TestValidateBoundary(t *testing.T) {
	valid := []string{"paperbridge", "frame-1", "a.b_c", "0123456789"}
	for _, s := range valid {
		if err := ValidateBoundary(s); err != nil {
			t.Errorf("ValidateBoundary(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{"", "has space", "quote\"", "new\nline", strings.Repeat("a", 71)}
	for _, s := range invalid {
		if err := ValidateBoundary(s); err == nil {
			t.Errorf("ValidateBoundary(%q) = nil, want an error", s)
		}
	}
}

func TestNewMultipartEncoderRejectsBadExtraHeaders(t *testing.T) {
	bad := [][]StreamHeader{
		{{Name: "", Value: "x"}},
		{{Name: "Bad: Name", Value: "x"}},
		{{Name: "X", Value: "line\r\nInjected: 1"}},
	}
	for _, headers := range bad {
		if _, err := NewMultipartEncoder("bnd", headers); err == nil {
			t.Errorf("NewMultipartEncoder(%+v) = nil error, want a rejection", headers)
		}
	}
}

// Encoding then splitting is the shape of the proxy path: normalise whatever
// the upstream sent into our own framing.
func TestEncodeSplitRoundTrip(t *testing.T) {
	jpg := encodeJPEG(t, 24, 24)
	enc, err := NewMultipartEncoder("rt", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	frames, rest := SplitMultipart(enc.EncodePart(jpg), "rt", 0)
	if len(frames) != 1 || !bytes.Equal(frames[0], jpg) {
		t.Fatalf("got %d frames, want the original image back", len(frames))
	}
	if len(rest) > len("--rt") {
		t.Errorf("leftover of %d bytes is larger than a partial delimiter", len(rest))
	}
}

// RFC 2046 allows delimiter characters that RFC 2045 does not allow in a bare
// parameter token. Written unquoted, such a boundary makes the whole media
// type unparsable and a compliant client cannot find the delimiter at all.
func TestContentTypeQuotesANonTokenBoundary(t *testing.T) {
	enc, err := NewMultipartEncoder("a:b/c", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}

	ct := enc.ContentType()
	if want := `multipart/x-mixed-replace; boundary="a:b/c"`; ct != want {
		t.Errorf("ContentType() = %q, want %q", ct, want)
	}

	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("ParseMediaType(%q): %v", ct, err)
	}
	if mediaType != "multipart/x-mixed-replace" || params["boundary"] != "a:b/c" {
		t.Errorf("parsed %q %v, want the boundary back intact", mediaType, params)
	}
	if got, ok := BoundaryFromContentType(ct); !ok || got != "a:b/c" {
		t.Errorf("BoundaryFromContentType = %q (ok=%v), want a:b/c", got, ok)
	}
}
