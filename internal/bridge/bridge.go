// Package bridge wires a capture source to the frame hub and owns the
// lifecycle of both, so the tray menu and the management API can change the
// source at runtime without either of them knowing how a driver is built.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	return &Bridge{hub: h, status: st, log: log, cfgPath: cfgPath, cfg: cfg}
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

	if b.stream != nil {
		b.stream.SetStreamOptions(encoder, cfg.Server.HoldOnSourceLoss)
	}

	if b.cfgPath != "" {
		if err := config.Save(b.cfgPath, cfg); err != nil {
			// The running configuration is already correct, so nothing is torn
			// down; the caller is told so it can say the change is temporary.
			b.log.Error("settings applied but could not be saved", "path", b.cfgPath, "error", err)
			return fmt.Errorf("%w to %s: %w", config.ErrNotSaved, b.cfgPath, err)
		}
	}
	return nil
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

	cameras, err := source.ListDevices(ctx, b.Snapshot().Source.UVC.FFmpegPath)
	if err != nil {
		b.log.Warn("could not list capture devices", "error", err)
	}
	devices.Cameras = cameras

	ports, err := source.ListSerialPorts()
	if err != nil {
		b.log.Warn("could not list serial ports", "error", err)
	}
	devices.SerialPorts = ports

	return devices, nil
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
		_, err := b.newSource(b.status)
		return err
	}

	// The reporter is how a driver announces its first frame, so wrapping it
	// is what turns "started" into "working" without the driver knowing.
	firstFrame := make(chan struct{})
	reporter := &frameWatcher{inner: b.status, seen: firstFrame}

	drv, err := b.newSource(reporter)
	if err != nil {
		return err
	}

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
		pump(frames, transform, b.hub, log)
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
	case <-firstFrame:
	case err := <-failed:
		b.stopLocked()
		return err
	case <-timer.C:
		b.stopLocked()
		return fmt.Errorf("bridge: %s produced no frame within %s", drv.Name(), startVerifyTimeout)
	case <-b.root.Done():
		return nil
	}

	b.log.Info("source started", "source", drv.Name())
	return nil
}

// frameWatcher closes seen the first time a driver reports a frame, and
// otherwise forwards everything to the tracker unchanged.
type frameWatcher struct {
	inner source.Reporter
	seen  chan struct{}
	once  sync.Once
}

func (w *frameWatcher) Connected(name string) {
	w.once.Do(func() { close(w.seen) })
	w.inner.Connected(name)
}

func (w *frameWatcher) Disconnected(name string, err error) {
	// Deliberately not a verdict: the first attempt failing is normal for a
	// camera that has to fall back from passthrough to re-encoding.
	w.inner.Disconnected(name, err)
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

// pump applies the optional transform and publishes each frame.
func pump(frames <-chan core.Frame, transform core.Transform, h *hub.Hub, log *slog.Logger) {
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
	}
}

// newSource builds the driver named by the current settings, reporting its
// connection state to reporter.
func (b *Bridge) newSource(reporter source.Reporter) (source.Source, error) {
	cfg := b.cfg
	switch cfg.Source.Type {
	case config.SourceUVC:
		return source.NewUVC(source.UVCConfig{
			Device:       cfg.Source.UVC.Device,
			Size:         cfg.Source.UVC.Size,
			Framerate:    cfg.Source.UVC.Framerate,
			FFmpegPath:   cfg.Source.UVC.FFmpegPath,
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, reporter)

	case config.SourceSerial:
		return source.NewSerial(source.SerialConfig{
			Port:         cfg.Source.Serial.Port,
			Baud:         cfg.Source.Serial.Baud,
			Header:       cfg.SerialHeader(),
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, reporter)

	case config.SourceMJPEG:
		return source.NewMJPEGProxy(source.MJPEGConfig{
			URL:          cfg.Source.MJPEG.URL,
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, reporter)

	default:
		return nil, fmt.Errorf("bridge: unknown source type %q", cfg.Source.Type)
	}
}
