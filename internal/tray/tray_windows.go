package tray

import (
	"context"
	"time"

	"fyne.io/systray"

	"github.com/limit7412/PTCamBridge/internal/autostart"
	"github.com/limit7412/PTCamBridge/internal/i18n"
)

// refreshInterval paces the status line in the menu. It is a display concern
// only; nothing in the pipeline depends on it.
const refreshInterval = time.Second

// Run shows the tray icon and blocks until the user quits or ctx is cancelled.
//
// It must be called from the main goroutine: the tray runs a Windows message
// loop, which is tied to the thread that created the window.
func Run(ctx context.Context, opts Options) {
	systray.Run(func() { onReady(ctx, opts) }, func() {
		if opts.OnQuit != nil {
			opts.OnQuit()
		}
	})
}

func onReady(ctx context.Context, opts Options) {
	p := opts.Printer

	systray.SetIcon(iconICO)
	systray.SetTitle(p.S(i18n.DialogTitle))
	systray.SetTooltip(p.S(i18n.DialogTitle))

	statusItem := systray.AddMenuItem(p.S(i18n.MenuStatusStarting), p.S(i18n.MenuStatusTip))
	statusItem.Disable()
	addressItem := systray.AddMenuItem("http://"+opts.Address, p.S(i18n.MenuAddressTip))
	systray.AddSeparator()

	sourceMenu := systray.AddMenuItem(p.S(i18n.MenuSource), p.S(i18n.MenuSourceTip))
	sourceItems := make(map[string]*systray.MenuItem, len(sourceChoices))
	for _, choice := range sourceChoices {
		label := p.S(choice.label)
		sourceItems[choice.kind] = sourceMenu.AddSubMenuItemCheckbox(label, label, false)
	}

	pauseItem := systray.AddMenuItemCheckbox(p.S(i18n.MenuPause), p.S(i18n.MenuPauseTip), opts.Controller.Paused())
	systray.AddSeparator()

	logItem := systray.AddMenuItem(p.S(i18n.MenuLogDir), p.S(i18n.MenuLogDirTip))
	if opts.LogDir == "" {
		logItem.Hide()
	}
	configItem := systray.AddMenuItem(p.S(i18n.MenuSettings), p.S(i18n.MenuSettingsTip))
	if opts.ConfigPath == "" {
		configItem.Hide()
	}

	ffmpegItem := systray.AddMenuItem(p.S(i18n.MenuFFmpegGet), p.S(i18n.MenuFFmpegTip))
	if opts.FFmpeg == nil {
		ffmpegItem.Hide()
	}

	autostartItem := systray.AddMenuItemCheckbox(p.S(i18n.MenuAutostart), p.S(i18n.MenuAutostartTip), false)
	if !autostart.Supported() {
		autostartItem.Hide()
	} else if on, err := autostart.Enabled(opts.ConfigFlag); err != nil {
		opts.Log.Warn("could not read the autostart entry", "error", err)
	} else if on {
		autostartItem.Check()
	}

	systray.AddSeparator()
	quitItem := systray.AddMenuItem(p.S(i18n.MenuQuit), p.S(i18n.MenuQuitTip))

	go run(ctx, opts, menu{
		status:    statusItem,
		address:   addressItem,
		sources:   sourceItems,
		pause:     pauseItem,
		logDir:    logItem,
		configure: configItem,
		ffmpeg:    ffmpegItem,
		autostart: autostartItem,
		quit:      quitItem,
	})
}

// menu groups the items so the event loop reads as a single switch.
type menu struct {
	status    *systray.MenuItem
	address   *systray.MenuItem
	sources   map[string]*systray.MenuItem
	pause     *systray.MenuItem
	logDir    *systray.MenuItem
	configure *systray.MenuItem
	ffmpeg    *systray.MenuItem
	autostart *systray.MenuItem
	quit      *systray.MenuItem
}

