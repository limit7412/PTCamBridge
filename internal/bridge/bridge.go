// Package bridge wires a capture source to the frame hub and owns the
// lifecycle of both, so the tray menu and the management API can change the
// source at runtime without either of them knowing how a driver is built.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/server"
	"github.com/limit7412/PTCamBridge/internal/source"
	"github.com/limit7412/PTCamBridge/internal/status"
)

// undecodableRecheckInterval is how often the pump tries to decode again while
// nothing from a source has decoded yet. Frames in between are dropped: they
// come from an upstream that has not yet produced a usable image, and decoding
// every one of them at capture rate would cost more than the answer is worth.
var undecodableRecheckInterval = 500 * time.Millisecond

// frameQueueDepth is the slot between the driver and the transform step. One
// frame is enough: the hub past it never blocks, and a deeper queue would only
// add latency.
const frameQueueDepth = 1

// startVerifyTimeout is how long Apply waits for a new source to prove itself
// before giving up on it.
//
// The proof is the first frame, not the absence of an early error: a driver
// reconnects on its own, so "has not failed yet" says nothing.
//
// The window has to outlast a whole first attempt, or a source that was going
// to work gets rolled back for being slow. The two worst cases are close to
// each other:
//
//   - MJPEG spends its connect timeout on each of dial, TLS and response
//     headers, then its stall timeout waiting for the first frame: 5s x 4.
//   - UVC spends its stall timeout on the first ffmpeg, and a camera with no
//     native MJPEG then spends a backoff and a second one: 10s + 1s + 10s.
//
// Nothing here costs a working source anything. It returns the moment a frame
// reaches the hub, so this only runs long when the answer is going to be no.
var startVerifyTimeout = 30 * time.Second

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
	// -device or a PTCAMBRIDGE_* meant for one run is not written back as if
	// the user had chosen it permanently.
	persistBase config.Config

	// unsaved is a write that never reached the file. It is nil while the
	// file is up to date. Keeping it means a retry writes the settings that
	// were lost rather than diffing against a running configuration that
	// already has them and finding nothing to do.
	unsaved *pendingSave

	// stream is told about settings the HTTP server has to reapply itself.
	// It is set once during wiring, before anything can call Apply.
	stream StreamConfigurator

	// view mirrors the state the tray polls once a second. Reading that
	// through mu would freeze the tray's whole event loop for the length of a
	// slow Apply -- up to startVerifyTimeout -- so the user could not even
	// quit while a failing source was being given its chance.
	view atomic.Pointer[view]

	mu      sync.Mutex
	cfg     config.Config
	root    context.Context
	cancel  context.CancelFunc
	stopped chan struct{}
	paused  bool

	// died says the driver launched under cfg stopped on its own -- an MJPEG
	// upstream answering 404, ffmpeg not on disk -- rather than being
	// cancelled. It is atomic because the driver goroutine is what learns it,
	// and that goroutine cannot take mu: a verifying Apply holds it while
	// waiting for exactly that news.
	//
	// Read through provenLocked rather than on its own. Startup and resume
	// launch without waiting for a frame, so they can only record that a
	// driver began; this is the other half of the answer, and without it a
	// source that died a second later still counts as working. The bridge
	// would then refuse to be handed a different one while paused, which is
	// the corner all of this exists to open up.
	died atomic.Bool

	// proven says the settings in cfg got a driver running, and that nothing
	// since has said otherwise.
	//
	// Two decisions need it, and both went wrong without it. Reverting a
	// failed change assumed the settings it went back to had been working;
	// when they had not -- the bridge started on a source it could never
	// build, which is what unconfigured UVC does -- the revert failed too and
	// left nothing capturing. Refusing a change while paused assumed there was
	// a working source to protect, and went on refusing when there was not.
	//
	// Pausing does not clear it. A source deliberately stopped is still one
	// that works, and that is exactly what the refusal above is protecting.
	proven bool
}

// pendingSave is a settings write that failed and still has to happen.
//
// Both halves are needed. want is the whole configuration, so it can be
// written as-is. from is what the file held when this pending write was first
// built, and it is the only thing that says which parts of want are the
// bridge's own doing: the leaves where they differ. Everything else in want is
// just a copy of the file at that moment, and laying that back over a file the
// user has edited since would undo the edit.
type pendingSave struct {
	want config.Config
	from config.Config
}

