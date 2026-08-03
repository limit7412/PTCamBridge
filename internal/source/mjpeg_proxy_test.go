package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// countingReporter records the connection transitions a driver reports.
type countingReporter struct {
	connects    atomic.Int64
	disconnects atomic.Int64
}

func (r *countingReporter) Connected(string)           { r.connects.Add(1) }
func (r *countingReporter) Disconnected(string, error) { r.disconnects.Add(1) }

// collect runs a driver until it has produced want frames or the deadline
// passes, then cancels it and returns what arrived.
func collect(t *testing.T, drv Source, want int, timeout time.Duration) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	frames := make(chan core.Frame, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = drv.Run(ctx, frames)
	}()

	var got [][]byte
	for len(got) < want {
		select {
		case f := <-frames:
			got = append(got, f.Data)
		case <-ctx.Done():
			cancel()
			<-done
			return got
		}
	}
	cancel()
	<-done
	return got
}

// serveMultipart writes an endless multipart MJPEG stream in the shape ESP32
// firmware produces.
func serveMultipart(t *testing.T, jpg []byte, boundary string, withContentLength bool, limit int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		for i := 0; limit == 0 || i < limit; i++ {
			fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\n", boundary)
			if withContentLength {
				fmt.Fprintf(w, "Content-Length: %d\r\n", len(jpg))
			}
			fmt.Fprint(w, "\r\n")
			if _, err := w.Write(jpg); err != nil {
				return
			}
			fmt.Fprint(w, "\r\n")
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
}

func TestMJPEGProxyReadsAMultipartStream(t *testing.T) {
	jpg := testJPEG(t)
	upstream := serveMultipart(t, jpg, "frame", true, 0)
	defer upstream.Close()

	reporter := &countingReporter{}
	drv, err := NewMJPEGProxy(MJPEGConfig{URL: upstream.URL}, discardLogger(), reporter)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}

	got := collect(t, drv, 3, 10*time.Second)
	if len(got) < 3 {
		t.Fatalf("got %d frames, want at least 3", len(got))
	}
	for i, f := range got {
		if !bytes.Equal(f, jpg) {
			t.Errorf("frame %d does not match the upstream image", i)
		}
	}
	if reporter.connects.Load() == 0 {
		t.Error("the driver never reported that it connected")
	}
}

// Firmware that omits Content-Length still has to work, since the body then
// runs to the next boundary.
func TestMJPEGProxyWithoutContentLength(t *testing.T) {
	jpg := testJPEG(t)
	upstream := serveMultipart(t, jpg, "frame", false, 0)
	defer upstream.Close()

	drv, err := NewMJPEGProxy(MJPEGConfig{URL: upstream.URL}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}
	got := collect(t, drv, 2, 10*time.Second)
	if len(got) < 2 || !bytes.Equal(got[0], jpg) {
		t.Fatalf("got %d frames, want at least 2 matching the upstream image", len(got))
	}
}

// Some cameras answer with a bare concatenated JPEG stream and no multipart
// wrapper at all.
func TestMJPEGProxyWithBareJPEGStream(t *testing.T) {
	jpg := testJPEG(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			if _, err := w.Write(jpg); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	drv, err := NewMJPEGProxy(MJPEGConfig{URL: upstream.URL}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}
	got := collect(t, drv, 3, 10*time.Second)
	if len(got) < 3 {
		t.Fatalf("got %d frames, want at least 3", len(got))
	}
}

// FR-5: an upstream that drops out must be picked up again without restarting
// the bridge.
func TestMJPEGProxyReconnectsAfterTheUpstreamCloses(t *testing.T) {
	jpg := testJPEG(t)
	var sessions atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessions.Add(1)
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpg))
		w.Write(jpg)
		fmt.Fprint(w, "\r\n")
		// Then hang up, which is what a rebooting camera looks like.
	}))
	defer upstream.Close()

	reporter := &countingReporter{}
	drv, err := NewMJPEGProxy(MJPEGConfig{URL: upstream.URL}, discardLogger(), reporter)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}

	// The first retry waits one second, so two frames means one reconnect.
	got := collect(t, drv, 2, 15*time.Second)
	if len(got) < 2 {
		t.Fatalf("got %d frames, want 2 (one per connection)", len(got))
	}
	if sessions.Load() < 2 {
		t.Errorf("the upstream saw %d connections, want at least 2", sessions.Load())
	}
	if reporter.disconnects.Load() == 0 {
		t.Error("the driver never reported the disconnection")
	}
}

// A stream that connects and then goes silent is not the same as a closed
// one; without a stall timeout the driver would wait forever.
func TestMJPEGProxyTimesOutOnASilentStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()

	reporter := &countingReporter{}
	drv, err := NewMJPEGProxy(MJPEGConfig{URL: upstream.URL, StallTimeout: 200 * time.Millisecond}, discardLogger(), reporter)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}
	collect(t, drv, 1, 3*time.Second)

	if reporter.disconnects.Load() == 0 {
		t.Error("a silent stream was never treated as a disconnection")
	}
}

func TestMJPEGProxyRejectsBadConfiguration(t *testing.T) {
	for _, url := range []string{"", "   ", "ftp://camera/", "://bad"} {
		if _, err := NewMJPEGProxy(MJPEGConfig{URL: url}, discardLogger(), nil); err == nil {
			t.Errorf("NewMJPEGProxy(%q) = nil error, want a rejection", url)
		}
	}
}

func TestMJPEGProxyReportsAnErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer upstream.Close()

	reporter := &countingReporter{}
	drv, err := NewMJPEGProxy(MJPEGConfig{URL: upstream.URL}, discardLogger(), reporter)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}
	if got := collect(t, drv, 1, 2*time.Second); len(got) != 0 {
		t.Errorf("got %d frames from a 404, want none", len(got))
	}
	if reporter.disconnects.Load() == 0 {
		t.Error("a 404 was not reported as a failure")
	}
}

func TestMJPEGProxyName(t *testing.T) {
	drv, err := NewMJPEGProxy(MJPEGConfig{URL: "http://example.invalid/"}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}
	if drv.Name() != "mjpeg" {
		t.Errorf("Name() = %q, want mjpeg", drv.Name())
	}
}

// A server that answers and refuses us is not going to change its mind on a
// retry, and Apply has to hear about it while it can still roll back.
func TestMJPEGProxyTreatsAPermanent4xxAsFatal(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer ts.Close()

			p, err := NewMJPEGProxy(MJPEGConfig{URL: ts.URL}, discardLogger(), nil)
			if err != nil {
				t.Fatalf("NewMJPEGProxy: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- p.Run(ctx, make(chan core.Frame, 4)) }()

			select {
			case err := <-done:
				var fatal *FatalError
				if !errors.As(err, &fatal) {
					t.Fatalf("Run returned %v, want a FatalError", err)
				}
			case <-ctx.Done():
				t.Fatalf("Run kept retrying a %d", code)
			}
		})
	}
}

// 429 and friends are the server asking us to come back, which is what the
// reconnect loop already does.
func TestMJPEGProxyKeepsRetryingATransient4xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	p, err := NewMJPEGProxy(MJPEGConfig{URL: ts.URL}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, make(chan core.Frame, 4)) }()

	select {
	case err := <-done:
		t.Fatalf("Run gave up on a 429: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	cancel()
	<-done
}

// A camera behind basic auth must not write its password into the log on every
// reconnect.
func TestMJPEGProxyKeepsCredentialsOutOfMessages(t *testing.T) {
	// Port 1 on loopback refuses immediately, so the connect error is the one
	// carrying the URL.
	p, err := NewMJPEGProxy(MJPEGConfig{URL: "http://camera:hunter2@127.0.0.1:1/"}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}
	if strings.Contains(p.safeURL, "hunter2") {
		t.Fatalf("safeURL = %q, want the password masked", p.safeURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = p.session(ctx, make(chan core.Frame, 1))
	if err == nil {
		t.Fatal("expected the connection to fail")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the password: %v", err)
	}
}

// url.Parse accepts "http:///stream"; the transport does not. Catching it here
// makes it a configuration error the bridge can roll back from, rather than
// something the reconnect loop retries forever.
func TestNewMJPEGProxyRejectsAURLWithNoHost(t *testing.T) {
	for _, raw := range []string{"http:///stream", "http://", "https:///"} {
		if _, err := NewMJPEGProxy(MJPEGConfig{URL: raw}, discardLogger(), nil); err == nil {
			t.Errorf("NewMJPEGProxy(%q) accepted a URL with no host", raw)
		}
	}
}

// The stall timer must not start until the response is in hand. Started before
// the request it spends its budget on connecting, so a camera that answers
// slowly but streams fine gets its body cancelled out from under it.
func TestMJPEGProxyStallTimerStartsAfterTheResponse(t *testing.T) {
	jpg := testJPEG(t)
	// Headers after most of the stall budget, then frames at a normal rate.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for {
			fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpg))
			if _, err := w.Write(jpg); err != nil {
				return
			}
			fmt.Fprint(w, "\r\n")
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}))
	defer ts.Close()

	p, err := NewMJPEGProxy(MJPEGConfig{
		URL:            ts.URL,
		ConnectTimeout: 2 * time.Second,
		StallTimeout:   500 * time.Millisecond,
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := make(chan core.Frame, 8)
	go func() { _ = p.session(ctx, out) }()

	select {
	case <-out:
	case <-ctx.Done():
		t.Fatal("no frame arrived: the stall budget was spent before the response")
	}
}

// Bytes arriving is not the same as frames arriving. An upstream that keeps
// the socket busy with data no parser can use would otherwise hold the session
// open forever while /healthz reported the source as lost.
func TestMJPEGProxyGivesUpOnDataThatNeverBecomesFrames(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for {
			// Well-formed enough to keep reading, never a whole image.
			if _, err := io.WriteString(w, "--frame\r\nContent-Type: image/jpeg\r\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	defer ts.Close()

	p, err := NewMJPEGProxy(MJPEGConfig{URL: ts.URL, StallTimeout: 500 * time.Millisecond}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.session(ctx, make(chan core.Frame, 4)) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the session ended without reporting the stall")
		}
		if !strings.Contains(err.Error(), "went quiet") {
			t.Errorf("error = %v, want the stall to be reported", err)
		}
	case <-ctx.Done():
		t.Fatal("the session stayed open on data that never became a frame")
	}
}

// Cancellation is not a failure, and the difference carries: the bridge reads
// a non-nil error from Run as the source having died under it, and stops
// counting the settings that produced it as working. A driver that reported
// its own shutdown as an error would make every pause look like a fault.
func TestMJPEGReturnsNilWhenCancelled(t *testing.T) {
	upstream := serveMultipart(t, testJPEG(t), "frame", true, 0)
	defer upstream.Close()

	p, err := NewMJPEGProxy(MJPEGConfig{URL: upstream.URL}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewMJPEGProxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan core.Frame, 4)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, frames) }()

	// Cancel once it is streaming, so the stop lands mid-read rather than
	// before the driver has done anything.
	select {
	case <-frames:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no frame arrived, so the cancellation would not be mid-stream")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
