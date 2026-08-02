package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/status"
)

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func newTestServer(t *testing.T, opts Options) (*Server, *hub.Hub, *status.Tracker) {
	t.Helper()
	if opts.Hub == nil {
		opts.Hub = hub.New()
	}
	if opts.Status == nil {
		opts.Status = status.New()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Encoder.Boundary() == "" {
		enc, err := core.NewMultipartEncoder("", nil)
		if err != nil {
			t.Fatalf("NewMultipartEncoder: %v", err)
		}
		opts.Encoder = enc
	}
	if opts.Version == "" {
		opts.Version = "test"
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, opts.Hub, opts.Status
}

// publishUntilDone keeps frames flowing for the duration of a test.
func publishUntilDone(t *testing.T, h *hub.Hub, jpg []byte) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				h.Publish(core.Frame{Data: jpg})
			}
		}
	}()
}

// The client reads the socket looking for the boundary and for Content-Length.
// Chunked framing would interleave hex length lines with that structure, so
// this reads the raw bytes off the wire rather than trusting net/http's client
// to hide it.
func TestStreamIsNotChunkedOnTheWire(t *testing.T) {
	s, h, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	jpg := testJPEG(t)
	publishUntilDone(t, h, jpg)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	reader := bufio.NewReader(conn)
	var headers []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		headers = append(headers, line)
	}

	joined := strings.Join(headers, "\n")
	if !strings.HasPrefix(headers[0], "HTTP/1.1 200") {
		t.Fatalf("status line = %q, want 200", headers[0])
	}
	if strings.Contains(strings.ToLower(joined), "transfer-encoding: chunked") {
		t.Errorf("the response is chunked, which the client cannot parse:\n%s", joined)
	}
	if !strings.Contains(joined, "Content-Type: multipart/x-mixed-replace; boundary=paperbridge") {
		t.Errorf("Content-Type missing or wrong:\n%s", joined)
	}

	// The body must begin with the delimiter, not a chunk size line.
	body := make([]byte, len("--paperbridge\r\n"))
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "--paperbridge\r\n" {
		t.Errorf("body starts with %q, want the boundary delimiter", body)
	}
}

// The exact part layout is the compatibility contract with the client.
func TestStreamPartLayout(t *testing.T) {
	s, h, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	jpg := testJPEG(t)
	publishUntilDone(t, h, jpg)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "GET /stream HTTP/1.1\r\nHost: localhost\r\n\r\n")

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	want := fmt.Sprintf("--paperbridge\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpg))
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read part header: %v", err)
	}
	if string(got) != want {
		t.Fatalf("part header:\ngot:  %q\nwant: %q", got, want)
	}

	payload := make([]byte, len(jpg)+2)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("read part body: %v", err)
	}
	if !bytes.Equal(payload[:len(jpg)], jpg) {
		t.Error("the frame on the wire does not match the published frame")
	}
	if string(payload[len(jpg):]) != "\r\n" {
		t.Errorf("part body ends with %q, want CRLF", payload[len(jpg):])
	}
}

// A client that connects mid-stream should see the current frame at once
// rather than waiting for the next capture.
func TestStreamSendsTheLatestFrameImmediately(t *testing.T) {
	s, h, _ := newTestServer(t, Options{HoldOnSourceLoss: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	jpg := testJPEG(t)
	h.Publish(core.Frame{Data: jpg})

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if !bytes.HasPrefix(buf[:n], []byte("--paperbridge\r\n")) {
		t.Errorf("first bytes = %q, want a part delimiter", buf[:n])
	}
}

// With holding disabled, a source that stops must free the client so it can
// reconnect.
func TestStreamClosesAfterSourceLoss(t *testing.T) {
	s, h, _ := newTestServer(t, Options{HoldOnSourceLoss: false})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	h.Publish(core.Frame{Data: testJPEG(t)})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	start := time.Now()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil && ctx.Err() == nil {
		t.Fatalf("read stream: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the stream stayed open after the source stopped")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the stream took %s to close, expected roughly the loss timeout", elapsed)
	}
}

func TestSnapshot(t *testing.T) {
	s, h, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/snapshot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("before any frame: status = %d, want 503", resp.StatusCode)
	}

	jpg := testJPEG(t)
	h.Publish(core.Frame{Data: jpg})

	resp, err = http.Get(ts.URL + "/snapshot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, jpg) {
		t.Error("the snapshot does not match the published frame")
	}
}

func TestHealthz(t *testing.T) {
	s, h, tracker := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	get := func() (int, Health) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		var body Health
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.StatusCode, body
	}

	code, body := get()
	if code != http.StatusServiceUnavailable || body.OK {
		t.Errorf("disconnected: status %d ok=%v, want 503 and not ok", code, body.OK)
	}

	tracker.Connected("uvc")
	h.Publish(core.Frame{Data: testJPEG(t)})

	code, body = get()
	if code != http.StatusOK || !body.OK {
		t.Errorf("connected: status %d ok=%v reason=%q, want 200 and ok", code, body.OK, body.Reason)
	}

	tracker.SetPaused(true)
	code, body = get()
	if code != http.StatusServiceUnavailable || body.OK {
		t.Errorf("paused: status %d ok=%v, want 503 and not ok", code, body.OK)
	}
}

