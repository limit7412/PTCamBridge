//go:build !windows

package tray

import "context"

// Run blocks until ctx is cancelled.
//
// The tray only exists on Windows, which is the platform the PaperTracker
// client ships for. Elsewhere the bridge runs headless and the same control
// surface is available over the management API.
func Run(ctx context.Context, opts Options) {
	opts.Log.Info("no system tray on this platform, running headless", "address", opts.Address)
	<-ctx.Done()
	if opts.OnQuit != nil {
		opts.OnQuit()
	}
}
