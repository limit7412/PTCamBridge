package bridge

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
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

// Switching to a source that is not attached now fails and leaves the working
// one running, instead of reporting success with nothing producing frames.
func TestBridgeSwitchRejectsAnUnavailableSource(t *testing.T) {
	shortenVerify(t, 1500*time.Millisecond)
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

	if err := b.Switch(ctx, config.SourceSerial); err == nil {
		t.Fatal("expected switching to a serial port that is not there to fail")
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the working mjpeg source kept", got)
	}

	before := frames.Stats().Published
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames.Stats().Published > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the working source stopped producing frames after a rejected switch")
}

// Switching to a source that does work is accepted, and the frames keep coming.
func TestBridgeSwitchToAWorkingSource(t *testing.T) {
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

	if err := b.Switch(ctx, config.SourceMJPEG); err != nil {
		t.Fatalf("Switch to a working source: %v", err)
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want mjpeg", got)
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

// Building a driver only proves the settings parse. A driver that fails once
// it is actually running -- no ffmpeg binary on disk, say -- must cost the
// settings change rather than the source that was working a moment ago.
func TestBridgeApplyRevertsWhenTheNewSourceFailsAsynchronously(t *testing.T) {
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

	// The device name is set, so the driver builds. It is ffmpeg that is
	// missing, and UVC only finds that out inside Run.
	broken := b.Snapshot()
	broken.Source.Type = config.SourceUVC
	broken.Source.UVC.Device = "camera"
	broken.Source.UVC.FFmpegPath = filepath.Join(t.TempDir(), "no-such-ffmpeg")

	if err := b.Apply(ctx, broken); err == nil {
		t.Fatal("expected Apply to fail for a driver that cannot run")
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

// Losing the write is not the same as losing the change, but the caller still
// has to hear about it: what it just set will not survive a restart.
func TestBridgeApplyReportsASaveFailure(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	// A directory sitting where the settings file belongs fails the rename
	// without making anything else about the run unusual.
	path := filepath.Join(t.TempDir(), config.FileName)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Transform.Rotate = 180

	err := b.Apply(ctx, updated)
	if !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("Apply error = %v, want one wrapping config.ErrNotSaved", err)
	}
	// Only the write failed, so the change itself is still in effect.
	if got := b.Snapshot().Transform.Rotate; got != 180 {
		t.Errorf("rotate = %d, want the change to still be active", got)
	}
}

// Settings the running process cannot adopt are refused, rather than accepted
// and written to a file that then disagrees with what is running.
func TestBridgeApplyRejectsSettingsThatNeedARestart(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	original := b.Snapshot().Server.Listen
	moved := b.Snapshot()
	moved.Server.Listen = "127.0.0.1:19999"

	if err := b.Apply(ctx, moved); err == nil {
		t.Fatal("expected a listen address change to be rejected, not silently ignored")
	}
	if got := b.Snapshot().Server.Listen; got != original {
		t.Errorf("listen = %q, want it left at %q", got, original)
	}
}

// streamConfiguratorFunc adapts a function to StreamConfigurator.
type streamConfiguratorFunc func(core.MultipartEncoder, bool)

func (f streamConfiguratorFunc) SetStreamOptions(enc core.MultipartEncoder, hold bool) {
	f(enc, hold)
}

// The boundary and the extra headers exist to match whatever the PaperTracker
// client parses today, so a change to them has to reach the server itself.
func TestBridgeApplyReconfiguresTheStream(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())

	var gotBoundary string
	var gotHold bool
	b.SetStreamConfigurator(streamConfiguratorFunc(func(enc core.MultipartEncoder, hold bool) {
		gotBoundary, gotHold = enc.Boundary(), hold
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Server.Boundary = "othermark"
	updated.Server.HoldOnSourceLoss = true
	if err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if gotBoundary != "othermark" || !gotHold {
		t.Errorf("server was given boundary %q hold %v, want othermark true", gotBoundary, gotHold)
	}
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

// shortenVerify keeps the start verification from dominating test runtime.
func shortenVerify(t *testing.T, d time.Duration) {
	t.Helper()
	previous := startVerifyTimeout
	startVerifyTimeout = d
	t.Cleanup(func() { startVerifyTimeout = previous })
}

// Only a frame proves a source works. A driver that starts, fails to reach its
// camera and settles into reconnecting has not started anything the user can
// use, so Apply must not keep those settings.
func TestBridgeApplyRevertsWhenTheNewSourceNeverDelivers(t *testing.T) {
	shortenVerify(t, 1500*time.Millisecond)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	// An upstream that accepts the connection and then says nothing: no error
	// to report, and no frame either.
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer silent.Close()

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
	broken.Source.MJPEG.URL = silent.URL

	if err := b.Apply(ctx, broken); err == nil {
		t.Fatal("expected Apply to fail for a source that never produced a frame")
	}
	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the working upstream restored", got)
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

// The request that asked for the change is what the verification is being run
// for. Once it has gone -- a client that disconnected, or an HTTP timeout --
// finishing the change anyway leaves the bridge and the settings file on a
// source the caller was told nothing about, and was told had failed.
func TestBridgeApplyStopsWhenTheRequestIsCancelled(t *testing.T) {
	// Long enough that the test would hang on it rather than pass by accident.
	shortenVerify(t, time.Minute)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	// Accepts the connection, then says nothing: verification would wait.
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer silent.Close()

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	req, cancelReq := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancelReq)
	defer cancelReq()

	broken := b.Snapshot()
	broken.Source.MJPEG.URL = silent.URL

	done := make(chan error, 1)
	go func() { done <- b.Apply(req, broken) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Apply to fail once the request was cancelled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Apply error = %v, want it to carry context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Apply kept verifying after the request that asked for it had gone")
	}

	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the working upstream restored", got)
	}
}

// A change that only touches the server settings never reaches the
// verification, so the request context has to be checked on the way in too.
// Waiting for the lock can take as long as another caller's whole
// verification, which is exactly when a PUT gives up.
func TestBridgeApplyRejectsAnAlreadyCancelledRequest(t *testing.T) {
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

	// Server-only, so captureUnchanged holds and no source is restarted.
	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = true

	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()

	if err := b.Apply(dead, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context.Canceled", err)
	}
	if b.Snapshot().Server.HoldOnSourceLoss {
		t.Error("the change was applied for a request that had already gone")
	}
}

// The parsers upstream check structure because that is all they can afford per
// frame. Structure is not an image, so a source that only ever emits SOI/EOI
// would otherwise be saved as working while the tracker gets nothing.
func TestBridgeApplyRejectsASourceSendingUndecodableFrames(t *testing.T) {
	shortenVerify(t, 3*time.Second)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	// Structurally a JPEG, and no image in it.
	hollow := mjpegUpstream(t, []byte{0xFF, 0xD8, 0xFF, 0xD9})

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
	broken.Source.MJPEG.URL = hollow.URL

	err := b.Apply(ctx, broken)
	if err == nil {
		t.Fatal("expected a source with no decodable image to be refused")
	}
	// Not the timeout: the frame did arrive, it was the decode that rejected
	// it. Accepting a timeout here would let the test pass on a source that
	// simply never delivered.
	if !strings.Contains(err.Error(), "usable JPEG") {
		t.Errorf("error = %v, want the frame rejected as undecodable rather than missing", err)
	}
	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the working upstream restored", got)
	}
}

// Startup is the opposite case: a camera that is not there yet must be left to
// reconnect, because nothing will start it a second time.
func TestBridgeStartLeavesAnUnreachableSourceRetrying(t *testing.T) {
	shortenVerify(t, 500*time.Millisecond)

	// Nothing is listening here, so every attempt fails and retries.
	cfg := mjpegConfig("http://127.0.0.1:1/")
	frames := hub.New()
	b := New(cfg, "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start reported a failure for a source that should keep retrying: %v", err)
	}
	defer b.Stop()

	// The driver has to still be alive: stopping it is what would stop a
	// camera plugged in later from ever being picked up.
	time.Sleep(750 * time.Millisecond)
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the configured mjpeg source", got)
	}
	if b.stopped == nil {
		t.Error("no driver is running after Start; a source appearing later would never be picked up")
	}
}

// While paused nothing runs, but Apply still has to reject settings that could
// not start: resuming later would otherwise fail with the previous, working
// configuration already gone.
func TestBridgeApplyValidatesTheDriverWhilePaused(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	unbuildable := b.Snapshot()
	unbuildable.Source.Type = config.SourceUVC
	unbuildable.Source.UVC.Device = "" // no device: the driver refuses to build

	if err := b.Apply(ctx, unbuildable); err == nil {
		t.Fatal("expected settings that cannot build a driver to be rejected while paused")
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the previous settings kept", got)
	}

	// Resuming must therefore still work.
	if err := b.SetPaused(false); err != nil {
		t.Errorf("resume after a rejected change: %v", err)
	}
}

// Changing only the settings the HTTP server owns must not interrupt the
// camera. Restarting it would drop the stream for nothing, and the start
// verification would reject the change outright while the camera happened to
// be reconnecting -- which is exactly when hold_on_source_loss gets touched.
func TestBridgeApplyDoesNotRestartCaptureForServerOnlySettings(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	tracker := status.New()
	b := New(mjpegConfig(upstream.URL), "", frames, tracker, discardLogger())

	var gotHold bool
	b.SetStreamConfigurator(streamConfiguratorFunc(func(_ core.MultipartEncoder, hold bool) {
		gotHold = hold
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	// The driver instance in place before the change has to be the same one
	// afterwards; a restart would replace it.
	before := b.stopped

	updated := b.Snapshot()
	updated.Server.HoldOnSourceLoss = true
	if err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply of a server-only change: %v", err)
	}

	if b.stopped != before {
		t.Error("the capture source was restarted for a change it does not depend on")
	}
	if !gotHold {
		t.Error("the server was not told about the new hold_on_source_loss")
	}
	if !b.Snapshot().Server.HoldOnSourceLoss {
		t.Error("the change was not kept")
	}
}

// The driver announces a frame as soon as it has parsed one, but the transform
// sits between there and the hub. A frame it drops means no client sees
// anything, so it must not count as a working source.
func TestBridgeApplyRevertsWhenTheTransformDropsEveryFrame(t *testing.T) {
	shortenVerify(t, 1500*time.Millisecond)
	good := mjpegUpstream(t, testJPEG(t, 16, 16))

	// Structurally a valid JPEG -- the driver parses and forwards it -- but
	// its header claims 65535x65535, so the transform refuses to decode it.
	oversized := bytes.Clone(testJPEG(t, 16, 16))
	sof := bytes.Index(oversized, []byte{0xFF, 0xC0})
	if sof < 0 {
		t.Fatal("fixture has no baseline SOF0 to rewrite")
	}
	copy(oversized[sof+5:sof+9], []byte{0xFF, 0xFF, 0xFF, 0xFF})
	huge := mjpegUpstream(t, oversized)

	frames := hub.New()
	cfg := mjpegConfig(good.URL)
	cfg.Transform.Rotate = 90
	b := New(cfg, "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	broken := b.Snapshot()
	broken.Source.MJPEG.URL = huge.URL

	if err := b.Apply(ctx, broken); err == nil {
		t.Fatal("expected Apply to fail when no frame survives the transform")
	}
	if got := b.Snapshot().Source.MJPEG.URL; got != good.URL {
		t.Errorf("URL = %q, want the working upstream restored", got)
	}
}

// A shutdown that lands mid-verification is not proof of anything, and must
// not persist a configuration nothing ever confirmed.
func TestBridgeApplyFailsWhenShutdownInterruptsVerification(t *testing.T) {
	shortenVerify(t, 10*time.Second)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer silent.Close()

	path := filepath.Join(t.TempDir(), config.FileName)
	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), path, frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	// Shut down while Apply is still waiting for the silent source.
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	unverified := b.Snapshot()
	unverified.Source.MJPEG.URL = silent.URL
	if err := b.Apply(ctx, unverified); err == nil {
		t.Fatal("expected a shutdown during verification to fail the apply")
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		saved, _ := config.Load(path)
		t.Errorf("an unverified configuration was persisted: %q", saved.Source.MJPEG.URL)
	}
}

// An override meant for one run must not become permanent the first time
// something unrelated is changed.
func TestBridgeApplySavesOnlyWhatChanged(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	// What the file says.
	fileCfg := mjpegConfig(upstream.URL)
	fileCfg.Source.UVC.Device = "the camera the user configured"

	// What this run is actually using, after a -device override.
	effective := fileCfg
	effective.Source.UVC.Device = "just for this run"

	if err := config.Save(path, fileCfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(effective, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(fileCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// Change something else entirely.
	updated := b.Snapshot()
	updated.Server.HoldOnSourceLoss = true
	if err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the change the caller made was not saved")
	}
	if got := saved.Source.UVC.Device; got != "the camera the user configured" {
		t.Errorf("saved device = %q, want the run override left out of the file", got)
	}
}

// The override does have to be saved when it is what the caller changed.
func TestBridgeApplySavesADeliberateChangeToAnOverriddenField(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	fileCfg := mjpegConfig(upstream.URL)
	fileCfg.Source.UVC.Device = "from the file"
	effective := fileCfg
	effective.Source.UVC.Device = "from the command line"

	b := New(effective, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(fileCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Source.UVC.Device = "picked in the tray"
	if err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := saved.Source.UVC.Device; got != "picked in the tray" {
		t.Errorf("saved device = %q, want the deliberate change", got)
	}
}

// Settings for a source that is not running describe nothing the camera is
// doing, so filling them in ahead of a switch must not interrupt it.
func TestCaptureUnchangedIgnoresTheInactiveSources(t *testing.T) {
	base := mjpegConfig("http://camera.invalid/")

	preparing := base
	preparing.Source.UVC.Device = "a camera to switch to later"
	preparing.Source.Serial.Port = "COM7"
	if !captureUnchanged(base, preparing) {
		t.Error("filling in an inactive source was treated as a capture change")
	}

	switched := base
	switched.Source.MJPEG.URL = "http://other.invalid/"
	if captureUnchanged(base, switched) {
		t.Error("changing the active source's URL was not treated as a capture change")
	}

	resized := base
	resized.Source.MaxFrameSize = 1 << 20
	if captureUnchanged(base, resized) {
		t.Error("changing max_frame_size was not treated as a capture change")
	}

	rotated := base
	rotated.Transform.Rotate = 90
	if captureUnchanged(base, rotated) {
		t.Error("changing the transform was not treated as a capture change")
	}
}

// A save that failed is reported as "this will be lost on restart". Advancing
// the base anyway would fold the change into the next successful save and
// write out the very thing that error promised was temporary.
// startWithAnUnwritableConfig runs a bridge whose settings file cannot be
// written, and returns it along with the path once the obstruction has been
// cleared, so the caller can watch what the next save does.
func startWithAnUnwritableConfig(t *testing.T, ctx context.Context) (*Bridge, string) {
	t.Helper()

	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	// A directory where the file belongs fails the rename.
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(b.Stop)

	lost := b.Snapshot()
	lost.Server.Boundary = "unwritable"
	if err := b.Apply(ctx, lost); !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("Apply error = %v, want config.ErrNotSaved", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the obstruction: %v", err)
	}
	return b, path
}

// Retrying is the obvious thing to do with a change that was applied but not
// saved, and it has to actually write it. The retry sends settings the running
// bridge already has, so there is nothing to diff against it -- the pending
// write is the only record that the file is behind.
func TestBridgeApplyRetryPersistsAChangeThatFailedToSave(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, path := startWithAnUnwritableConfig(t, ctx)

	if err := b.Apply(ctx, b.Snapshot()); err != nil {
		t.Fatalf("retrying the same settings: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Server.Boundary != "unwritable" {
		t.Errorf("saved boundary = %q, want the change the retry was asking to persist", saved.Server.Boundary)
	}
}

// The same pending write also rides along with the next unrelated change. The
// running bridge has been using the value all along, so leaving it out would
// keep the file describing a configuration that is not the one in effect.
func TestBridgeApplyCarriesAnUnsavedChangeIntoTheNextSave(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, path := startWithAnUnwritableConfig(t, ctx)

	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = true
	if err := b.Apply(ctx, next); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the change that did save is missing from the file")
	}
	if saved.Server.Boundary != "unwritable" {
		t.Errorf("saved boundary = %q, want the earlier change that is still in effect", saved.Server.Boundary)
	}
}

// The settings file is not written only from here: the tray offers "Edit
// settings", and the values that need a restart can only be changed that way.
// A save built on the file as it was at startup would write those edits back
// over the moment anything else was changed.
func TestBridgeApplyKeepsAnEditMadeToTheFileWhileRunning(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	fileCfg := mjpegConfig(upstream.URL)
	if err := config.Save(path, fileCfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(fileCfg, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(fileCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// The user edits the file by hand. server.listen is one of the settings
	// that can only be changed this way, which is what makes losing it likely.
	edited := fileCfg
	edited.Server.Listen = "127.0.0.1:19999"
	edited.Source.UVC.Device = "picked while running"
	if err := config.Save(path, edited); err != nil {
		t.Fatalf("Save the edit: %v", err)
	}

	// Then changes something unrelated through the tray before restarting.
	updated := b.Snapshot()
	updated.Server.HoldOnSourceLoss = true
	if err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the change the caller made was not saved")
	}
	if saved.Server.Listen != "127.0.0.1:19999" {
		t.Errorf("saved listen = %q, want the hand edit kept", saved.Server.Listen)
	}
	if saved.Source.UVC.Device != "picked while running" {
		t.Errorf("saved device = %q, want the hand edit kept", saved.Source.UVC.Device)
	}
}

// Nothing can be started while paused, so nothing can be proven. Taking the
// change anyway would swap a working configuration for an unproven one and
// write it out, and resume does not verify either.
func TestBridgeApplyRejectsASourceChangeWhilePaused(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	other := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	cfg := mjpegConfig(upstream.URL)
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	frames := hub.New()
	b := New(cfg, path, frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}

	next := b.Snapshot()
	next.Source.MJPEG.URL = other.URL
	if err := b.Apply(ctx, next); err == nil {
		t.Fatal("expected a source change to be refused while paused")
	}
	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the settings left alone", got)
	}

	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.Source.MJPEG.URL != upstream.URL {
		t.Errorf("saved URL = %q, want the unverified change kept out of the file", saved.Source.MJPEG.URL)
	}
}

// Pausing is for releasing the camera, so the settings that do not touch it
// still have to be changeable while paused -- including the one about what to
// do when there is no source.
func TestBridgeApplyAllowsAServerChangeWhilePaused(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}

	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = true
	if err := b.Apply(ctx, next); err != nil {
		t.Fatalf("Apply a server-only change while paused: %v", err)
	}
	if !b.Snapshot().Server.HoldOnSourceLoss {
		t.Error("the server-only change was not applied")
	}
}

// A settings file that will not parse is most likely one the user is part way
// through editing. Writing over it to persist a change that is already in
// effect trades their edit for something that could just as well be written a
// moment later.
func TestBridgeApplyWillNotOverwriteAnUnparsableFile(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	cfg := mjpegConfig(upstream.URL)
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(cfg, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// Mid-edit: a half-typed table header.
	halfEdited := "[server\nlisten = '127.0.0.1:18080'\n"
	if err := os.WriteFile(path, []byte(halfEdited), 0o644); err != nil {
		t.Fatalf("write the half-edited file: %v", err)
	}

	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = true
	err := b.Apply(ctx, next)
	if !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("Apply error = %v, want config.ErrNotSaved", err)
	}

	// The change is in effect even though it could not be written.
	if !b.Snapshot().Server.HoldOnSourceLoss {
		t.Error("the change was not applied to the running settings")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(onDisk) != halfEdited {
		t.Errorf("the file was rewritten:\n%s", onDisk)
	}

	// Once the file parses again, the held change is written.
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save the finished edit: %v", err)
	}
	if err := b.Apply(ctx, b.Snapshot()); err != nil {
		t.Fatalf("retry after the edit was finished: %v", err)
	}
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the held change was not written once the file could be read")
	}
}