// view is the lock-free copy of what callers read but never change.
type view struct {
	cfg    config.Config
	paused bool
}

// provenLocked reports whether the current settings have a working source
// behind them: one that started, and that has not stopped on its own since.
// The caller holds mu.
func (b *Bridge) provenLocked() bool {
	return b.proven && !b.died.Load()
}

// publishView refreshes the lock-free copy. The caller holds mu.
func (b *Bridge) publishView() {
	b.view.Store(&view{cfg: b.cfg, paused: b.paused})
}

// New builds a bridge for the given settings. Start must be called to begin
// capturing.
func New(cfg config.Config, cfgPath string, h *hub.Hub, st *status.Tracker, log *slog.Logger) *Bridge {
	b := &Bridge{hub: h, status: st, log: log, cfgPath: cfgPath, cfg: cfg, persistBase: cfg}
	b.publishView()
	return b
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
	return b.startLocked()
}

// Stop halts capture and waits for the driver to finish releasing its device.
func (b *Bridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopLocked()
}

// Snapshot returns the settings currently in effect.
//
// It reads the lock-free copy, so it stays answerable while a settings change
// is in progress. A caller that needs the settings and the change to be one
// operation must hold mu itself; see Switch.
func (b *Bridge) Snapshot() config.Config {
	return b.view.Load().cfg
}

// Apply adopts new settings, restarting the source, and persists them when a
// config path was provided. The settings are only kept if the new source
// starts, so a bad device name does not leave the bridge with nothing running.
//
// Settings that only take effect at startup are rejected rather than accepted
// and quietly ignored; see restartRequired. A failure to persist is reported
// as an error wrapping config.ErrNotSaved, because the caller has to know that
// what it just changed will not survive a restart.
func (b *Bridge) Apply(ctx context.Context, cfg config.Config) error {
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return b.applyLocked(ctx, cfg)
}

// applyLocked is Apply with mu already held, so that a caller which has to
// read the current settings first can do the whole read-modify-apply without
// letting anything in between. The caller has already normalised and
// validated cfg.
func (b *Bridge) applyLocked(ctx context.Context, cfg config.Config) error {
	// Waiting for the lock can take as long as another caller's whole
	// verification, so the request that got here may already be gone. Nothing
	// below this point is free to happen on its behalf: a change that only
	// touches the server settings never reaches the verification and would
	// otherwise be applied and written out for a caller that was told the
	// request timed out.
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("bridge: the request ended before its change was applied: %w", ctx.Err())
	}

	previous := b.cfg
	// Read before anything is torn down: from here on the flag tracks the
	// settings being tried, and whether the ones being replaced were working
	// is the only thing that says a revert can succeed.
	provenBefore := b.provenLocked()
	if err := restartRequired(previous, cfg); err != nil {
		return err
	}

	// Apply's guarantee is that settings are only kept if the new source
	// starts, and while paused nothing can start: the most that could be
	// checked is that a driver object can be constructed, which says nothing
	// about the device being there. Accepting the change anyway would trade a
	// known-good configuration for an unproven one and write it out, and
	// resuming does not verify either -- a camera plugged in after sign-in has
	// to be picked up, so resume leaves the driver retrying exactly as startup
	// does. Saying so is better than any of that.
	//
	// All of which assumes there is a known-good configuration. With none --
	// nothing ever started, or what did has since failed -- the refusal
	// protects nothing and only takes away the one move left: picking a
	// different source. Somebody who pauses a bridge that is already down
	// would have to edit the settings file to get it back.
	if b.paused && b.provenLocked() && !captureUnchanged(previous, cfg) {
		return errors.New("capture is paused, so a new source cannot be tried: resume first, then change it")
	}

	// Build the encoder before anything is torn down: an unusable boundary
	// should not cost the user the source that is running right now.
	encoder, err := core.NewMultipartEncoder(cfg.Server.Boundary, cfg.StreamHeaders())
	if err != nil {
		return fmt.Errorf("server.boundary: %w", err)
	}

	if captureUnchanged(previous, cfg) && b.captureAsExpectedLocked() {
		// Only the server-side settings moved, so the camera is left alone.
		// Restarting it would interrupt the stream for nothing, and the
		// verification below would reject the change outright while the camera
		// happened to be reconnecting -- including hold_on_source_loss, which
		// is the setting for exactly that situation.
		b.cfg = cfg
		b.publishView()
	} else {
		b.stopLocked()
		b.cfg = cfg
		b.publishView()

		if err := b.verifyStartLocked(ctx); err != nil {
			b.log.Error("new settings could not start a source, reverting", "error", err)
			b.cfg = previous
			b.publishView()

			// The previous source is put back without being made to prove
			// itself again. That is right when it was working, and it is
			// still worth doing when it was not: tearing the new source down
			// cleared the status, and this is what puts the old source and
			// the reason it was not running back on the tray and /healthz.
			// Skipping it would answer a failed camera change by replacing a
			// precise complaint with "no source".
			if revertErr := b.startLocked(); revertErr != nil {
				if !provenBefore {
					// It was not running before this change either, so this
					// second failure is not news and has nothing to do with
					// what the caller asked for. Reporting the pair of them
					// buries the one that does.
					b.log.Warn("nothing is capturing: the settings in place before this change had not started a source either", "error", revertErr)
					return err
				}
				return fmt.Errorf("apply failed (%w) and the previous source could not be restored: %v", err, revertErr)
			}
			return err
		}
	}

	if b.stream != nil {
		b.stream.SetStreamOptions(encoder, cfg.Server.HoldOnSourceLoss)
	}

	if b.cfgPath != "" {
		// Changes are merged onto whatever is still waiting to be written, not
		// onto the file's last known contents. After a failed save the two are
		// not the same, and building on the file would drop the earlier change
		// on the floor -- including when the caller reacts to the error by
		// sending the very same settings again, which diffs to nothing against
		// the running configuration and would otherwise rewrite the stale file
		// and report success.
		base, baseErr := b.saveBaseLocked()
		saved := mergeChanges(base, previous, cfg)
		err := baseErr
		if err == nil {
			err = config.Save(b.cfgPath, saved)
		}
		if err != nil {
			// The running configuration is already correct, so nothing is torn
			// down; the caller is told so it can say the change is temporary.
			// The pending write is kept so a retry, or the next change, writes
			// it once the file can be written again.
			b.holdUnsavedLocked(base, previous, cfg)
			b.log.Error("settings applied but could not be saved", "path", b.cfgPath, "error", err)
			return fmt.Errorf("%w to %s: %w", config.ErrNotSaved, b.cfgPath, err)
		}
		b.persistBase = saved
		b.unsaved = nil
	}
	return nil
}