func TestStats(t *testing.T) {
	s, h, tracker := newTestServer(t, Options{Version: "1.2.3"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	tracker.Connected("serial")
	h.Publish(core.Frame{Data: testJPEG(t)})

	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	var body Stats
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", body.Version)
	}
	if body.Frames.Published != 1 {
		t.Errorf("Published = %d, want 1", body.Frames.Published)
	}
	if body.Source.Source != "serial" || !body.Source.Connected {
		t.Errorf("Source = %+v, want a connected serial source", body.Source)
	}
}

// fakeController records what the management API asked for.
type fakeController struct {
	cfg      config.Config
	switched string
	applied  bool
}

func (c *fakeController) Snapshot() config.Config { return c.cfg }

func (c *fakeController) Apply(_ context.Context, cfg config.Config) error {
	c.cfg = cfg
	c.applied = true
	return nil
}

func (c *fakeController) Switch(_ context.Context, sourceType string) error {
	c.switched = sourceType
	c.cfg.Source.Type = sourceType
	return nil
}

func (c *fakeController) Devices(context.Context) (Devices, error) { return Devices{}, nil }

func TestManagementAPIIsAbsentUnlessEnabled(t *testing.T) {
	s, _, _ := newTestServer(t, Options{Controller: &fakeController{}, EnableAdmin: false})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// The catch-all root route means an unregistered admin path 404s rather
	// than streaming, which is the behaviour worth pinning down.
	resp, err := http.Get(ts.URL + "/api/v1/config")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the API is disabled", resp.StatusCode)
	}
}

func TestManagementAPI(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default()}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	t.Run("get config", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/config")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()

		var got config.Config
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Server.Listen != config.Default().Server.Listen {
			t.Errorf("Listen = %q, want the default", got.Server.Listen)
		}
	})

	t.Run("put config", func(t *testing.T) {
		updated := config.Default()
		updated.Source.Type = config.SourceMJPEG
		updated.Source.MJPEG.URL = "http://192.168.1.50/"
		body, _ := json.Marshal(updated)

		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d: %s", resp.StatusCode, out)
		}
		if !ctrl.applied || ctrl.cfg.Source.Type != config.SourceMJPEG {
			t.Errorf("controller did not receive the new settings: %+v", ctrl.cfg.Source)
		}
	})

	t.Run("put invalid config is rejected", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", strings.NewReader(`{"source":{"type":"nonsense"}}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("switch source", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(`{"type":"serial"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if ctrl.switched != "serial" {
			t.Errorf("switched to %q, want serial", ctrl.switched)
		}
	})

	t.Run("devices", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/devices")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
}

func TestUnknownPathIsNotFound(t *testing.T) {
	s, _, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// A client that stops reading must not slow down the others.
func TestManyStreamClients(t *testing.T) {
	s, h, _ := newTestServer(t, Options{HoldOnSourceLoss: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	publishUntilDone(t, h, testJPEG(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		defer resp.Body.Close()

		if i == 0 {
			// Leave this one unread for the whole test.
			continue
		}
		buf := make([]byte, 64)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatalf("client %d read: %v", i, err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.Subscribers() == 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Subscribers() = %d, want 3", h.Subscribers())
}
