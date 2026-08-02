package tray

import (
	"context"
	"fmt"
	"time"

	"fyne.io/systray"

	"github.com/limit7412/PTCamBridge/internal/autostart"
	"github.com/limit7412/PTCamBridge/internal/config"
)

// refreshInterval paces the status line in the menu. It is a display concern
// only; nothing in the pipeline depends on it.
const refreshInterval = time.Second

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
	systray.SetIcon(iconICO)
	systray.SetTitle("PaperBridge")
	systray.SetTooltip("PaperBridge")

	statusItem := systray.AddMenuItem("Starting...", "Current source and frame rate")
	statusItem.Disable()
	addressItem := systray.AddMenuItem("http://"+opts.Address, "Stream address; click to open a preview")
	systray.AddSeparator()

	sourceMenu := systray.AddMenuItem("Source", "Choose the camera to bridge")
	sourceItems := make(map[string]*systray.MenuItem, len(sourceChoices))
	for _, choice := range sourceChoices {
		sourceItems[choice.kind] = sourceMenu.AddSubMenuItemCheckbox(choice.label, choice.label, false)
	}

	pauseItem := systray.AddMenuItemCheckbox("Pause", "Stop capturing and release the camera", opts.Controller.Paused())
	systray.AddSeparator()

	logItem := systray.AddMenuItem("Open log folder", "Show the log files in Explorer")
	if opts.LogDir == "" {
		logItem.Hide()
	}
	configItem := systray.AddMenuItem("Edit settings", "Open paperbridge.toml")
	if opts.ConfigPath == "" {
		configItem.Hide()
	}

	autostartItem := systray.AddMenuItemCheckbox("Start with Windows", "Launch PaperBridge at sign-in", false)
	if !autostart.Supported() {
		autostartItem.Hide()
	} else if on, err := autostart.Enabled(opts.ConfigFlag); err != nil {
		opts.Log.Warn("could not read the autostart entry", "error", err)
	} else if on {
		autostartItem.Check()
	}

	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit", "Stop PaperBridge")

	go run(ctx, opts, menu{
		status:    statusItem,
		address:   addressItem,
		sources:   sourceItems,
		pause:     pauseItem,
		logDir:    logItem,
		configure: configItem,
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
	autostart *systray.MenuItem
	quit      *systray.MenuItem
}

// run handles menu clicks and refreshes the status line until the tray exits.
func run(ctx context.Context, opts Options, m menu) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	// A click on any source entry arrives on its own channel; funnel them all
	// into one so the select below stays a fixed size.
	switches := make(chan string, len(m.sources))
	for kind, item := range m.sources {
		go func(kind string, clicked <-chan struct{}) {
			for range clicked {
				select {
				case switches <- kind:
				case <-ctx.Done():
					return
				}
			}
		}(kind, item.ClickedCh)
	}

	refresh(opts, m)
	for {
		select {
		case <-ctx.Done():
			systray.Quit()
			return

		case <-ticker.C:
			refresh(opts, m)

		case kind := <-switches:
			// Off the event loop: a source that is not attached is given up to
			// the verification timeout to prove itself, and running that here
			// would stop the menu answering at all -- including Quit, which is
			// exactly what the user reaches for when a switch is hanging.
			//
			// Nothing is refreshed on completion. The ticker redraws once a
			// second from state the bridge exposes without a lock, so the menu
			// catches up on its own whichever way the switch goes.
			go func(kind string) {
				if err := opts.Controller.Switch(ctx, kind); err != nil {
					opts.Log.Error("could not switch source", "source", kind, "error", err)
				}
			}(kind)
			refresh(opts, m)

		case <-m.pause.ClickedCh:
			// Also off the loop, and for the same reason: pausing takes the
			// bridge's lock, so a click that arrives during a slow switch
			// would block here and undo the point of the goroutine above.
			// The target state is decided here so two quick clicks cannot
			// both read the same value.
			paused := !opts.Controller.Paused()
			go func(paused bool) {
				if err := opts.Controller.SetPaused(paused); err != nil {
					opts.Log.Error("could not change the paused state", "paused", paused, "error", err)
				}
			}(paused)
			refresh(opts, m)

		case <-m.address.ClickedCh:
			openTarget("http://"+opts.Address+"/snapshot", opts)

		case <-m.logDir.ClickedCh:
			openTarget(opts.LogDir, opts)

		case <-m.configure.ClickedCh:
			openTarget(opts.ConfigPath, opts)

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

	m.status.SetTitle(statusLine(snapshot.Source, paused, snapshot.Connected, stats.InputFPS, stats.Subscribers, snapshot.LastError))
	systray.SetTooltip("PaperBridge - " + m.status.String())
}

// statusLine is the one line of text the user reads to know whether it works.
func statusLine(source string, paused, connected bool, fps float64, clients int, lastError string) string {
	if source == "" {
		source = "no source"
	}
	switch {
	case paused:
		return source + ": paused"
	case !connected && lastError != "":
		return fmt.Sprintf("%s: reconnecting (%s)", source, truncate(lastError, 60))
	case !connected:
		return source + ": connecting..."
	default:
		return fmt.Sprintf("%s: %.1f fps, %d client(s)", source, fps, clients)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
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