// captureAsExpectedLocked reports whether the capture is in the state the
// current settings ask for, which is what makes leaving it alone safe.
//
// A driver can stop on its own: Run returns a FatalError for something
// retrying cannot fix, such as an MJPEG upstream answering 404 or ffmpeg not
// being on disk yet. Nothing restarts it, and the settings that produced it
// are still the ones in b.cfg. Re-selecting that same source once the cause is
// dealt with is the obvious way to recover, and skipping the restart because
// the settings did not change would answer that with success while /healthz
// stayed at 503.
//
// Paused, stopped and not-yet-started all count as expected: nothing is meant
// to be running, so there is nothing to put right.
func (b *Bridge) captureAsExpectedLocked() bool {
	if b.root == nil || b.paused || b.root.Err() != nil {
		return true
	}
	if b.stopped == nil {
		return false
	}
	select {
	case <-b.stopped:
		// Both capture goroutines have exited.
		return false
	default:
		return true
	}
}

// holdUnsavedLocked records a change that could not be written, so a later
// save can still make it.
//
// A pending write already in hand is extended in place rather than rebuilt
// from the file. Its basis has to stay where it was: it is what separates the
// bridge's own changes from the file contents that happen to be sitting in
// want, and moving it forward would fold anything the user edited in the
// meantime into the set of values the bridge intends to write back.
func (b *Bridge) holdUnsavedLocked(base, previous, cfg config.Config) {
	if b.unsaved != nil {
		b.unsaved.want = mergeChanges(b.unsaved.want, previous, cfg)
		return
	}
	// base is the file as it was just read, so the difference between it and
	// what this change produces is exactly what the bridge is asking for. When
	// the read failed it is the last known contents instead, which is the best
	// guess available and no worse than the alternative of writing nothing.
	b.unsaved = &pendingSave{want: mergeChanges(base, previous, cfg), from: base}
}

