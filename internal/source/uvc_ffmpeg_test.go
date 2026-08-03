package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

// Falling back is a guess being tested, not a verdict. Re-encoding asks the
// same device for a different output format: if that works the format really
// was the problem, and if it produces nothing either then the device was, so
// passthrough comes back. Otherwise a camera that was merely busy at sign-in
// costs every later frame a decode and re-encode for the life of the process,
// on the strength of a stderr string this cannot be expected to recognise.
func TestUVCCodecChoice(t *testing.T) {
	const busy = "[dshow @ 000001] I/O error"
	const missing = `[dshow @ 000001] Could not find video device with name "camera"`
	failed := errors.New("ffmpeg exited")

	t.Run("a device that never opened keeps passthrough", func(t *testing.T) {
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}
		u.chooseCodec(0, missing, failed)
		if !u.copyCodec {
			t.Error("passthrough was disabled by a device that never opened")
		}
	})

	t.Run("an unrecognised failure is probed and then undone", func(t *testing.T) {
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}

		u.chooseCodec(0, busy, failed)
		if u.copyCodec {
			t.Fatal("re-encoding was never tried")
		}
		u.chooseCodec(0, busy, failed)
		if !u.copyCodec {
			t.Error("re-encoding produced nothing either, so passthrough should be back")
		}
	})

	t.Run("re-encoding that works is confirmed before it settles", func(t *testing.T) {
		const noMJPEG = "Selected video codec mjpeg is not supported by the device"
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}

		u.chooseCodec(0, noMJPEG, failed)
		if u.copyCodec {
			t.Fatal("re-encoding was never tried")
		}
		// Frames under re-encoding say the camera works, not that it has no
		// MJPEG: the passthrough attempt before this one may have caught it
		// busy. Passthrough gets one more try against a device known to work.
		u.chooseCodec(12, "", nil)
		if !u.copyCodec {
			t.Fatal("passthrough was written off on a single comparison")
		}
		if u.reencodeReal {
			t.Fatal("one working re-encode settled it")
		}

		// It fails again, and now the two results are about the same device in
		// the same state.
		u.chooseCodec(0, noMJPEG, failed)
		if u.copyCodec || !u.reencodeReal {
			t.Fatal("a second passthrough failure did not settle it")
		}
		// The camera being unplugged later must not undo that.
		u.chooseCodec(0, busy, failed)
		if u.copyCodec {
			t.Error("passthrough came back after re-encoding had been proven necessary")
		}
		u.chooseCodec(12, "", nil)
		if u.copyCodec {
			t.Error("a working re-encode reopened the question")
		}
	})

	// The case the confirmation exists for: the camera was busy when
	// passthrough ran and free when re-encoding did, so the comparison proves
	// nothing. Passthrough works on the retry and stays.
	t.Run("a device that was merely busy keeps passthrough", func(t *testing.T) {
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}

		u.chooseCodec(0, busy, failed)
		u.chooseCodec(12, "", nil) // re-encoding, on a camera that has recovered
		if !u.copyCodec {
			t.Fatal("passthrough was not tried again")
		}
		u.chooseCodec(12, "", nil) // and passthrough works
		if !u.copyCodec || u.reencodeReal {
			t.Error("a working passthrough was given up")
		}
		// A later dropout must not resurrect the earlier guess.
		u.chooseCodec(0, busy, failed)
		u.chooseCodec(12, "", nil)
		if !u.copyCodec {
			t.Error("passthrough was written off on the strength of an old failure")
		}
	})

	t.Run("passthrough that works is left alone", func(t *testing.T) {
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}
		u.chooseCodec(12, "", nil)
		if !u.copyCodec || u.reencodeReal {
			t.Error("a working passthrough was changed")
		}
	})
}

// The other side of it: a device that opened and could not deliver MJPEG has
// to try re-encoding, or it never works at all.
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

// A copy the bridge fetched for itself is the last place looked at, after the
// three the user controls. Anything else and an installation deliberately
// pointed at a particular ffmpeg would quietly stop using it.
func TestUVCPrefersTheUsersFFmpegOverTheFetchedOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup needs an executable bit")
	}

	settings := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", settings)
	t.Setenv("APPDATA", settings)
	fetched := writeExecutable(t, filepath.Join(settings, "PTCamBridge", "bin"), ffmpegBinaryName())

	// Nothing else on offer: the fetched copy is found.
	t.Setenv("PATH", t.TempDir())
	u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger()}
	if got, err := u.ffmpegPath(); err != nil || got != fetched {
		t.Fatalf("ffmpegPath() = %q, %v; want the fetched copy %q", got, err, fetched)
	}

	// One on PATH outranks it.
	onPath := t.TempDir()
	writeExecutable(t, onPath, ffmpegBinaryName())
	t.Setenv("PATH", onPath)
	got, err := u.ffmpegPath()
	if err != nil {
		t.Fatalf("ffmpegPath: %v", err)
	}
	if got == fetched {
		t.Errorf("ffmpegPath() = the fetched copy, want the one on PATH")
	}

	// And the configured override outranks everything.
	configured := writeExecutable(t, t.TempDir(), "my-ffmpeg")
	u.cfg.FFmpegPath = configured
	if got, err := u.ffmpegPath(); err != nil || got != configured {
		t.Errorf("ffmpegPath() = %q, %v; want the configured %q", got, err, configured)
	}
}

// With nothing anywhere, the failure has to say what the user can do about it.
func TestUVCSaysHowToGetFFmpegWhenThereIsNone(t *testing.T) {
	settings := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", settings)
	t.Setenv("APPDATA", settings)
	t.Setenv("PATH", t.TempDir())

	u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger()}
	_, err := u.ffmpegPath()
	if !errors.Is(err, ErrNoFFmpeg) {
		t.Fatalf("ffmpegPath() error = %v, want ErrNoFFmpeg", err)
	}
	for _, want := range []string{"tray", "ffmpeg_path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Missing ffmpeg has to stay retryable. The tray can fetch one while the bridge
// is running, and if the driver gave up for good the fetch would finish with
// the camera still dead until the user restarted -- which is the whole flow the
// download exists to serve.
func TestUVCKeepsRetryingWhenThereIsNoFFmpegYet(t *testing.T) {
	settings := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", settings)
	t.Setenv("APPDATA", settings)
	t.Setenv("PATH", t.TempDir())

	u, err := NewUVC(UVCConfig{Device: "camera"}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	runErr := u.Run(ctx, make(chan core.Frame, 4))

	var fatal *FatalError
	if errors.As(runErr, &fatal) {
		t.Fatalf("Run gave up with %v, want it to keep retrying until ffmpeg appears", runErr)
	}
	if runErr != nil {
		t.Errorf("Run = %v, want nil once the context expires", runErr)
	}
}

func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
