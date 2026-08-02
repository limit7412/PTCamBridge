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

// Bridge owns the active source and republishes its frames on the hub.
type Bridge struct {
	hub    *hub.Hub
	status *status.Tracker
	log    *slog.Logger

	// cfgPath is where Apply persists settings; empty disables persistence.
	cfgPath string

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

// Start begins capturing and keeps doing so until ctx is cancelled. The
// context also bounds every source started later through Apply or Switch.
func (b *Bridge) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.root != nil {
		return errors.New("bridge: already started")
	}
	b.root = ctx
	return b.startLocked()
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
func (b *Bridge) Apply(_ context.Context, cfg config.Config) error {
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	previous := b.cfg
	b.stopLocked()
	b.cfg = cfg

	if err := b.startLocked(); err != nil {
		b.log.Error("new settings could not start a source, reverting", "error", err)
		b.cfg = previous
		if revertErr := b.startLocked(); revertErr != nil {
			return fmt.Errorf("apply failed (%w) and the previous source could not be restored: %v", err, revertErr)
		}
		return err
	}

	if b.cfgPath != "" {
		if err := config.Save(b.cfgPath, cfg); err != nil {
			// The running configuration is already correct; only persistence
			// failed, so report it without tearing anything down.
			b.log.Error("settings applied but could not be saved", "path", b.cfgPath, "error", err)
		}
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
	return b.startLocked()
}

// Paused reports whether capture is currently paused.
func (b *Bridge) Paused() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paused
}

// startLocked builds and launches the configured driver. The caller holds mu.
func (b *Bridge) startLocked() error {
	if b.root == nil || b.paused || b.root.Err() != nil {
		return nil
	}
	drv, err := b.newSource()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(b.root)
	frames := make(chan core.Frame, frameQueueDepth)
	stopped := make(chan struct{})

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
		if err := drv.Run(ctx, frames); err != nil && ctx.Err() == nil {
			log.Error("source stopped", "source", drv.Name(), "error", err)
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