// saveBaseLocked returns what the next save should build on: the settings file
// as it stands right now, plus anything an earlier save failed to write.
//
// Re-reading matters because the file is not only written from here. The tray
// offers "Edit settings", and the settings that need a restart can only be
// changed that way, so a user who edits server.listen and then touches
// anything in the tray before restarting would have had that edit written back
// over from a base captured at startup.
//
// A file that exists but cannot be read or parsed is an error rather than a
// reason to fall back: the likeliest way to get one is the user part way
// through editing it, and writing over that would destroy the edit to save a
// change that is already in effect and can be written later.
//
// A file that is not there at all is different. Nothing is being lost, so the
// last known contents are the right base and Save recreates the file from
// them.
// A read failure is reported but does not skip the pending write: the caller
// builds the next pending write from what comes back, and a base without the
// earlier changes in it would quietly drop them. Two changes made while the
// file is unparsable would otherwise leave only the second one to be written
// when it can be read again, because the first is already in the running
// configuration and so no longer shows up as a difference.
func (b *Bridge) saveBaseLocked() (config.Config, error) {
	// The last thing known about the file, in order of freshness. A pending
	// write recorded what the file held when it was built, which is newer than
	// the startup snapshot -- and it is what a recreated file has to be built
	// from, or an edit made before the file went missing comes back undone.
	base := b.persistBase
	if b.unsaved != nil {
		base = b.unsaved.from
	}
	var readErr error
	if _, err := os.Stat(b.cfgPath); err == nil {
		onDisk, err := config.LoadFile(b.cfgPath)
		if err != nil {
			readErr = fmt.Errorf("re-read %s before saving: %w", b.cfgPath, err)
		} else {
			base = onDisk
		}
	}
	if b.unsaved != nil {
		// Lay the pending write back on top of whatever the file says now.
		// Only the leaves it actually meant to change are copied; the rest of
		// want is a snapshot of the file from back then, and writing that back
		// would undo anything edited since.
		base = mergeChanges(base, b.unsaved.from, b.unsaved.want)
	}
	return base, readErr
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
	switch out.Kind() {
	case reflect.Struct:
		for i := 0; i < out.NumField(); i++ {
			mergeChanged(out.Field(i), previous.Field(i), next.Field(i))
		}
		return
	case reflect.Map:
		mergeChangedMap(out, previous, next)
		return
	}
	if !reflect.DeepEqual(previous.Interface(), next.Interface()) {
		out.Set(next)
	}
}

// mergeChangedMap copies the entries this change actually touched.
//
// A map is not one value. server.extra_headers is a set of independent
// settings that happens to be written as one table, and it is edited by hand
// at least as often as it is changed through the API -- adjusting the wire
// format for a PaperTracker release is what it exists for. Replacing it whole
// would take a header the user added to the file and drop it because the
// bridge changed a different one, which is the very thing merging by leaf is
// meant to prevent.
//
// A key removed by the change is removed here too: that is a change to that
// key like any other.
func mergeChangedMap(out, previous, next reflect.Value) {
	touched := make(map[any]reflect.Value)
	for _, key := range next.MapKeys() {
		was := previous.MapIndex(key)
		now := next.MapIndex(key)
		if !was.IsValid() || !reflect.DeepEqual(was.Interface(), now.Interface()) {
			touched[key.Interface()] = now
		}
	}
	for _, key := range previous.MapKeys() {
		if !next.MapIndex(key).IsValid() {
			// An invalid value stands for "this key is gone".
			touched[key.Interface()] = reflect.Value{}
		}
	}
	if len(touched) == 0 {
		return
	}

	// Built fresh rather than written into: the map in out is the same one the
	// base configuration holds, and the base is somebody else's copy -- the
	// last known file contents, or a pending write. Editing it in place would
	// change their idea of the file as a side effect of building this one.
	merged := reflect.MakeMap(out.Type())
	if !out.IsNil() {
		for _, key := range out.MapKeys() {
			merged.SetMapIndex(key, out.MapIndex(key))
		}
	}
	for key, value := range touched {
		merged.SetMapIndex(reflect.ValueOf(key), value)
	}
	out.Set(merged)
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
	case previous.UI != next.UI:
		// The tray builds its menu once, with the labels the language gave it.
		// Accepting the change would save it and report success while every
		// word on screen stayed as it was, so the API would disagree with the
		// interface until the next start.
		return errors.New("ui.language cannot be changed while running: edit the settings file and restart")
	}
	return nil
}

