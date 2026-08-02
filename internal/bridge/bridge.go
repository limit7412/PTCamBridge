// Package bridge wires a capture source to the frame hub and owns the
// lifecycle of both, so the tray menu and the management API can change the
// source at runtime without either of them knowing how a driver is built.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/server"
	"github.com/limit7412/PTCamBridge/internal/source"
	"github.com/limit7412/PTCamBridge/internal/status"
)

// frameQueueDepth is the slot between the driver and the transform step. One
// frame is enough: the hub past it never blocks, and a deeper queue would only
// add latency.
const frameQueueDepth = 1

// startVerifyTimeout is how long Apply waits for a new source to prove itself
// before giving up on it.
//
// The proof is the first frame, not the absence of an early error: a driver
// reconnects on its own, so "has not failed yet" says nothing. The window has
// to cover a whole first attempt -- the MJPEG connect timeout is five seconds,
// and a camera that has to fall back from passthrough to re-encoding spends a
// backoff and a second ffmpeg startup getting there.
var startVerifyTimeout = 10 * time.Second

// StreamConfigurator receives the parts of a settings change that the HTTP
// server owns and must adopt for itself.
type StreamConfigurator interface {
	SetStreamOptions(encoder core.MultipartEncoder, holdOnSourceLoss bool)
}

// Bridge owns the active source and republishes its frames on the hub.
type Bridge struct {
	hub    *hub.Hub
	status *status.Tracker
	log    *slog.Logger

	// cfgPath is where Apply persists settings; empty disables persistence.
	cfgPath string

	// persistBase is what the settings file said, before the environment and
	// the command line were layered on. Saving starts from this so a
	// -device or a PAPERBRIDGE_* meant for one run is not written back as if
	// the user had chosen it permanently.
	persistBase config.Config

	// stream is told about settings the HTTP server has to reapply itself.
	// It is set once during wiring, before anything can call Apply.
	stream StreamConfigurator

	mu      sync.Mutex
	cfg     config.Config
	root    context.Context
	cancel  context.CancelFunc
	stopped chan struct{}
	paused  bool
}

// New builds a bridge for the given settings. Start must be called to begin
// capturing.
func New(cfg config.Config, cfgPath string, h *hub.Hub, st *status.Tracker, log *slog.Logger) *Bridge {
	return &Bridge{hub: h, status: st, log: log, cfgPath: cfgPath, cfg: cfg, persistBase: cfg}
}

// SetPersistBase records the settings as the file has them, which is what
// saving builds on. Without it the effective settings are saved verbatim, and
// a one-off override becomes permanent the first time anything calls Apply.
func (b *Bridge) SetPersistBase(cfg config.Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.persistBase = cfg
}

// SetStreamConfigurator registers the HTTP server so that Apply can hand it the
// stream settings it owns. It must be called during wiring, before the
// management API or the tray can reach Apply.
func (b *Bridge) SetStreamConfigurator(sc StreamConfigurator) {
	b.stream = sc
}

// Start begins capturing and keeps doing so until ctx is cancelled. The
// context also bounds every source started later through Apply or Switch.
func (b *Bridge) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.root != nil {
		return errors.New("bridge: already started")
	}
	b.root = ctx
	// Startup does not verify: a camera that is not plugged in yet has to be
	// picked up when it appears, and tearing the driver down would stop that.
	return b.startLocked(false)
}

// Stop halts capture and waits for the driver to finish releasing its device.
func (b *Bridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopLocked()
}

// Snapshot returns the settings currently in effect.
func (b *Bridge) Snapshot() config.Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

