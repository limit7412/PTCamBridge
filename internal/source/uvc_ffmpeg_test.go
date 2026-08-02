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

func TestDeviceUnavailable(t *testing.T) {
	cases := map[string]bool{
		`[dshow @ 000001] Could not find video device with name "Babble"`: true,
		"[video4linux2] Cannot open video device /dev/video9":             true,
		"/dev/video9: No such file or directory":                          true,
		"Error while decoding stream: Invalid data found":                 false,
		"": false,
	}
	for diag, want := range cases {
		if got := deviceUnavailable(diag); got != want {
			t.Errorf("deviceUnavailable(%q) = %v, want %v", diag, got, want)
		}
	}
}

// A device ffmpeg cannot open will not appear on a retry, so the driver has to
// give up rather than loop -- that is what lets Apply roll back to the source
// that was working.
func TestUVCTreatsAnUnopenableDeviceAsFatal(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "no-such-camera",
		FFmpegPath: fakeFFmpeg(t, `[dshow @ 000001] Could not find video device with name "no-such-camera"`),
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
		t.Fatal("Run kept retrying a device ffmpeg cannot open")
	}
}

// The mirror image: a camera that delivered frames and then went away is
// exactly what the reconnect loop is for, so the same diagnostic must not end
// the driver once a frame has arrived.
func TestUVCKeepsRetryingAfterADeviceThatWorked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in driver is a shell script")
	}
	dir := t.TempDir()
	jpegPath := filepath.Join(dir, "frame.jpg")
	if err := os.WriteFile(jpegPath, testJPEG(t), 0o644); err != nil {
		t.Fatalf("write the fixture frame: %v", err)
	}
	// The first run delivers a frame and exits; every run after it reports the
	// device as gone.
	marker := filepath.Join(dir, "ran")
	script := "#!/bin/sh\n" +
		"if [ -f " + marker + " ]; then echo 'Could not find video device' >&2; exit 1; fi\n" +
		"touch " + marker + "\n" +
		"cat " + jpegPath + "\n" +
		"exit 1\n"
	path := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the stand-in ffmpeg: %v", err)
	}

	u, err := NewUVC(UVCConfig{Device: "camera", FFmpegPath: path}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- u.Run(ctx, make(chan core.Frame, 4)) }()

	// Long enough for the first attempt, the 1s backoff and the second attempt.
	select {
	case err := <-done:
		t.Fatalf("Run gave up on a camera that had been working: %v", err)
	case <-time.After(2500 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}