// run handles menu clicks and refreshes the status line until the tray exits.
func run(ctx context.Context, opts Options, m menu) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	// Every entry that changes the bridge is watched in one place, so the
	// order they reach the worker in is the order they were clicked. See
	// watchClicks for why a case each in the select below would not do.
	actions := make(chan string, commandQueueDepth)
	clicks := make(map[string]<-chan struct{}, len(m.sources)+1)
	for kind, item := range m.sources {
		clicks[kind] = item.ClickedCh
	}
	clicks[actionPause] = m.pause.ClickedCh
	go watchClicks(ctx, opts.Log, clicks, actions)

	// Everything that changes the bridge goes through one worker, in the
	// order it was clicked. See commandQueue.
	queue := newCommandQueue(opts.Log, commandQueueDepth)
	defer queue.close()

	wantPaused := opts.Controller.Paused()

	refresh(opts, m)
	for {
		select {
		case <-ctx.Done():
			systray.Quit()
			return

		case <-ticker.C:
			refresh(opts, m)

		case action := <-actions:
			// Nothing is refreshed on completion. The ticker redraws once a
			// second from state the bridge exposes without a lock, so the menu
			// catches up on its own whichever way the action goes.
			if action == actionPause {
				// The toggle is against what the last click asked for, not
				// what the bridge currently reports. Two clicks in quick
				// succession both see the old state otherwise -- the first has
				// not reached SetPaused yet -- so they ask for the same thing
				// twice and the pair does not cancel out.
				//
				// Only a request that was actually queued counts. A dropped
				// one changed nothing, and moving the target anyway would
				// leave the next click asking for the state the bridge is
				// already in, which looks like a button that does nothing.
				paused := !wantPaused
				queued := queue.submit(actionPause, func() {
					if err := opts.Controller.SetPaused(paused); err != nil {
						opts.Log.Error("could not change the paused state", "paused", paused, "error", err)
					}
				})
				if queued {
					wantPaused = paused
				}
			} else {
				kind := action
				queue.submit("switch source", func() {
					if err := opts.Controller.Switch(ctx, kind); err != nil {
						opts.Log.Error("could not switch source", "source", kind, "error", err)
					}
				})
			}
			refresh(opts, m)

		case <-m.address.ClickedCh:
			openTarget("http://"+opts.Address+"/snapshot", opts)

		case <-m.logDir.ClickedCh:
			openTarget(opts.LogDir, opts)

		case <-m.configure.ClickedCh:
			openTarget(opts.ConfigPath, opts)

		case <-m.ffmpeg.ClickedCh:
			// Not on the command queue: this neither touches the bridge's
			// settings nor competes with a source switch, and it blocks on a
			// person reading a dialog. Queueing it would hold every later
			// click behind however long that takes.
			go startFFmpegFetch(opts)

		case <-m.autostart.ClickedCh:
			toggleAutostart(opts, m)

		case <-m.quit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// refresh redraws the parts of the menu that reflect live state.
func refresh(opts Options, m menu) {
	snapshot := opts.Status.Snapshot()
	stats := opts.Hub.Stats()
	cfg := opts.Controller.Snapshot()

	for kind, item := range m.sources {
		if kind == cfg.Source.Type {
			item.Check()
		} else {
			item.Uncheck()
		}
	}

	paused := opts.Controller.Paused()
	if paused {
		m.pause.Check()
	} else {
		m.pause.Uncheck()
	}

	if opts.FFmpeg != nil {
		m.ffmpeg.SetTitle(ffmpegStatusLine(opts.Printer, opts.FFmpeg.State()))
	}

	m.status.SetTitle(statusLine(opts.Printer, snapshot, paused, stats.InputFPS, stats.Subscribers))
	systray.SetTooltip(opts.Printer.S(i18n.DialogTitle) + " - " + m.status.String())
}

// startFFmpegFetch asks first, then downloads.
func startFFmpegFetch(opts Options) {
	if opts.FFmpeg == nil {
		return
	}
	p := opts.Printer
	state := opts.FFmpeg.State()
	switch {
	case state.Downloading:
		return
	case state.Installed:
		// Already there. Saying so beats a click that looks like it did
		// nothing, and re-downloading a working ffmpeg is not what it means.
		confirm(p.S(i18n.DialogTitle), p.F(i18n.DialogFFmpegInstalled, state.Path))
		return
	}
	if !confirm(p.S(i18n.DialogFFmpegTitle), ffmpegPrompt(p, state.Source)) {
		return
	}
	if err := opts.FFmpeg.Start(); err != nil {
		opts.Log.Error("could not start the ffmpeg download", "error", err)
	}
}

func toggleAutostart(opts Options, m menu) {
	var err error
	if m.autostart.Checked() {
		err = autostart.Disable()
	} else {
		err = autostart.Enable(opts.ConfigFlag)
	}
	if err != nil {
		opts.Log.Error("could not change the autostart entry", "error", err)
		return
	}
	if m.autostart.Checked() {
		m.autostart.Uncheck()
	} else {
		m.autostart.Check()
	}
}

func openTarget(target string, opts Options) {
	if target == "" {
		return
	}
	if err := openPath(target); err != nil {
		opts.Log.Error("could not open", "target", target, "error", err)
	}
}
