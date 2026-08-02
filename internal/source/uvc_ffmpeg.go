package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// UVC capture runs through an ffmpeg child process rather than a native
// binding. Windows has no usable cgo-free camera API, and shelling out keeps
// the bridge buildable with CGO_ENABLED=0 as a single executable.

// readChunk is the stdout read size. A 240x240 JPEG is a few kilobytes, so
// this holds several frames per read without wasting memory.
const readChunk = 64 << 10

// stderrTail is how much of ffmpeg's diagnostic output is kept to explain a
// failure.
const stderrTail = 4 << 10

// UVCConfig configures the ffmpeg-backed camera driver.
type UVCConfig struct {
	// Device is the platform capture device: a DirectShow friendly name on
	// Windows, a /dev/video* path on Linux, an AVFoundation index on macOS.
	Device string
	// Size is a WxH string such as "240x240". Empty lets the device choose.
	Size string
	// Framerate is the requested capture rate. Zero lets the device choose.
	Framerate int
	// FFmpegPath overrides the bundled binary.
	FFmpegPath string
	// MaxFrameSize bounds a single JPEG; zero selects the core default.
	MaxFrameSize int
}

// UVC captures from a camera by reading ffmpeg's MJPEG output.
type UVC struct {
	cfg      UVCConfig
	log      *slog.Logger
	reporter Reporter
	// copyCodec is cleared once a camera has proven it cannot deliver MJPEG
	// natively, after which frames are re-encoded instead.
	copyCodec bool
	// delivered records that this driver has produced at least one frame, so
	// a device that worked once is retried rather than given up on.
	delivered bool
}

// NewUVC builds the driver. The device must be set.
func NewUVC(cfg UVCConfig, log *slog.Logger, reporter Reporter) (*UVC, error) {
	if strings.TrimSpace(cfg.Device) == "" {
		return nil, errors.New("uvc: no capture device configured")
	}
	if reporter == nil {
		reporter = NopReporter{}
	}
	return &UVC{cfg: cfg, log: log, reporter: reporter, copyCodec: true}, nil
}

// Name implements Source.
func (u *UVC) Name() string { return "uvc" }

// Run implements Source.
func (u *UVC) Run(ctx context.Context, out chan<- core.Frame) error {
	return runWithBackoff(ctx, u.log, u.Name(), u.reporter, func(ctx context.Context) error {
		frames, diag, err := u.capture(ctx, out, u.copyCodec)
		if frames > 0 {
			u.delivered = true
		}
		switch {
		case err == nil || ctx.Err() != nil:
			return err

		case !u.delivered && deviceUnavailable(diag):
			// ffmpeg named the device itself, so neither waiting nor a
			// different codec is going to help. Reporting this as fatal is
			// what lets Apply put the previous source back instead of
			// retrying a camera that is not there.
			//
			// The check is skipped once a frame has arrived: the same
			// message then means a working camera was unplugged, and that is
			// exactly the case the reconnect loop exists for.
			return fatalf(err)

		case frames == 0 && u.copyCodec:
			// The camera never produced a frame in passthrough mode, so it
			// most likely has no MJPEG output format. Re-encode from here on.
			u.copyCodec = false
			u.log.Info("camera did not deliver MJPEG, switching to re-encoding", "device", u.cfg.Device)
		}
		return err
	})
}

// deviceUnavailableSigns are the ffmpeg diagnostics that mean the configured
// device could not be opened at all, as opposed to a capture that started and
// then broke. Matching text is a heuristic -- ffmpeg has no exit code for
// this -- so an unrecognised message costs only the previous behaviour of
// retrying, and /healthz still reports 503 while no frames arrive.
var deviceUnavailableSigns = []string{
	"could not find video device",       // dshow
	"could not enumerate video devices", // dshow
	"cannot open video device",          // v4l2
	"no such file or directory",         // v4l2
	"could not open video device",
	"video device not found", // avfoundation
}

