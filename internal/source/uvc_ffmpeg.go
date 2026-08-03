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
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
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

// defaultUVCStallTimeout is how long ffmpeg may go without producing a frame
// before the child is killed and the attempt retried.
//
// A wedged USB camera or DirectShow filter leaves ffmpeg running and silent
// rather than exiting, and a blocking read on its stdout never returns. The
// reconnect loop is downstream of that read, so without this the bridge stays
// dead until the user restarts it. The window has to cover ffmpeg's own
// startup, which on Windows takes a second or two before the first frame.
const defaultUVCStallTimeout = 10 * time.Second

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
	// StallTimeout is how long ffmpeg may go without producing a frame before
	// it is killed and the attempt retried; zero selects
	// defaultUVCStallTimeout.
	StallTimeout time.Duration
}

// UVC captures from a camera by reading ffmpeg's MJPEG output.
type UVC struct {
	cfg      UVCConfig
	log      *slog.Logger
	reporter Reporter
	// copyCodec asks the camera for MJPEG and passes it through. It is cleared
	// when passthrough produces nothing, and set again if re-encoding produces
	// nothing either -- see chooseCodec.
	copyCodec bool
	// reencodeWorked records that re-encoding has produced frames from this
	// camera at least once. On its own that is not proof the camera has no
	// MJPEG: the passthrough attempt before it may simply have caught the
	// device busy, with the camera free again by the time re-encoding ran.
	reencodeWorked bool
	// reencodeReal records that passthrough failed a second time, on a device
	// known by then to work. That is the comparison the two modes exist to
	// make, and it is what settles the question for good.
	reencodeReal bool
}

// NewUVC builds the driver. The device must be set.
func NewUVC(cfg UVCConfig, log *slog.Logger, reporter Reporter) (*UVC, error) {
	if strings.TrimSpace(cfg.Device) == "" {
		return nil, errors.New("uvc: no capture device configured")
	}
	if reporter == nil {
		reporter = NopReporter{}
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = defaultUVCStallTimeout
	}
	return &UVC{cfg: cfg, log: log, reporter: reporter, copyCodec: true}, nil
}

// Name implements Source.
func (u *UVC) Name() string { return "uvc" }

// Run implements Source.
func (u *UVC) Run(ctx context.Context, out chan<- core.Frame) error {
	return runWithBackoff(ctx, u.log, u.Name(), u.reporter, func(ctx context.Context) error {
		// A camera that is simply not plugged in yet reports the same thing as
		// one that will never exist, so nothing here is treated as fatal: the
		// driver keeps retrying and a camera attached later is picked up. It
		// is the bridge that decides whether a source is working, by waiting
		// for the first frame when the caller needs an answer.
		frames, diag, err := u.capture(ctx, out, u.copyCodec)
		u.chooseCodec(frames, diag, err)
		return err
	})
}

// chooseCodec picks the mode the next attempt runs in.
//
// Passthrough failing is not evidence that the camera has no MJPEG output. A
// device that is busy, or still settling after sign-in, fails the same way and
// says so in words this cannot be expected to recognise -- the diagnostics
// differ by ffmpeg version, backend and driver. Matching them positively would
// only move the guess somewhere harder to see.
//
// What does distinguish the two is what happens next. Re-encoding asks for a
// different output format from the same device: if it produces nothing either,
// the device was the problem all along and passthrough is tried again, so a
// camera that comes back later is not decoded and re-encoded for the rest of
// the process.
//
// Re-encoding that works is not the end of it either. The camera may have been
// busy for the passthrough attempt and free by the time this one ran, and the
// two results are minutes apart on a device whose state changed in between. So
// the next attempt asks passthrough once more, on a device now known to work,
// and only a second failure settles it. That costs one attempt after the first
// disconnect on a camera that really has no MJPEG, and it is what keeps a
// momentary conflict from turning every later frame into a decode and re-encode.
func (u *UVC) chooseCodec(frames uint64, diag string, err error) {
	if frames > 0 {
		switch {
		case u.copyCodec:
			// Passthrough works, so whatever went wrong before was the device.
			u.reencodeWorked = false
		case !u.reencodeWorked:
			// Worth knowing, but not yet worth believing: try passthrough once
			// more when this attempt ends, now that the camera has proven it
			// can deliver frames at all.
			u.reencodeWorked = true
			u.copyCodec = true
			u.log.Debug("re-encoding worked; passthrough gets one more try on the next attempt", "device", u.cfg.Device)
		}
		return
	}
	if err == nil {
		return
	}

	if u.copyCodec {
		if deviceUnavailable(diag) {
			// The device never opened, so nothing was learned about its
			// formats. Trying the re-encode is pointless as well as misleading.
			return
		}
		if u.reencodeWorked {
			// Twice now, either side of a re-encode that delivered frames from
			// this same camera. That is as close to a positive answer as this
			// can get.
			u.copyCodec = false
			u.reencodeReal = true
			u.log.Info("camera has no MJPEG output of its own, re-encoding from here on", "device", u.cfg.Device)
			return
		}
		u.copyCodec = false
		u.log.Debug("passthrough produced no frames, trying re-encoding", "device", u.cfg.Device)
		return
	}
	if u.reencodeReal {
		return
	}
	u.copyCodec = true
	u.log.Debug("re-encoding produced no frames either, going back to passthrough", "device", u.cfg.Device)
}