// Apply adopts new settings, restarting the source, and persists them when a
// config path was provided. The settings are only kept if the new source
// starts, so a bad device name does not leave the bridge with nothing running.
//
// Settings that only take effect at startup are rejected rather than accepted
// and quietly ignored; see restartRequired. A failure to persist is reported
// as an error wrapping config.ErrNotSaved, because the caller has to know that
// what it just changed will not survive a restart.
func (b *Bridge) Apply(_ context.Context, cfg config.Config) error {
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	previous := b.cfg
	if err := restartRequired(previous, cfg); err != nil {
		return err
	}

	// Build the encoder before anything is torn down: an unusable boundary
	// should not cost the user the source that is running right now.
	encoder, err := core.NewMultipartEncoder(cfg.Server.Boundary, cfg.StreamHeaders())
	if err != nil {
		return fmt.Errorf("server.boundary: %w", err)
	}

	if captureUnchanged(previous, cfg) {
		// Only the server-side settings moved, so the camera is left alone.
		// Restarting it would interrupt the stream for nothing, and the
		// verification below would reject the change outright while the camera
		// happened to be reconnecting -- including hold_on_source_loss, which
		// is the setting for exactly that situation.
		b.cfg = cfg
	} else {
		b.stopLocked()
		b.cfg = cfg

		if err := b.startLocked(true); err != nil {
			b.log.Error("new settings could not start a source, reverting", "error", err)
			b.cfg = previous
			// The previous source was working, so it is put back without being
			// made to prove itself again.
			if revertErr := b.startLocked(false); revertErr != nil {
				return fmt.Errorf("apply failed (%w) and the previous source could not be restored: %v", err, revertErr)
			}
			return err
		}
	}

	if b.stream != nil {
		b.stream.SetStreamOptions(encoder, cfg.Server.HoldOnSourceLoss)
	}

	if b.cfgPath != "" {
		saved := mergeChanges(b.persistBase, previous, cfg)
		b.persistBase = saved
		if err := config.Save(b.cfgPath, saved); err != nil {
			// The running configuration is already correct, so nothing is torn
			// down; the caller is told so it can say the change is temporary.
			b.log.Error("settings applied but could not be saved", "path", b.cfgPath, "error", err)
			return fmt.Errorf("%w to %s: %w", config.ErrNotSaved, b.cfgPath, err)
		}
	}
	return nil
}

// mergeChanges returns base with every value this change actually touched
// taken from next.
//
// The point is what it leaves alone: a field the caller did not change keeps
// whatever the settings file had, so an override that only applies to this run
// is not written back by an unrelated change somewhere else in the tree.
func mergeChanges(base, previous, next config.Config) config.Config {
	out := base
	mergeChanged(reflect.ValueOf(&out).Elem(), reflect.ValueOf(previous), reflect.ValueOf(next))
	return out
}

// mergeChanged walks the settings tree and copies the leaves that differ.
func mergeChanged(out, previous, next reflect.Value) {
	if out.Kind() == reflect.Struct {
		for i := 0; i < out.NumField(); i++ {
			mergeChanged(out.Field(i), previous.Field(i), next.Field(i))
		}
		return
	}
	if !reflect.DeepEqual(previous.Interface(), next.Interface()) {
		out.Set(next)
	}
}

// captureUnchanged reports whether two settings would build and run the same
// source, which is what decides if a change has to interrupt the camera.
//
// Only the selected source's own settings count. Comparing the whole Source
// tree would restart a working camera because the user filled in the MJPEG URL
// they intend to switch to later, and while that camera was reconnecting the
// change would be rejected outright.
//
// A source type added later falls through to the default and restarts, which
// is the safe answer; a new field inside an existing source is caught by the
// struct comparisons.
func captureUnchanged(previous, next config.Config) bool {
	if previous.Source.Type != next.Source.Type ||
		previous.Source.MaxFrameSize != next.Source.MaxFrameSize ||
		previous.Transform != next.Transform {
		return false
	}
	switch next.Source.Type {
	case config.SourceUVC:
		return previous.Source.UVC == next.Source.UVC
	case config.SourceSerial:
		return previous.Source.Serial.Port == next.Source.Serial.Port &&
			previous.Source.Serial.Baud == next.Source.Serial.Baud &&
			slices.Equal(previous.Source.Serial.Header, next.Source.Serial.Header)
	case config.SourceMJPEG:
		return previous.Source.MJPEG == next.Source.MJPEG
	default:
		return false
	}
}

