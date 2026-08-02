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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/source"
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
	// applyErr is what Apply returns, for the failure mappings.
	applyErr error
	// devices is what Devices returns.
	devices Devices
	// devicesCalls counts enumerations, which cost a subprocess on Windows.
	devicesCalls int
}

func (c *fakeController) Snapshot() config.Config { return c.cfg }

func (c *fakeController) Apply(_ context.Context, cfg config.Config) error {
	if c.applyErr != nil {
		return c.applyErr
	}
	c.cfg = cfg
	c.applied = true
	return nil
}

func (c *fakeController) Switch(_ context.Context, sourceType string) error {
	c.switched = sourceType
	c.cfg.Source.Type = sourceType
	return nil
}

func (c *fakeController) Devices(context.Context) Devices {
	c.devicesCalls++
	return c.devices
}

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
		req.Header.Set("Content-Type", "application/json")
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
		req.Header.Set("Content-Type", "application/json")
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

// Listening on loopback is not a control by itself: any page the user visits
// can reach 127.0.0.1, and a form-style POST gets there without a preflight.
// Such a request must not be able to take the camera away from the tracker.
func TestManagementAPIRejectsRequestsAPageCouldSend(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		origin      string
		host        string
		want        int
	}{
		{
			// The bypass: text/plain is a simple request, so no preflight is
			// ever made and the Origin check below never gets a chance to run.
			name:        "simple post with a plain text body",
			contentType: "text/plain;charset=UTF-8",
			want:        http.StatusUnsupportedMediaType,
		},
		{
			name:        "form post",
			contentType: "application/x-www-form-urlencoded",
			want:        http.StatusUnsupportedMediaType,
		},
		{
			// A page that does send JSON triggers a preflight, and this is what
			// the preflight fails on.
			name:        "json post from a foreign page",
			contentType: "application/json",
			origin:      "https://evil.example",
			want:        http.StatusForbidden,
		},
		{
			// DNS rebinding: the name resolves to loopback but travels in Host.
			name:        "rebound host name",
			contentType: "application/json",
			host:        "evil.example",
			want:        http.StatusForbidden,
		},
		{
			name:        "json post from a local page",
			contentType: "application/json",
			origin:      "http://127.0.0.1:18080",
			want:        http.StatusOK,
		},
		{
			// curl and the tray send no Origin at all.
			name:        "json post with no origin",
			contentType: "application/json",
			want:        http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &fakeController{cfg: config.Default()}
			s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/source", strings.NewReader(`{"type":"serial"}`))
			req.Header.Set("Content-Type", tc.contentType)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.host != "" {
				req.Host = tc.host
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				out, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, out)
			}
			if switched := ctrl.switched != ""; switched != (tc.want == http.StatusOK) {
				t.Errorf("controller switched = %v, but the request returned %d", switched, resp.StatusCode)
			}
		})
	}
}

// A GET needs no preflight and carries no Origin when a page asks for it as a
// subresource -- <img src="http://127.0.0.1:18080/api/v1/devices">. The page
// cannot read the answer, but the answer is not free: enumerating devices runs
// ffmpeg and waits for it, so a page cycling URLs keeps starting processes on
// the machine. Sec-Fetch-Site says where the request came from and no page can
// forge it or stop the browser sending it.
func TestDeviceEnumerationRefusesACrossSiteGet(t *testing.T) {
	cases := []struct {
		name string
		site string
		want int
	}{
		{name: "a subresource on someone else's page", site: "cross-site", want: http.StatusForbidden},
		{name: "another port on this machine", site: "same-site", want: http.StatusForbidden},
		{name: "a page served by the bridge", site: "same-origin", want: http.StatusOK},
		{name: "the address typed in", site: "none", want: http.StatusOK},
		{name: "curl, which sends no such header", site: "", want: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &fakeController{cfg: config.Default()}
			s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/devices", nil)
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				out, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, out)
			}
			if listed := ctrl.devicesCalls > 0; listed != (tc.want == http.StatusOK) {
				t.Errorf("the controller was asked to enumerate = %v, but the request returned %d", listed, resp.StatusCode)
			}
		})
	}
}