// capture runs one ffmpeg process to completion and returns how many frames it
// yielded along with ffmpeg's diagnostic output.
func (u *UVC) capture(ctx context.Context, out chan<- core.Frame, copyCodec bool) (uint64, string, error) {
	path, err := u.ffmpegPath()
	if err != nil {
		// Only a configured path that does not work is fatal: the settings name
		// a specific file and it is not there, which no amount of retrying
		// fixes. Finding none at all is not the same thing -- the tray can
		// fetch one while the bridge is running, and the retry loop is what
		// picks it up. Giving up here would mean the fetch finishes and the
		// camera stays dead until the user restarts.
		if errors.Is(err, ErrNoFFmpeg) {
			return 0, "", err
		}
		return 0, "", fatalf(err)
	}
	args := u.args(copyCodec)
	u.log.Debug("starting ffmpeg", "path", path, "args", strings.Join(args, " "))

	// Killing the child is what unblocks the read: a silent ffmpeg that is
	// still running holds the pipe open, so there is nothing to time out on
	// the reading side.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stall := time.AfterFunc(u.cfg.StallTimeout, cancelRun)
	defer stall.Stop()

	cmd := exec.CommandContext(runCtx, path, args...)
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

	frames, readErr := u.pump(ctx, stdout, out, func() {
		stall.Reset(u.cfg.StallTimeout)
	})
	waitErr := cmd.Wait()
	stderr := diag.String()

	// Waiting for the child guarantees the device is released before a source
	// switch opens it again; UVC access is exclusive.
	if ctx.Err() != nil {
		return frames, stderr, nil
	}
	switch {
	case runCtx.Err() != nil:
		return frames, stderr, fmt.Errorf("uvc: %s produced no frame for %s (ffmpeg: %s)", u.cfg.Device, u.cfg.StallTimeout, stderr)
	case readErr != nil:
		return frames, stderr, fmt.Errorf("uvc: %w (ffmpeg: %s)", readErr, stderr)
	case waitErr != nil:
		return frames, stderr, fmt.Errorf("uvc: ffmpeg exited: %w (%s)", waitErr, stderr)
	default:
		return frames, stderr, fmt.Errorf("uvc: ffmpeg exited without error (%s)", stderr)
	}
}

// deviceUnavailableSigns are the ffmpeg diagnostics that mean the device could
// not be opened at all, as opposed to one that opened and could not deliver
// MJPEG. Matching text is a heuristic, so it only guards the codec fallback
// and never a decision to stop retrying: a missed match costs a needless
// re-encode, which is what happened unconditionally before.
var deviceUnavailableSigns = []string{
	"could not find video device",       // dshow
	"could not enumerate video devices", // dshow
	"cannot open video device",          // v4l2
	"could not open video device",
	"no such file or directory", // v4l2
	"video device not found",    // avfoundation
}

// deviceUnavailable reports whether an ffmpeg diagnostic blames the device
// rather than the format asked of it.
func deviceUnavailable(diag string) bool {
	lower := strings.ToLower(diag)
	for _, sign := range deviceUnavailableSigns {
		if strings.Contains(lower, sign) {
			return true
		}
	}
	return false
}

// pump reads ffmpeg's MJPEG stdout and forwards each complete image, calling
// alive for every frame so the caller can tell a slow camera from a wedged one.
func (u *UVC) pump(ctx context.Context, stdout io.Reader, out chan<- core.Frame, alive func()) (uint64, error) {
	assembler := newFrameAssembler(core.SplitJPEGStream, u.cfg.MaxFrameSize)
	buf := make([]byte, readChunk)
	var count uint64

	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			for _, f := range assembler.feed(buf[:n]) {
				// Frames, not bytes: ffmpeg writing something that never
				// assembles into an image is as dead as ffmpeg writing
				// nothing, and only one of those would be noticed otherwise.
				alive()
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
// next to the executable, then PATH, then one fetched by the bridge itself.
//
// The fetched copy comes last on purpose. It is the one PaperBridge manages, so
// it is also the one the user cannot easily choose against -- putting it ahead
// of PATH would mean an installation that has deliberately been pointed at a
// particular ffmpeg silently stops using it the first time somebody clicks the
// tray item.
func (u *UVC) ffmpegPath() (string, error) {
	if u.cfg.FFmpegPath != "" {
		if _, err := os.Stat(u.cfg.FFmpegPath); err != nil {
			return "", fmt.Errorf("uvc: configured ffmpeg_path %q is not usable: %w", u.cfg.FFmpegPath, err)
		}
		return u.cfg.FFmpegPath, nil
	}
	if exe, err := os.Executable(); err == nil {
		alongside := filepath.Join(filepath.Dir(exe), ffmpegBinaryName())
		if _, statErr := os.Stat(alongside); statErr == nil {
			return alongside, nil
		}
	}
	if path, err := exec.LookPath(ffmpegBinaryName()); err == nil {
		return path, nil
	}
	if path, ok := ffmpegfetch.Installed(); ok {
		return path, nil
	}
	return "", ErrNoFFmpeg
}

// ErrNoFFmpeg means there is no ffmpeg anywhere the driver looks.
//
// Deliberately not fatal: unlike a mistyped ffmpeg_path, this is a state the
// machine can leave without the bridge being restarted -- the user fetches
// ffmpeg from the tray, or installs one on PATH -- and the retry loop is what
// notices. It is the same reasoning as a camera that is not plugged in yet.
var ErrNoFFmpeg = errors.New("uvc: ffmpeg not found next to the executable, on PATH, or in the settings folder; fetch it from the tray menu or set source.uvc.ffmpeg_path")

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