// Switch changes the active source type, leaving everything else alone.
//
// Reading the current settings and applying the modified copy is one exclusive
// operation. Split in two, a settings change that lands in the gap is undone:
// Switch would go on to apply a whole configuration it read before that
// change, and save it.
func (b *Bridge) Switch(ctx context.Context, sourceType string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	cfg := b.cfg
	cfg.Source.Type = sourceType
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return err
	}
	return b.applyLocked(ctx, cfg)
}

// Devices lists the cameras and serial ports available right now.
//
// The two are gathered independently and reported that way. They fail
// independently -- a machine with no ffmpeg still has serial ports -- and one
// error is not a reason to withhold the other list.
//
// Enumeration failing is also not the same as finding nothing, and the caller
// is the only one that can tell the user which it was. Swallowing it here
// leaves the tray and the API showing an empty list as if the machine had no
// camera at all.
func (b *Bridge) Devices(ctx context.Context) server.Devices {
	var devices server.Devices

	cameras, err := source.ListDevices(ctx, b.Snapshot().Source.UVC.FFmpegPath)
	if err != nil {
		b.log.Warn("could not list capture devices", "error", err)
		devices.CameraError = err.Error()
	}
	devices.Cameras = cameras

	ports, err := source.ListSerialPorts()
	if err != nil {
		b.log.Warn("could not list serial ports", "error", err)
		devices.SerialError = err.Error()
	}
	devices.SerialPorts = ports

	return devices
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
	b.publishView()
	b.status.SetPaused(paused)

	if paused {
		b.stopLocked()
		b.log.Info("capture paused")
		return nil
	}
	b.log.Info("capture resumed")
	if err := b.startLocked(); err != nil {
		// Resuming is not a claim that the source works, any more than
		// starting up is: both leave a driver retrying so that a camera
		// plugged in later is picked up. Failing the resume instead left the
		// bridge paused, and a paused bridge is one that cannot be handed a
		// different source -- the two together are a corner with no way out
		// short of editing the settings file.
		//
		// The reason is not lost by returning nil here. Whatever could not
		// start is on the status tracker, which is what the tray shows and
		// what /healthz answers with.
		b.log.Error("capture resumed but the source could not be started", "error", err)
	}
	return nil
}

// Paused reports whether capture is currently paused. Like Snapshot it reads
// the lock-free copy, so the tray can redraw during a slow settings change.
func (b *Bridge) Paused() bool {
	return b.view.Load().paused
}

// startLocked builds and launches the configured driver, leaving it to retry
// in the background. That is what startup and resume want: a camera plugged in
// after sign-in still has to be picked up. The caller holds mu.
func (b *Bridge) startLocked() error {
	return b.launchLocked(nil)
}

// verifyStartLocked launches the configured driver and waits for it to deliver
// a frame, returning an error with nothing running if it does not. That is
// what Apply needs: only a frame proves a source works, since a driver that
// cannot reach its camera reconnects rather than failing.
//
// The wait also ends if ctx does. ctx is the request that asked for the
// change, and once the client behind it has gone there is nobody left to tell
// that the new source came up -- carrying on would persist a setting the
// caller was told nothing about. The context bounds only the wait; the source
// itself lives on the bridge's own root context.
func (b *Bridge) verifyStartLocked(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return b.launchLocked(ctx)
}