// A change that took effect but could not be written is this side's failure,
// not the caller's, and the two must not report the same way.
func TestManagementAPIReportsASaveFailureSeparately(t *testing.T) {
	ctrl := &fakeController{
		cfg:      config.Default(),
		applyErr: fmt.Errorf("%w to /nowhere: read-only file system", config.ErrNotSaved),
	}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body, _ := json.Marshal(config.Default())
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a persistence failure", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "could not be saved") {
		t.Errorf("body = %q, want it to say the settings were not saved", out)
	}
}

// The boundary is the knob for matching a PaperTracker release, so changing it
// has to reach the wire without a restart.
func TestSetStreamOptionsAppliesToNewStreams(t *testing.T) {
	s, frames, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	frames.Publish(core.Frame{Data: testJPEG(t)})

	updated, err := core.NewMultipartEncoder("othermark", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	s.SetStreamOptions(updated, false)

	resp, err := http.Get(ts.URL + "/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	boundary, ok := core.BoundaryFromContentType(resp.Header.Get("Content-Type"))
	if !ok || boundary != "othermark" {
		t.Fatalf("boundary = %q (ok=%v), want othermark", boundary, ok)
	}

	buf := make([]byte, 64)
	n, _ := io.ReadFull(resp.Body, buf)
	if !bytes.Contains(buf[:n], []byte("--othermark")) {
		t.Errorf("first part = %q, want it delimited by the new boundary", buf[:n])
	}
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

// The hub keeps the last image indefinitely. Replaying it to every reconnect
// while the camera is down would feed the tracker the same stale mouth shape
// over and over, so a frame older than the loss timeout is withheld.
func TestStreamWithholdsAStaleOpeningFrame(t *testing.T) {
	frames := hub.New()
	s, _, _ := newTestServer(t, Options{Hub: frames})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	frames.Publish(core.Frame{
		Data:     testJPEG(t),
		RecvedAt: time.Now().Add(-time.Minute),
	})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	read := make(chan int, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := io.ReadFull(resp.Body, buf)
		read <- n
	}()

	select {
	case n := <-read:
		t.Errorf("the stream opened with %d bytes, want the stale frame withheld", n)
	case <-time.After(500 * time.Millisecond):
	}
}

// A frame that just arrived is still what a reconnecting client should see
// straight away, rather than waiting for the next capture.
func TestStreamSendsAFreshOpeningFrame(t *testing.T) {
	frames := hub.New()
	s, _, _ := newTestServer(t, Options{Hub: frames})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	frames.Publish(core.Frame{Data: testJPEG(t), RecvedAt: time.Now()})

	resp, err := http.Get(ts.URL + "/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 32)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read the opening part: %v", err)
	}
	if !bytes.Contains(buf, []byte("--"+core.DefaultBoundary)) {
		t.Errorf("opening bytes = %q, want the current frame", buf)
	}
}

// An enumeration that failed is not an empty machine, and only the caller can
// tell the user which it was. The two lists fail independently, so one failing
// must not take the other's results with it: a machine with no ffmpeg still
// has serial ports, and a picker that showed neither would be wrong about both.
func TestDevicesReportsAPartialListWithItsError(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default(), devices: Devices{
		CameraError: "ffmpeg is not executable",
		SerialPorts: []source.SerialPort{{Name: "COM5", Vendor: "Espressif"}},
	}}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/devices")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: half an answer is still an answer", resp.StatusCode)
	}
	var got Devices
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.CameraError, "not executable") {
		t.Errorf("camera_error = %q, want the enumeration error", got.CameraError)
	}
	if len(got.SerialPorts) != 1 || got.SerialPorts[0].Name != "COM5" {
		t.Errorf("serial_ports = %+v, want the list that did enumerate", got.SerialPorts)
	}
	if got.SerialError != "" {
		t.Errorf("serial_error = %q, want it empty: that enumeration worked", got.SerialError)
	}
}