// deviceUnavailable reports whether an ffmpeg diagnostic blames the device.
func deviceUnavailable(diag string) bool {
	lower := strings.ToLower(diag)
	for _, sign := range deviceUnavailableSigns {
		if strings.Contains(lower, sign) {
			return true
		}
	}
	return false
}

// capture runs one ffmpeg process to completion and returns how many frames it
// yielded along with ffmpeg's diagnostic output, which is the only thing that
// says why a run ended.
func (u *UVC) capture(ctx context.Context, out chan<- core.Frame, copyCodec bool) (uint64, string, error) {
	path, err := u.ffmpegPath()
	if err != nil {
		return 0, "", fatalf(err)
	}
	args := u.args(copyCodec)
	u.log.Debug("starting ffmpeg", "path", path, "args", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, path, args...)
	configureChildProcess(cmd)
	// Without a delay a killed ffmpeg can leave the pipe open and wedge Wait.
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, "", fmt.Errorf("uvc: stdout pipe: %w", err)
	}
	diag := &tailWriter{max: stderrTail}
	cmd.Stderr = diag

	if err := cmd.Start(); err != nil {
		return 0, "", fatalf(fmt.Errorf("uvc: start ffmpeg: %w", err))
	}

	frames, readErr := u.pump(ctx, stdout, out)
	waitErr := cmd.Wait()
	stderr := diag.String()

	// Waiting for the child guarantees the device is released before a source
	// switch opens it again; UVC access is exclusive.
	if ctx.Err() != nil {
		return frames, stderr, nil
	}
	switch {
	case readErr != nil:
		return frames, stderr, fmt.Errorf("uvc: %w (ffmpeg: %s)", readErr, stderr)
	case waitErr != nil:
		return frames, stderr, fmt.Errorf("uvc: ffmpeg exited: %w (%s)", waitErr, stderr)
	default:
		return frames, stderr, fmt.Errorf("uvc: ffmpeg exited without error (%s)", stderr)
	}
}

// pump reads ffmpeg's MJPEG stdout and forwards each complete image.
func (u *UVC) pump(ctx context.Context, stdout io.Reader, out chan<- core.Frame) (uint64, error) {
	assembler := newFrameAssembler(core.SplitJPEGStream, u.cfg.MaxFrameSize)
	buf := make([]byte, readChunk)
	var count uint64

	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			for _, f := range assembler.feed(buf[:n]) {
				if count == 0 {
					u.reporter.Connected(u.Name())
				}
				count++
				if sendErr := send(ctx, out, f); sendErr != nil {
					return count, sendErr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return count, errors.New("ffmpeg closed its output")
			}
			return count, err
		}
	}
}

// args builds the ffmpeg command line for the current platform. Passthrough
// avoids a decode/encode round trip when the camera already emits MJPEG.
func (u *UVC) args(copyCodec bool) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}

	format, input := platformInput(u.cfg.Device)
	args = append(args, "-f", format)
	if u.cfg.Size != "" {
		args = append(args, "-video_size", u.cfg.Size)
	}
	if u.cfg.Framerate > 0 {
		args = append(args, "-framerate", fmt.Sprint(u.cfg.Framerate))
	}
	if copyCodec {
		// Ask the device itself for MJPEG so the frames can be passed through.
		switch format {
		case "v4l2":
			args = append(args, "-input_format", "mjpeg")
		default:
			args = append(args, "-vcodec", "mjpeg")
		}
	}
	args = append(args, "-i", input, "-an")

	if copyCodec {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "mjpeg", "-q:v", "4")
	}
	return append(args, "-f", "mjpeg", "pipe:1")
}

// platformInput maps a configured device onto the ffmpeg input format and
// argument for the running OS.
func platformInput(device string) (format, input string) {
	switch runtime.GOOS {
	case "windows":
		return "dshow", "video=" + device
	case "darwin":
		return "avfoundation", device
	default:
		return "v4l2", device
	}
}

