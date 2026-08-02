package core

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
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

// Without Content-Length the end of an image is found by walking its markers.
// A frame that happens to carry the boundary bytes -- inside an EXIF blob here
// -- must come through whole rather than being cut at the false delimiter.
func TestSplitMultipartKeepsBoundaryBytesInsideAFrame(t *testing.T) {
	jpg := injectAPP1(t, encodeJPEG(t, 16, 16), []byte("\r\n--b\r\nContent-Type: image/jpeg\r\n\r\n"))
	if !bytes.Contains(jpg, []byte("--b")) {
		t.Fatal("fixture does not contain the delimiter bytes")
	}

	var body []byte
	body = append(body, "--b\r\nContent-Type: image/jpeg\r\n\r\n"...)
	body = append(body, jpg...)
	body = append(body, "\r\n--b--"...)

	frames, _ := SplitMultipart(body, "b", 0)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if !bytes.Equal(frames[0], jpg) {
		t.Errorf("frame is %d bytes, want the original %d", len(frames[0]), len(jpg))
	}
}

// An upstream that sends a boundary and then dribbles header bytes without ever
// terminating them must not be able to grow the reader's buffer without bound.
func TestSplitMultipartBoundsUnterminatedHeaders(t *testing.T) {
	buf := []byte("--b\r\nX-Filler: ")
	buf = append(buf, bytes.Repeat([]byte("a"), 64<<10)...)

	frames, rest := SplitMultipart(buf, "b", 0)
	if len(frames) != 0 {
		t.Fatalf("got %d frames, want none", len(frames))
	}
	if len(rest) > maxPartHeaderBytes {
		t.Errorf("carried %d bytes forward, want at most %d", len(rest), maxPartHeaderBytes)
	}
}

// A run of fill bytes ending in EOI is structurally a JPEG of whatever length
// the run happens to be, so the ceiling has to be applied inside the run too --
// otherwise source.max_frame_size is bypassed and the frame reaches the hub.
func TestScanJPEGAppliesMaxSizeToFillBytes(t *testing.T) {
	padded := []byte{markerPrefix, markerSOI}
	padded = append(padded, bytes.Repeat([]byte{markerPrefix}, 40<<10)...)
	padded = append(padded, markerEOI)

	if n, err := ScanJPEG(padded, 4); err == nil {
		t.Errorf("ScanJPEG accepted a %d byte frame under a 4 byte limit", n)
	}

	frames, _ := SplitJPEGStream(padded, 4)
	if len(frames) != 0 {
		t.Errorf("SplitJPEGStream produced %d frames past the limit", len(frames))
	}

	// A generous limit still accepts it: fill bytes are legal.
	if _, err := ScanJPEG(padded, 1<<20); err != nil {
		t.Errorf("ScanJPEG rejected legal fill bytes under a large limit: %v", err)
	}
}

// max_frame_size can be set near MaxInt, and Content-Length comes off the
// wire, so adding the two before the bounds check wraps negative: the check
// passes and the slice that follows panics.
func TestSplitMultipartSurvivesAnEnormousContentLength(t *testing.T) {
	body := fmt.Sprintf("--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", math.MaxInt)
	frames, rest := SplitMultipart([]byte(body), "frame", math.MaxInt)

	if len(frames) != 0 {
		t.Errorf("got %d frames from a part whose body never arrived", len(frames))
	}
	if len(rest) == 0 {
		t.Error("the incomplete part was dropped rather than held for more data")
	}
}

// Resynchronising has to cost each byte one step in total, not one scan.
//
// A bare upstream can send an SOI every two bytes. Treating each of those as a
// harmless standalone marker made a scan walk to the size ceiling before
// failing, and resuming the search two bytes along made the next one do it all
// again: quadratic in the read. The driver cannot check its context inside
// that, so a source switch or a quit would be left waiting on it.
func TestSplitJPEGStreamResyncIsLinear(t *testing.T) {
	// Nothing but image starts, overrunning the ceiling.
	const size = 1 << 20
	buf := bytes.Repeat([]byte{markerPrefix, markerSOI}, size/2)

	done := make(chan int, 1)
	go func() {
		frames, _ := SplitJPEGStream(buf, size/2)
		done <- len(frames)
	}()

	select {
	case n := <-done:
		if n != 0 {
			t.Errorf("got %d frames from a run of image starts", n)
		}
	case <-time.After(2 * time.Second):
		// Linear is a few milliseconds here. Quadratic is upwards of
		// 10^11 steps, so this is not a close call to make.
		t.Fatal("resynchronising over 1 MiB of image starts did not finish in 2s")
	}
}

// A frame cut short by a dropped connection is followed immediately by the
// next one. Resynchronising past that SOI instead of onto it would throw the
// good frame away too, so an upstream alternating between broken and whole
// frames would publish nothing at all.
func TestSplitJPEGStreamRecoversTheFrameAfterATruncatedOne(t *testing.T) {
	good := encodeJPEG(t, 32, 32)
	// Cut inside the entropy-coded data, which is where a dropped connection
	// almost always lands: it is the bulk of the frame.
	truncated := good[:len(good)-8]
	buf := append(append([]byte{}, truncated...), good...)

	frames, _ := SplitJPEGStream(buf, 0)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want the whole one after the truncated one", len(frames))
	}
	if !bytes.Equal(frames[0], good) {
		t.Error("the recovered frame does not match the one that was sent")
	}
}