// restartRequired rejects changes to settings that are only read while the
// process starts. Accepting them would report success and persist the value
// while the running bridge kept using the old one.
func restartRequired(previous, next config.Config) error {
	switch {
	case previous.Server.Listen != next.Server.Listen:
		return errors.New("server.listen cannot be changed while running: edit the settings file and restart")
	case previous.Log.Level != next.Log.Level:
		return errors.New("log.level cannot be changed while running: edit the settings file and restart")
	case previous.Log.Dir != next.Log.Dir:
		return errors.New("log.dir cannot be changed while running: edit the settings file and restart")
	case previous.PaperTracker != next.PaperTracker:
		return errors.New("papertracker settings are only read at startup: edit the settings file and restart")
	}
	return nil
}

// Switch changes the active source type, leaving everything else alone.
func (b *Bridge) Switch(ctx context.Context, sourceType string) error {
	cfg := b.Snapshot()
	cfg.Source.Type = sourceType
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return err
	}
	return b.Apply(ctx, cfg)
}

// Devices lists the cameras and serial ports available right now.
func (b *Bridge) Devices(ctx context.Context) (server.Devices, error) {
	var devices server.Devices

	cameras, cameraErr := source.ListDevices(ctx, b.Snapshot().Source.UVC.FFmpegPath)
	if cameraErr != nil {
		b.log.Warn("could not list capture devices", "error", cameraErr)
	}
	devices.Cameras = cameras

	ports, serialErr := source.ListSerialPorts()
	if serialErr != nil {
		b.log.Warn("could not list serial ports", "error", serialErr)
	}
	devices.SerialPorts = ports

	// Enumeration failing is not the same as finding nothing, and the caller
	// is the only one that can tell the user which it was. Swallowing it here
	// leaves the tray and the API showing an empty list as if the machine had
	// no camera at all.
	return devices, errors.Join(cameraErr, serialErr)
}

// SetPaused stops or resumes capture. Pausing releases the camera, which
// matters for UVC: the device is exclusive, and Baballonia cannot open it
// while the bridge holds it.
func (b *Bridge) SetPaused(paused bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.paused == paused {
		return nil
	}
	b.paused = paused
	b.status.SetPaused(paused)

	if paused {
		b.stopLocked()
		b.log.Info("capture paused")
		return nil
	}
	b.log.Info("capture resumed")
	return b.startLocked(false)
}

// Paused reports whether capture is currently paused.
func (b *Bridge) Paused() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paused
}