// ffmpegPath resolves the binary: the configured override, then a copy sitting
// next to the executable, then PATH.
func (u *UVC) ffmpegPath() (string, error) {
	if u.cfg.FFmpegPath != "" {
		if _, err := os.Stat(u.cfg.FFmpegPath); err != nil {
			return "", fmt.Errorf("uvc: configured ffmpeg_path %q is not usable: %w", u.cfg.FFmpegPath, err)
		}
		return u.cfg.FFmpegPath, nil
	}
	if exe, err := os.Executable(); err == nil {
		bundled := filepath.Join(filepath.Dir(exe), ffmpegBinaryName())
		if _, statErr := os.Stat(bundled); statErr == nil {
			return bundled, nil
		}
	}
	path, err := exec.LookPath(ffmpegBinaryName())
	if err != nil {
		return "", fmt.Errorf("uvc: ffmpeg not found next to the executable or on PATH: %w", err)
	}
	return path, nil
}

func ffmpegBinaryName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

// Device is a capture device offered to the user in the tray menu and over the
// management API.
type Device struct {
	Name string `json:"name"`
	// Alternative is the DirectShow device path, which is stable across
	// reboots where the friendly name is not.
	Alternative string `json:"alternative,omitempty"`
}

// dshowDeviceLine matches ffmpeg's device listing, for example:
//
//	[dshow @ 0000...] "HD Webcam" (video)
var (
	dshowDeviceLine = regexp.MustCompile(`"([^"]+)"\s*\((video|audio)\)`)
	dshowAltLine    = regexp.MustCompile(`Alternative name\s*"([^"]+)"`)
)

// ListDevices enumerates video capture devices. On Windows this asks ffmpeg
// for the DirectShow list, which it writes to stderr and then exits non-zero;
// elsewhere the /dev/video* nodes are returned.
func ListDevices(ctx context.Context, ffmpegPath string) ([]Device, error) {
	if runtime.GOOS != "windows" {
		return listVideoNodes()
	}
	u := &UVC{cfg: UVCConfig{Device: "dummy", FFmpegPath: ffmpegPath}, log: slog.Default()}
	path, err := u.ffmpegPath()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	configureChildProcess(cmd)
	diag := &tailWriter{max: 64 << 10}
	cmd.Stderr = diag
	runErr := cmd.Run()

	if ctx.Err() != nil {
		return nil, fmt.Errorf("uvc: listing capture devices did not finish: %w", ctx.Err())
	}
	// A non-zero exit is expected here: "dummy" is not a real device, and the
	// listing itself goes to stderr. Anything that is not an exit status means
	// ffmpeg never ran, which is worth reporting rather than showing the user
	// an empty camera list.
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return nil, fmt.Errorf("uvc: run ffmpeg to list devices: %w", runErr)
	}

	return parseDshowDevices(diag.String()), nil
}

// parseDshowDevices extracts the video entries from an ffmpeg device listing.
func parseDshowDevices(out string) []Device {
	var devices []Device
	for _, line := range strings.Split(out, "\n") {
		if alt := dshowAltLine.FindStringSubmatch(line); alt != nil {
			if len(devices) > 0 && devices[len(devices)-1].Alternative == "" {
				devices[len(devices)-1].Alternative = alt[1]
			}
			continue
		}
		m := dshowDeviceLine.FindStringSubmatch(line)
		if m == nil || m[2] != "video" {
			continue
		}
		devices = append(devices, Device{Name: m[1]})
	}
	return devices
}

// listVideoNodes enumerates V4L2 capture nodes, the Linux equivalent of the
// DirectShow listing. It keeps the tray menu populated on non-Windows builds.
func listVideoNodes() ([]Device, error) {
	matches, err := filepath.Glob("/dev/video*")
	if err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(matches))
	for _, m := range matches {
		devices = append(devices, Device{Name: m})
	}
	return devices, nil
}

// tailWriter keeps the last max bytes written to it, so a child process can be
// left running without its diagnostics growing without bound.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(string(w.buf))
}
