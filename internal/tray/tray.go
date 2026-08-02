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

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/status"

	"log/slog"
)

//go:embed icon.ico
var iconICO []byte

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