// launchLocked builds and launches the configured driver. A non-nil verifyCtx
// asks it to wait for the first published frame; nil returns as soon as the
// driver is running. The caller holds mu.
func (b *Bridge) launchLocked(verifyCtx context.Context) error {
	if b.root == nil || b.paused || b.root.Err() != nil {
		// Nothing is going to run, but the settings still have to be able to
		// produce a driver. Accepting them unchecked while paused would save a
		// configuration that Resume then cannot start, with the previous
		// working one already gone.
		//
		// Constructing one is not running one, so these settings are unproven
		// whichever way it goes. Leaving the flag as it was would credit them
		// with what the settings they replaced had done.
		b.proven = false
		if _, err := b.newSource(); err != nil {
			return err
		}
		// Nothing started, but the settings did change, and the status is
		// where the tray and /healthz read the source from. Left alone it
		// keeps naming the source that was replaced, complete with the error
		// that source last had -- so a camera swapped while paused reads as
		// the old camera still failing, until somebody resumes.
		b.status.SetSource(b.cfg.Source.Type)
		return nil
	}

	drv, err := b.newSource()
	if err != nil {
		// Nothing will retry this: the settings themselves are unusable, so
		// there is no driver to reconnect. Without recording it the tray shows
		// "connecting..." and /healthz says only that the source is not
		// connected, both of which describe something that is trying. The
		// default settings have no UVC device name, so this is the first thing
		// a new user meets.
		b.proven = false
		b.status.SetSource(b.cfg.Source.Type)
		b.status.Disconnected(b.cfg.Source.Type, err)
		return err
	}

	// Verification watches the hub, not the driver. A driver announces a
	// frame as soon as it has parsed one, but the transform sits between
	// there and the hub and can still drop it -- an image over the pixel
	// limit, or one the decoder rejects. Only a frame that reached the hub
	// means a client would see anything.
	//
	// The pump decides that, and it does the same work whether anyone is
	// waiting for the answer or not: no frame is published until one has been
	// decoded. This is where the answer is collected when the caller needs it.
	published := make(chan error, 1)
	var publishedOnce sync.Once
	maxPixels := b.cfg.CoreTransform().MaxPixels
	firstFrame := func(err error) {
		publishedOnce.Do(func() { published <- err })
	}

	ctx, cancel := context.WithCancel(b.root)
	frames := make(chan core.Frame, frameQueueDepth)
	stopped := make(chan struct{})
	// Buffered so the driver never blocks reporting a failure nobody is
	// waiting for any more.
	failed := make(chan error, 1)

	b.cancel = cancel
	b.stopped = stopped
	b.died.Store(false)
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
		err := drv.Run(ctx, frames)
		if err != nil {
			// Recorded whatever the context says, because the two race: a
			// driver that has just returned its failure and a pause that
			// cancels before this line reads ctx.Err() would leave the
			// settings looking proven with nothing behind them, which is the
			// state this exists to rule out.
			//
			// Reading it here would also be asking the wrong question. A
			// driver returns nil when its own context is cancelled -- all
			// three do, and being able to say "it stopped because it was
			// told to" is the whole reason they bother -- so a non-nil error
			// is a failure of the source, not of the cancellation.
			b.died.Store(true)
		}
		if err != nil && ctx.Err() == nil {
			log.Error("source stopped", "source", drv.Name(), "error", err)
			failed <- err
		}
	}()

	go func() {
		defer wg.Done()
		pump(latestOnly(frames, log), transform, b.hub, log, maxPixels, firstFrame)
	}()

	go func() {
		wg.Wait()
		close(stopped)
	}()

	if verifyCtx == nil {
		b.proven = true
		b.log.Info("source started", "source", drv.Name())
		return nil
	}

	// Everything from here to the frame is a way of not getting one.
	b.proven = false

	timer := time.NewTimer(startVerifyTimeout)
	defer timer.Stop()
	var frameErr error
	select {
	case frameErr = <-published:
	case err := <-failed:
		b.stopLocked()
		return err
	case <-timer.C:
		b.stopLocked()
		return fmt.Errorf("bridge: %s produced no frame within %s", drv.Name(), startVerifyTimeout)
	case <-verifyCtx.Done():
		// The caller gave up waiting. It is going to report a failure, so
		// finishing the change behind its back would leave the running bridge
		// and the settings file on a source nobody was ever told about.
		b.stopLocked()
		return fmt.Errorf("bridge: %s was still starting when the request ended: %w", drv.Name(), verifyCtx.Err())
	case <-b.root.Done():
		// Shutting down is not proof that anything works. Reporting success
		// here would persist a configuration nothing ever verified, and the
		// next run would start on it.
		b.stopLocked()
		return errors.New("bridge: shutting down before the new source produced a frame")
	}

	if err := verifyOutcome(drv.Name(), frameErr, verifyCtx.Err(), b.root.Err()); err != nil {
		b.stopLocked()
		return err
	}

	b.proven = true
	b.log.Info("source started", "source", drv.Name())
	return nil
}

