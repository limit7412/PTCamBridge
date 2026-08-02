package bridge

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/status"
)

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mjpegUpstream stands in for a WiFi camera.
func mjpegUpstream(t *testing.T, jpg []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// waitForFrame blocks until the hub has a frame, or fails the test.
func waitForFrame(t *testing.T, h *hub.Hub, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f, ok := h.Latest(); ok {
			return f.Data
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no frame reached the hub within %s", timeout)
	return nil
}

func mjpegConfig(url string) config.Config {
	cfg := config.Default()
	cfg.Source.Type = config.SourceMJPEG
	cfg.Source.MJPEG.URL = url
	cfg.Normalise()
	return cfg
}

// The whole pipeline: upstream camera, driver, transform step, hub.
func TestBridgeDeliversFramesToTheHub(t *testing.T) {
	jpg := testJPEG(t, 32, 32)
	upstream := mjpegUpstream(t, jpg)

	frames := hub.New()
	tracker := status.New()
	b := New(mjpegConfig(upstream.URL), "", frames, tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	got := waitForFrame(t, frames, 5*time.Second)
	if !bytes.Equal(got, jpg) {
		t.Error("the frame on the hub does not match what the camera sent")
	}
	if snapshot := tracker.Snapshot(); !snapshot.Connected || snapshot.Source != "mjpeg" {
		t.Errorf("status = %+v, want a connected mjpeg source", snapshot)
	}
}

// A no-op transform must forward the original bytes, not a re-encode.
func TestBridgeForwardsUntransformedFramesUntouched(t *testing.T) {
	jpg := testJPEG(t, 32, 32)
	upstream := mjpegUpstream(t, jpg)

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	if got := waitForFrame(t, frames, 5*time.Second); !bytes.Equal(got, jpg) {
		t.Errorf("the frame was altered: %d bytes in, %d bytes out", len(jpg), len(got))
	}
}

func TestBridgeAppliesTheConfiguredTransform(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 64, 32))

	cfg := mjpegConfig(upstream.URL)
	cfg.Transform.Rotate = 90

	frames := hub.New()
	b := New(cfg, "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	got := waitForFrame(t, frames, 5*time.Second)
	image, err := jpeg.DecodeConfig(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("decode the published frame: %v", err)
	}
	if image.Width != 32 || image.Height != 64 {
		t.Errorf("published frame is %dx%d, want 32x64 after a quarter turn", image.Width, image.Height)
	}
}

func TestBridgeSwitchChangesTheSource(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	tracker := status.New()
	cfg := mjpegConfig(upstream.URL)
	cfg.Source.UVC.Device = "nonexistent-camera"
	b := New(cfg, "", frames, tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	if err := b.Switch(ctx, config.SourceSerial); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if got := b.Snapshot().Source.Type; got != config.SourceSerial {
		t.Errorf("source type = %q after switching, want serial", got)
	}

	if err := b.Switch(ctx, config.SourceMJPEG); err != nil {
		t.Fatalf("switch back: %v", err)
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q after switching back, want mjpeg", got)
	}
}

func TestBridgeRejectsAnUnknownSourceType(t *testing.T) {
	b := New(config.Default(), "", hub.New(), status.New(), discardLogger())
	if err := b.Switch(context.Background(), "hologram"); err == nil {
		t.Fatal("expected an unknown source type to be rejected")
	}
}

// Pausing has to release the camera: UVC access is exclusive, so a paused
// bridge that kept the handle would still lock Baballonia out.
func TestBridgePauseStopsAndResumeRestarts(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	tracker := status.New()
	b := New(mjpegConfig(upstream.URL), "", frames, tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !b.Paused() || !tracker.Snapshot().Paused {
		t.Error("the bridge does not report itself as paused")
	}

	before := frames.Stats().Published
	time.Sleep(100 * time.Millisecond)
	if after := frames.Stats().Published; after != before {
		t.Errorf("%d frames were published while paused", after-before)
	}

	if err := b.SetPaused(false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames.Stats().Published > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no frames were published after resuming")
}

// Settings that cannot start a source must not leave the bridge with nothing
// running: the previous, working source is put back.
func TestBridgeApplyRevertsWhenTheNewSourceCannotStart(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	broken := b.Snapshot()
	broken.Source.Type = config.SourceUVC
	broken.Source.UVC.Device = "" // no device: the driver refuses to build

	if err := b.Apply(ctx, broken); err == nil {
		t.Fatal("expected Apply to fail for a source that cannot start")
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the working mjpeg source to be restored", got)
	}

	before := frames.Stats().Published
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames.Stats().Published > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the restored source is not producing frames")
}

func TestBridgeApplyPersistsSettings(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Transform.Rotate = 180
	if err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Transform.Rotate != 180 {
		t.Errorf("saved rotate = %d, want 180", reloaded.Transform.Rotate)
	}
}

func TestBridgeApplyRejectsInvalidSettings(t *testing.T) {
	b := New(config.Default(), "", hub.New(), status.New(), discardLogger())

	invalid := config.Default()
	invalid.Server.Listen = "not-an-address"
	if err := b.Apply(context.Background(), invalid); err == nil {
		t.Fatal("expected invalid settings to be rejected")
	}
}

// Stopping must wait for the driver to exit. For UVC that is what guarantees
// the exclusive device handle is released before anything reopens it.
func TestBridgeStopWaitsForTheDriver(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrame(t, frames, 5*time.Second)

	b.Stop()
	published := frames.Stats().Published
	time.Sleep(100 * time.Millisecond)
	if after := frames.Stats().Published; after != published {
		t.Errorf("%d frames arrived after Stop returned, so the driver was still running", after-published)
	}

	b.Stop() // must be safe to call twice
}

func TestBridgeStartTwiceIsAnError(t *testing.T) {
	b := New(config.Default(), "", hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The default source is UVC with no device, so starting fails; that is a
	// source error, not a lifecycle one.
	_ = b.Start(ctx)
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected the second Start to be rejected")
	}
}
