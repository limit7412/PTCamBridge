// Package tray is the system tray front end: source selection, pause, status
// and the autostart toggle.
//
// The menu is the only UI PaperBridge has. Anything richer belongs in a
// separate process talking to the management API, which is why the controller
// interface here mirrors what that API exposes.
package tray

import (
	"context"
	_ "embed"
	"reflect"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/status"

	"log/slog"
)

//go:embed icon.ico
var iconICO []byte

// sourceChoice is one entry in the source submenu.
type sourceChoice struct {
	kind  string
	label string
}

var sourceChoices = []sourceChoice{
	{config.SourceUVC, "UVC camera (USB)"},
	{config.SourceSerial, "Wired board (serial)"},
	{config.SourceMJPEG, "MJPEG stream (WiFi)"},
}

// Controller is the slice of the bridge the menu drives.
type Controller interface {
	Snapshot() config.Config
	Switch(ctx context.Context, sourceType string) error
	Paused() bool
	SetPaused(paused bool) error
}

// Options configures the tray.
type Options struct {
	Controller Controller
	Hub        *hub.Hub
	Status     *status.Tracker
	Log        *slog.Logger
	// Address is the bound listen address, shown in the menu and used for the
	// "open snapshot" item.
	Address string
	// LogDir is opened by the "open log folder" item; empty hides it.
	LogDir string
	// ConfigPath is opened by the "edit settings" item; empty hides it.
	ConfigPath string
	// ConfigFlag is the settings path the user named on the command line, if
	// any. The autostart toggle registers it so a sign-in launch uses the same
	// file; empty means the default location.
	ConfigFlag string
	// OnQuit is called when the user chooses Quit, before the tray exits.
	OnQuit func()
}

// actionPause is the key watchClicks reports a pause click under. It shares a
// namespace with the source type names, which are uvc, serial and mjpeg.
const actionPause = "pause"

// watchClicks forwards clicks from every menu entry that changes the bridge
// onto one channel, in the order they arrive.
//
// One goroutine watching every channel, rather than one per entry and a
// separate case in the event loop. Both of those hand the order to something
// other than the user: with a goroutine each, two clicks are received
// independently and then race to forward; with separate select cases, Go picks
// among the ready ones at random. Either way, choosing a source and then
// pausing can arrive the other way round -- and since a source cannot be
// changed while paused, that turns into the old source being paused instead of
// the new one.
//
// It hands the click on without blocking and goes straight back to watching.
// systray sends with a select and a default, so a click lands only if a
// receiver is parked on that exact channel at that moment and is dropped
// otherwise; anything slow here would lose clicks rather than reorder them,
// which is the worse of the two.
func watchClicks(ctx context.Context, log *slog.Logger, entries map[string]<-chan struct{}, out chan<- string) {
	kinds := make([]string, 0, len(entries))
	cases := make([]reflect.SelectCase, 0, len(entries)+1)
	for kind, clicked := range entries {
		kinds = append(kinds, kind)
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(clicked),
		})
	}
	// Last, so its index is len(kinds).
	cases = append(cases, reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(ctx.Done()),
	})

	for {
		chosen, _, ok := reflect.Select(cases)
		if chosen == len(kinds) || !ok {
			// Shutting down, or the menu item was removed.
			return
		}
		select {
		case out <- kinds[chosen]:
		default:
			// Only reachable if the worker is far enough behind to fill the
			// buffer, which takes a settings change slow enough to hit the
			// verification timeout. Blocking instead would stop watching the
			// other entries, and systray drops clicks nobody is waiting on.
			log.Warn("ignoring a menu click, earlier ones are still being applied", "action", kinds[chosen])
		}
	}
}
