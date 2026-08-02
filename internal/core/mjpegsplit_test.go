package core

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// A thumbnail inside an APP1 segment contains its own SOI/EOI pair. A scanner
// that searched for 0xFF 0xD9 would cut the frame short here.
func TestScanJPEGIgnoresEOIInsideASegment(t *testing.T) {
	base := encodeJPEG(t, 32, 32)
	withThumb := injectAPP1(t, base, []byte{0xFF, 0xD8, 0xFF, 0xD9, 0x00, 0x11})

	n, err := ScanJPEG(withThumb, 0)
	if err != nil {
		t.Fatalf("ScanJPEG: %v", err)
	}
	if n != len(withThumb) {
		t.Fatalf("scanned %d bytes, want the whole %d byte image", n, len(withThumb))
	}
}

func TestScanJPEGReportsIncompleteAndInvalid(t *testing.T) {
	jpg := encodeJPEG(t, 16, 16)

	if _, err := ScanJPEG(jpg[:len(jpg)-3], 0); !errors.Is(err, ErrIncompleteJPEG) {
		t.Errorf("truncated image: got %v, want ErrIncompleteJPEG", err)
	}
	if _, err := ScanJPEG([]byte{0x00, 0x11, 0x22, 0x33}, 0); !errors.Is(err, ErrNotJPEG) {
		t.Errorf("non-image bytes: got %v, want ErrNotJPEG", err)
	}
	if _, err := ScanJPEG(jpg, 8); !errors.Is(err, ErrNotJPEG) {
		t.Errorf("image beyond maxSize: got %v, want ErrNotJPEG", err)
	}
}

// ScanJPEG must stop at the end of the first image even when more data follows,
// which is what makes back-to-back frames separable.
func TestScanJPEGStopsAtFirstImage(t *testing.T) {
	a := encodeJPEG(t, 16, 16)
	b := encodeJPEG(t, 16, 16)

	n, err := ScanJPEG(append(bytes.Clone(a), b...), 0)
	if err != nil {
		t.Fatalf("ScanJPEG: %v", err)
	}
	if n != len(a) {
		t.Fatalf("scanned %d bytes, want %d", n, len(a))
	}
}

func TestSplitJPEGStreamAcrossArbitraryChunks(t *testing.T) {
	a := encodeJPEG(t, 16, 16)
	b := encodeJPEG(t, 20, 12)
	stream := append(bytes.Clone(a), b...)

	for _, chunk := range []int{1, 7, 64, 4096} {
		t.Run(fmt.Sprintf("chunk=%d", chunk), func(t *testing.T) {
			var buf, rest []byte
			var got [][]byte
			for off := 0; off < len(stream); off += chunk {
				end := min(off+chunk, len(stream))
				buf = append(append(buf[:0], rest...), stream[off:end]...)
				var frames [][]byte
				frames, rest = SplitJPEGStream(buf, 0)
				got = append(got, frames...)
				rest = bytes.Clone(rest)
			}
			if len(got) != 2 {
				t.Fatalf("got %d frames, want 2", len(got))
			}
			if !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) {
				t.Error("frames do not match the originals")
			}
		})
	}
}

func TestSplitMultipartUsesContentLength(t *testing.T) {
	jpg := encodeJPEG(t, 16, 16)
	enc, err := NewMultipartEncoder("myboundary", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	body := enc.AppendPart(nil, jpg)
	body = enc.AppendPart(body, jpg)
	body = append(body, "--myboundary--\r\n"...)

	frames, _ := SplitMultipart(body, "myboundary", 0)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	for i, f := range frames {
		if !bytes.Equal(f, jpg) {
			t.Errorf("frame %d does not match the original", i)
		}
	}
}

// Some firmware omits Content-Length; the body then runs up to the next
// boundary and the trailing CRLF must not be counted as image data.
func TestSplitMultipartWithoutContentLength(t *testing.T) {
	jpg := encodeJPEG(t, 16, 16)
	var body []byte
	for i := 0; i < 2; i++ {
		body = append(body, "--b\r\nContent-Type: image/jpeg\r\n\r\n"...)
		body = append(body, jpg...)
		body = append(body, "\r\n"...)
	}
	body = append(body, "--b--"...)

	frames, _ := SplitMultipart(body, "b", 0)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0], jpg) {
		t.Error("frame does not match the original")
	}
}

func TestSplitMultipartAcrossArbitraryChunks(t *testing.T) {
	jpg := encodeJPEG(t, 16, 16)
	enc, err := NewMultipartEncoder("bnd", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	var stream []byte
	for i := 0; i < 3; i++ {
		stream = enc.AppendPart(stream, jpg)
	}

	var buf, rest []byte
	var got [][]byte
	for off := 0; off < len(stream); off += 13 {
		end := min(off+13, len(stream))
		buf = append(append(buf[:0], rest...), stream[off:end]...)
		var frames [][]byte
		frames, rest = SplitMultipart(buf, "bnd", 0)
		got = append(got, frames...)
		rest = bytes.Clone(rest)
	}
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
}

func TestSplitMultipartDropsCorruptPart(t *testing.T) {
	jpg := encodeJPEG(t, 16, 16)
	corrupt := bytes.Clone(jpg)
	corrupt[1] = 0x00

	var body []byte
	for _, payload := range [][]byte{corrupt, jpg} {
		body = append(body, fmt.Sprintf("--b\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(payload))...)
		body = append(body, payload...)
		body = append(body, "\r\n"...)
	}
	body = append(body, "--b--"...)

	frames, _ := SplitMultipart(body, "b", 0)
	if len(frames) != 1 || !bytes.Equal(frames[0], jpg) {
		t.Fatalf("got %d frames, want only the valid one", len(frames))
	}
}

func TestBoundaryFromContentType(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"multipart/x-mixed-replace; boundary=frame", "frame", true},
		{`multipart/x-mixed-replace;boundary="my bound"`, "my bound", true},
		{"multipart/x-mixed-replace; boundary=--dashes", "dashes", true},
		{"MULTIPART/X-MIXED-REPLACE; BOUNDARY=up", "up", true},
		{"image/jpeg", "", false},
		{"multipart/x-mixed-replace", "", false},
	}
	for _, tc := range cases {
		got, ok := BoundaryFromContentType(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("BoundaryFromContentType(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