// startLocked builds and launches the configured driver. The caller holds mu.
//
// With verify set it waits for the source to deliver a frame, and returns an
// error if it does not, leaving nothing running. That is what Apply needs:
// only a frame proves a source works, since a driver that cannot reach its
// camera reconnects rather than failing. Without verify the driver is left to
// retry in the background, which is what startup wants -- a camera plugged in
// after sign-in still has to be picked up.
func (b *Bridge) startLocked(verify bool) error {
	if b.root == nil || b.paused || b.root.Err() != nil {
		// Nothing is going to run, but the settings still have to be able to
		// produce a driver. Accepting them unchecked while paused would save a
		// configuration that Resume then cannot start, with the previous
		// working one already gone.
		_, err := b.newSource()
		return err
	}

	drv, err := b.newSource()
	if err != nil {
		return err
	}

	// Verification watches the hub, not the driver. A driver announces a
	// frame as soon as it has parsed one, but the transform sits between
	// there and the hub and can still drop it -- an image over the pixel
	// limit, or one the decoder rejects. Only a frame that reached the hub
	// means a client would see anything.
	published := make(chan struct{})
	var publishedOnce sync.Once
	onPublish := func() { publishedOnce.Do(func() { close(published) }) }

	ctx, cancel := context.WithCancel(b.root)
	frames := make(chan core.Frame, frameQueueDepth)
	stopped := make(chan struct{})
	// Buffered so the driver never blocks reporting a failure nobody is
	// waiting for any more.
	failed := make(chan error, 1)

	b.cancel = cancel
	b.stopped = stopped
	b.status.SetSource(drv.Name())

	transform := b.cfg.CoreTransform()
	log := b.log

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		// The driver owns the frame channel and closes it so the pump can
		// drain what is already in flight before exiting.
		defer close(frames)
		// Run only returns on cancellation or on a failure retrying cannot
		// fix, so any error here means this source will never produce a frame.
		if err := drv.Run(ctx, frames); err != nil && ctx.Err() == nil {
			log.Error("source stopped", "source", drv.Name(), "error", err)
			failed <- err
		}
	}()

	go func() {
		defer wg.Done()
		pump(frames, transform, b.hub, log, onPublish)
	}()

	go func() {
		wg.Wait()
		close(stopped)
	}()

	if !verify {
		b.log.Info("source started", "source", drv.Name())
		return nil
	}

	timer := time.NewTimer(startVerifyTimeout)
	defer timer.Stop()
	select {
	case <-published:
	case err := <-failed:
		b.stopLocked()
		return err
	case <-timer.C:
		b.stopLocked()
		return fmt.Errorf("bridge: %s produced no frame within %s", drv.Name(), startVerifyTimeout)
	case <-b.root.Done():
		// Shutting down is not proof that anything works. Reporting success
		// here would persist a configuration nothing ever verified, and the
		// next run would start on it.
		b.stopLocked()
		return errors.New("bridge: shutting down before the new source produced a frame")
	}

	b.log.Info("source started", "source", drv.Name())
	return nil
}

// stopLocked cancels the running driver and waits for it to exit. Waiting is
// what guarantees an exclusive device is free before the next driver opens it.
// The caller holds mu.
func (b *Bridge) stopLocked() {
	if b.cancel == nil {
		return
	}
	b.cancel()
	<-b.stopped
	b.cancel, b.stopped = nil, nil
	b.status.SetSource("")
}

// pump applies the optional transform and publishes each frame, calling
// onPublish for every frame that makes it to the hub.
func pump(frames <-chan core.Frame, transform core.Transform, h *hub.Hub, log *slog.Logger, onPublish func()) {
	for frame := range frames {
		if !transform.IsNoop() {
			data, err := transform.Apply(frame.Data)
			if err != nil {
				log.Warn("dropping a frame the transform could not handle", "error", err)
				continue
			}
			frame.Data = data
		}
		h.Publish(frame)
		onPublish()
	}
}

// newSource builds the driver named by the current settings.
func (b *Bridge) newSource() (source.Source, error) {
	cfg := b.cfg
	switch cfg.Source.Type {
	case config.SourceUVC:
		return source.NewUVC(source.UVCConfig{
			Device:       cfg.Source.UVC.Device,
			Size:         cfg.Source.UVC.Size,
			Framerate:    cfg.Source.UVC.Framerate,
			FFmpegPath:   cfg.Source.UVC.FFmpegPath,
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, b.status)

	case config.SourceSerial:
		return source.NewSerial(source.SerialConfig{
			Port:         cfg.Source.Serial.Port,
			Baud:         cfg.Source.Serial.Baud,
			Header:       cfg.SerialHeader(),
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, b.status)

	case config.SourceMJPEG:
		return source.NewMJPEGProxy(source.MJPEGConfig{
			URL:          cfg.Source.MJPEG.URL,
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, b.status)

	default:
		return nil, fmt.Errorf("bridge: unknown source type %q", cfg.Source.Type)
	}
}
