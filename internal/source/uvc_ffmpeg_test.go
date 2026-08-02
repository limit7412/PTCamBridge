package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// fakeFFmpeg writes a stand-in for ffmpeg that prints diag to stderr and exits
// non-zero, which is what ffmpeg does when it cannot open a device.
func fakeFFmpeg(t *testing.T, diag string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in driver is a shell script")
	}
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\necho " + strconv.Quote(diag) + " >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the stand-in ffmpeg: %v", err)
	}
	return path
}

// A camera that is not plugged in yet reports the same thing as one that will
// never exist, so the driver keeps retrying either way: a camera attached after
// sign-in still has to be picked up. Deciding whether a source works is the
// bridge's job, and it does it by waiting for a frame.
func TestUVCKeepsRetryingAnUnopenableDevice(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "not-plugged-in-yet",
		FFmpegPath: fakeFFmpeg(t, `[dshow @ 000001] Could not find video device with name "not-plugged-in-yet"`),
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- u.Run(ctx, make(chan core.Frame, 4)) }()

	// Long enough for the first attempt, the 1s backoff and the second.
	select {
	case err := <-done:
		t.Fatalf("Run gave up on a device that may still appear: %v", err)
	case <-time.After(2500 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

// A missing ffmpeg is different: no camera appearing later fixes it, and the
// driver has nothing to retry.
func TestUVCTreatsAMissingFFmpegAsFatal(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "camera",
		FFmpegPath: filepath.Join(t.TempDir(), "no-such-ffmpeg"),
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- u.Run(ctx, make(chan core.Frame, 4)) }()

	select {
	case err := <-done:
		var fatal *FatalError
		if !errors.As(err, &fatal) {
			t.Fatalf("Run returned %v, want a FatalError", err)
		}
	case <-ctx.Done():
		t.Fatal("Run kept retrying a missing ffmpeg binary")
	}
}

// A wedged camera leaves ffmpeg running and silent rather than exiting, and a
// blocking read on its stdout never returns. Without a stall timeout the
// reconnect loop is never reached and the bridge stays dead until restarted.
func TestUVCRecoversFromAnFFmpegThatGoesSilent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in driver is a shell script")
	}
	// Produces nothing and never exits, like a stuck DirectShow filter.
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatalf("write the stand-in ffmpeg: %v", err)
	}

	u, err := NewUVC(UVCConfig{
		Device:       "camera",
		FFmpegPath:   path,
		StallTimeout: 300 * time.Millisecond,
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- u.Run(ctx, make(chan core.Frame, 4)) }()

	// Long enough for the stall timeout to fire and a retry to begin.
	select {
	case err := <-done:
		t.Fatalf("Run returned instead of retrying: %v", err)
	case <-time.After(2 * time.Second):
	}

	// The real proof: cancelling returns promptly. Without the stall timeout
	// the read is still blocked on a child that never writes, and Run would
	// sit there until the process is killed.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop on cancellation; the read is still blocked")
	}
}

// Passthrough is turned off permanently, so it must only happen when the
// device actually opened and refused MJPEG. A camera that was merely not
// plugged in yet fails the same way, and latching on that would cost every
// later frame a decode and re-encode.
func TestUVCKeepsPassthroughWhenTheDeviceNeverOpened(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "not-plugged-in-yet",
		FFmpegPath: fakeFFmpeg(t, `[dshow @ 000001] Could not find video device with name "not-plugged-in-yet"`),
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_ = u.Run(ctx, make(chan core.Frame, 4))

	if !u.copyCodec {
		t.Error("passthrough was disabled by a device that never opened")
	}
}

// The other side of it: a device that opened and could not deliver MJPEG has
// to fall back, or it never works at all.
func TestUVCFallsBackWhenTheDeviceRejectsMJPEG(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "camera",
		FFmpegPath: fakeFFmpeg(t, "Selected video codec mjpeg is not supported by the device"),
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = u.Run(ctx, make(chan core.Frame, 4))

	if u.copyCodec {
		t.Error("passthrough was kept after the device rejected MJPEG")
	}
}