// verifyOutcome says what a delivered frame is worth, given whether the request
// that asked for the change and the bridge itself are still there.
//
// It is separate from the select above because a select cannot express
// priority. When the first frame and the request's deadline land together both
// cases are ready and either may be taken, so reading the frame case is not
// proof that nobody has given up waiting -- and taking it at face value applies
// the change and writes it to the settings file behind a client that was told
// it failed. Asking again once the frame is in hand makes the answer the same
// whichever case the select happened to pick.
func verifyOutcome(name string, frameErr, requestErr, shutdownErr error) error {
	if frameErr != nil {
		return fmt.Errorf("bridge: %s produced a frame that is not a usable JPEG: %w", name, frameErr)
	}
	if requestErr != nil {
		return fmt.Errorf("bridge: %s started as the request ended, so the change was not kept: %w", name, requestErr)
	}
	if shutdownErr != nil {
		// A configuration proved a moment before the bridge stops is still one
		// nothing has run on, and the next start would come up on it unverified.
		return errors.New("bridge: shutting down as the new source produced its first frame")
	}
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

// latestOnly forwards frames and keeps only the newest one waiting while the
// far side is busy.
//
// The driver hands its frames over a slot one frame deep, and it blocks when
// that slot is taken. A transform slower than the camera -- a rotate, or a
// re-encode -- is enough for that to happen every frame, and a blocked driver
// is a driver that has stopped reading its socket, its pipe or its serial
// port. The images pile up there instead, and the stream falls further behind
// live with every one of them: the hub's latest-frame-wins only applies after
// the transform, so it never sees them.
//
// Reading as fast as the driver can produce, and throwing away what the
// transform did not get to, is what keeps that queue from forming. Dropping
// frames is the intended answer here -- for mouth tracking the newest image is
// the only one worth having.
func latestOnly(in <-chan core.Frame, log *slog.Logger) <-chan core.Frame {
	out := make(chan core.Frame, 1)
	go func() {
		defer close(out)
		for frame := range in {
			select {
			case out <- frame:
				continue
			default:
			}
			// The slot holds a frame the transform has not taken yet, and it is
			// older than this one.
			select {
			case <-out:
				log.Debug("dropping a frame the transform did not keep up with")
			default:
			}
			select {
			case out <- frame:
			default:
			}
		}
	}()
	return out
}

// pump applies the optional transform and publishes each frame, calling
// onPublish with every frame that makes it to the hub.
func pump(frames <-chan core.Frame, transform core.Transform, h *hub.Hub, log *slog.Logger, maxPixels int, firstFrame func(error)) {
	// Nothing is published until one frame has been decoded. Everything before
	// this point checks structure, which is all a per-frame check can afford,
	// and a payload of SOI followed by EOI has the structure of a JPEG and no
	// image in it. A source sending those looks healthy the whole way through
	// -- frames counted, /healthz green, an fps in the tray -- while the
	// tracker gets nothing it can use, so the run does not count as working
	// until one frame proves it is.
	decoded := false
	// While nothing has decoded, frames are checked at intervals rather than
	// one by one: a decode is the expensive thing here, and an upstream sending
	// nothing usable at thirty frames a second should not cost thirty of them.
	var nextCheck time.Time
	// Only the first answer is passed on. It is the one a caller waiting on a
	// source switch asked for, and by the time a later frame decodes that
	// caller has already been told the source failed.
	reported := false
	report := func(err error) {
		if !reported {
			reported = true
			firstFrame(err)
		}
	}

	for frame := range frames {
		if !transform.IsNoop() {
			data, err := transform.Apply(frame.Data)
			if err != nil {
				log.Warn("dropping a frame the transform could not handle", "error", err)
				continue
			}
			frame.Data = data
		}

		switch {
		case !decoded:
			// The decode covers the header check as well, and it has to come
			// first: a frame with no readable header fails both, and "not a
			// usable image" is the answer that describes it.
			now := time.Now()
			if now.Before(nextCheck) {
				continue
			}
			if err := core.DecodableJPEG(frame.Data, maxPixels); err != nil {
				report(err)
				nextCheck = now.Add(undecodableRecheckInterval)
				log.Warn("dropping a frame that is not a usable image", "error", err)
				continue
			}
			report(nil)
			decoded = true

		case transform.IsNoop():
			// A frame forwarded untouched is never decoded on this side, so the
			// ceiling has to be applied to the header instead. The client is the
			// one that decodes it, and a few hundred bytes declaring 65535x65535
			// asks it for the allocation this limit exists to refuse. Reading a
			// header is cheap enough to do at capture rate; decoding is not.
			// Applying a transform already checks the same thing before it
			// decodes.
			if err := core.WithinPixelLimit(frame.Data, maxPixels); err != nil {
				log.Warn("dropping a frame that declares an image over the pixel limit", "error", err)
				continue
			}
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