// The settings file refuses unknown keys; the API has to agree. A misspelled
// field is otherwise dropped, the value it meant to set stays at its zero
// value, and the caller gets a 200 for a change that did something else.
func TestManagementAPIRejectsUnknownFields(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default()}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	t.Run("config", func(t *testing.T) {
		body := `{"server":{"hold_on_sorce_loss":true}}`
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for a misspelled field", resp.StatusCode)
		}
		if ctrl.applied {
			t.Error("the controller was handed a config built from a rejected body")
		}
	})

	t.Run("source", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(`{"tpye":"serial"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for a misspelled field", resp.StatusCode)
		}
		if ctrl.switched != "" {
			t.Errorf("the controller switched to %q from a rejected body", ctrl.switched)
		}
	})

	// A body the struct does know is still accepted.
	t.Run("well formed", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(`{"type":"serial"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
}

// Two JSON values in one body are two requests, not one with a typo. Decoding
// the first and stopping there reports success for a change the caller asked
// for and never got.
func TestManagementAPIRejectsTrailingContent(t *testing.T) {
	bodies := map[string]string{
		"a second value":   `{"type":"uvc"}{"type":"mjpeg"}`,
		"trailing junk":    `{"type":"uvc"} oops`,
		"a trailing array": `{"type":"uvc"}[1]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			ctrl := &fakeController{cfg: config.Default()}
			s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if ctrl.switched != "" {
				t.Errorf("the controller switched to %q from a rejected body", ctrl.switched)
			}
		})
	}
}

// Whitespace after the value is just formatting, not a second request.
func TestManagementAPIAcceptsTrailingWhitespace(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default()}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader("{\"type\":\"serial\"}\n\n"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// An absent type decodes to the empty string, and empty is not "missing" any
// further down: Normalise reads it as unset and fills in uvc, so a request
// with no type would move a working source rather than being rejected.
func TestManagementAPIRejectsAnEmptySourceType(t *testing.T) {
	for _, body := range []string{`{}`, `{"type":""}`, `{"type":"   "}`} {
		t.Run(body, func(t *testing.T) {
			ctrl := &fakeController{cfg: config.Default()}
			s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if ctrl.switched != "" {
				t.Errorf("the controller switched to %q with no type given", ctrl.switched)
			}
		})
	}
}

// Subscribing and reading the latest frame are two steps. A frame published in
// between lands in the new client's queue and becomes the latest at the same
// moment, so it would open the stream by sending the same image twice -- two
// samples of one mouth shape for the tracker.
func TestStreamDoesNotResendTheFrameItOpenedWith(t *testing.T) {
	s, h, _ := newTestServer(t, Options{HoldOnSourceLoss: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	first := testJPEG(t)
	h.Publish(core.Frame{Data: first})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	readPart := func() []byte {
		t.Helper()
		var length int
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read part header: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: ")); err == nil && strings.HasPrefix(line, "Content-Length:") {
				length = n
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Fatalf("read part body: %v", err)
		}
		// Each part ends with a CRLF of its own, before the next delimiter.
		trailer := make([]byte, 2)
		if _, err := io.ReadFull(reader, trailer); err != nil {
			t.Fatalf("read part trailer: %v", err)
		}
		return body
	}

	// The frame the stream opens with, which the client already had queued.
	if got := readPart(); !bytes.Equal(got, first) {
		t.Fatal("the stream did not open with the current frame")
	}

	// A distinguishable second frame. If the opening one were resent, this read
	// would return it again instead.
	second := append(bytes.Clone(first), 0x00)
	h.Publish(core.Frame{Data: second})
	if got := readPart(); !bytes.Equal(got, second) {
		t.Error("the frame the stream opened with was sent a second time")
	}
}

// The overlap the skip above is for: a frame published while a client is
// connecting is both the hub's latest and the first thing in that client's
// queue. The race itself cannot be staged from a test -- the publish has to
// land between Subscribe and Latest, two calls inside one handler -- but the
// condition it produces can be shown, and it is the sequence number that tells
// the two apart.
func TestHubQueuesTheFrameThatIsAlsoTheLatest(t *testing.T) {
	h := hub.New()
	frames, cancel := h.Subscribe()
	defer cancel()

	h.Publish(core.Frame{Data: testJPEG(t)})

	latest, ok := h.Latest()
	if !ok {
		t.Fatal("no latest frame after publishing")
	}
	select {
	case queued := <-frames:
		if queued.Seq != latest.Seq {
			t.Fatalf("queued frame %d, latest %d: want the same frame in both", queued.Seq, latest.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the frame never reached the subscriber")
	}
}
