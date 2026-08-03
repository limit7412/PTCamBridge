// Command ptcambridge bridges a Baballonia-compatible mouth tracking camera to
// the PaperTracker client, re-serving it as the MJPEG-over-HTTP stream that
// client expects on loopback.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/limit7412/PTCamBridge/internal/autostart"
	"github.com/limit7412/PTCamBridge/internal/bridge"
	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/console"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/i18n"
	"github.com/limit7412/PTCamBridge/internal/logging"
	"github.com/limit7412/PTCamBridge/internal/papertracker"
	"github.com/limit7412/PTCamBridge/internal/server"
	"github.com/limit7412/PTCamBridge/internal/source"
	"github.com/limit7412/PTCamBridge/internal/status"
	"github.com/limit7412/PTCamBridge/internal/tray"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	// A -H=windowsgui build has no streams of its own; borrow the launching
	// terminal's so the command line flags can still print.
	console.Attach()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ptcambridge:", err)
		os.Exit(1)
	}
}

// options are the command line flags, which sit above the environment and the
// settings file in the precedence order.
type options struct {
	configPath   string
	listen       string
	sourceType   string
	device       string
	serialPort   string
	mjpegURL     string
	logLevel     string
	headless     bool
	console      bool
	listDevices  bool
	showVersion  bool
	autostartOn  bool
	autostartNo  bool
	restoreCache bool
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.configPath, "config", "", "settings file (default: the per-user ptcambridge.toml)")
	flag.StringVar(&o.listen, "listen", "", "override server.listen, for example 127.0.0.1:18080")
	flag.StringVar(&o.sourceType, "source", "", "override source.type: uvc, serial or mjpeg")
	flag.StringVar(&o.device, "device", "", "override source.uvc.device")
	flag.StringVar(&o.serialPort, "serial-port", "", "override source.serial.port")
	flag.StringVar(&o.mjpegURL, "mjpeg-url", "", "override source.mjpeg.url")
	flag.StringVar(&o.logLevel, "log-level", "", "override log.level: debug, info, warn or error")
	flag.BoolVar(&o.headless, "headless", false, "run without a system tray icon")
	flag.BoolVar(&o.console, "console", false, "also write logs to stderr")
	flag.BoolVar(&o.listDevices, "list-devices", false, "list capture devices and serial ports, then exit")
	flag.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	flag.BoolVar(&o.autostartOn, "install-autostart", false, "register to start at sign-in, then exit")
	flag.BoolVar(&o.autostartNo, "uninstall-autostart", false, "remove the sign-in registration, then exit")
	flag.BoolVar(&o.restoreCache, "restore-cache", false, "put the PaperTracker address back the way it was, then exit")
	flag.Parse()
	return o
}

func run() error {
	opts := parseFlags()

	if opts.showVersion {
		fmt.Println("ptcambridge", Version)
		return nil
	}
	switch {
	case opts.autostartOn:
		// The -config the user gave here is registered alongside the
		// executable, so the next sign-in starts on the same settings file.
		return autostart.Enable(opts.configPath)
	case opts.autostartNo:
		return autostart.Disable()
	}
	if opts.listDevices {
		return listDevices(opts)
	}
	if opts.restoreCache {
		// Separate from turning write_cache off, because uninstalling is the
		// case where the settings file is about to be deleted too and there
		// will never be another start to notice the change.
		return restoreCache(opts)
	}

	cfgPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		return err
	}
	// The layers are separated here rather than inside Load, because saving
	// has to start from the file alone: an environment variable or a flag is
	// for this run, and writing it back would make it permanent.
	fileCfg, err := config.LoadFile(cfgPath)
	if err != nil {
		return err
	}
	cfg := fileCfg
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return err
	}
	applyFlags(&cfg, opts)
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return err
	}

	log, closeLog, err := setupLogging(cfg, opts.console)
	if err != nil {
		return err
	}
	defer closeLog.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Binding first doubles as the single-instance guard: the listen port is
	// this application's identity, so a second copy cannot take it.
	listener, err := server.Listen(cfg.Server.Listen)
	if err != nil {
		return describeBindFailure(cfg.Server.Listen, err)
	}
	defer listener.Close()
	address := listener.Addr().String()

	log.Info("ptcambridge starting",
		"version", Version,
		"address", address,
		"source", cfg.Source.Type,
		"config", cfgPath,
	)

	encoder, err := core.NewMultipartEncoder(cfg.Server.Boundary, cfg.StreamHeaders())
	if err != nil {
		return err
	}

	frames := hub.New()
	// The tracker names the failures the tray should translate; which ones
	// those are is the drivers' business, so the answer comes from there.
	tracker := status.New(status.WithErrorKeys(source.ErrorKey))
	app := bridge.New(cfg, cfgPath, frames, tracker, log)
	// Saving starts from what the file said, not from the effective settings:
	// a -device or a PTCAMBRIDGE_* is for this run, and must not be written
	// back the first time the tray changes something unrelated. fileCfg has
	// had neither layer applied.
	app.SetPersistBase(fileCfg)

	admin := cfg.IsLoopback()
	if !admin {
		// The management API can change the source and rewrite settings, and
		// it has no authentication. Off the loopback it is not offered.
		log.Warn("listening off loopback, the management API is disabled", "address", address)
	}
	// PTCamBridge does not ship ffmpeg -- see internal/ffmpegfetch for why --
	// so on the platform where a build is published it can fetch one when the
	// user asks. Nowhere else: elsewhere ffmpeg is a package manager away, and
	// offering a Windows binary would be worse than saying nothing.
	var fetcher *ffmpegfetch.Manager
	if ffmpegfetch.Supported() {
		fetcher = ffmpegfetch.New(ffmpegfetch.Options{Lifetime: ctx, Log: log})
	}
	srv, err := server.New(server.Options{
		Hub:              frames,
		Status:           tracker,
		Encoder:          encoder,
		Logger:           log,
		Controller:       app,
		EnableAdmin:      admin,
		FFmpeg:           ffmpegOption(fetcher),
		HoldOnSourceLoss: cfg.Server.HoldOnSourceLoss,
		Version:          Version,
	})
	if err != nil {
		return err
	}
	// The stream settings exist to match whatever the PaperTracker client
	// currently parses, so a change to them has to reach the server without
	// going through a restart.
	app.SetStreamConfigurator(srv)

	switch {
	case cfg.PaperTracker.WriteCache:
		if err := papertracker.WriteCache(cfg.PaperTracker.InstallDir, connectAddress(address)); err != nil {
			// The bridge still works; the user just has to point the client at
			// it by hand.
			log.Error("could not update the PaperTracker address cache", "error", err)
		} else {
			log.Info("PaperTracker address cache updated", "dir", cfg.PaperTracker.InstallDir, "address", address)
			// Noted so it can be undone later even if the setting naming it is
			// gone by then, which is what going back to the defaults looks like.
			if err := rememberWrittenDir(cfg.PaperTracker.InstallDir); err != nil {
				log.Warn("could not record which folder was changed, so restoring may not find it", "error", err)
			}
		}

	default:
		// Turning write_cache off has to undo what turning it on did.
		// Otherwise the client keeps its cached loopback address and, once the
		// bridge is gone, connects to nothing at all -- a setting the user
		// switched off would still be in force with no way to lift it.
		//
		// The folder is searched for when the settings no longer name one.
		// Going back to the defaults usually means deleting the whole
		// [papertracker] section, which clears install_dir along with
		// write_cache -- and that is exactly when the backup still needs
		// putting back.
		restoreCacheQuietly(log, cfg.PaperTracker.InstallDir)
	}

	if err := app.Start(ctx); err != nil {
		log.Error("could not start the configured source", "error", err)
		// Keep serving anyway: the user can pick a working source from the
		// tray or the API without restarting.
	}
	defer app.Stop()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx, listener)
		// A listener that dies takes the stream endpoint with it. Cancelling
		// brings the tray or the headless wait down too, so the process exits
		// and a supervisor can restart it, rather than staying up looking
		// healthy while serving nothing. The error is still buffered for the
		// shutdown wait below to report.
		stop()
	}()

	logDir, _ := cfg.LogDir()
	if opts.headless {
		log.Info("running headless", "address", address)
		<-ctx.Done()
	} else {
		// The tray owns the main goroutine from here: it runs a native
		// message loop that is bound to this thread.
		tray.Run(ctx, tray.Options{
			Controller: app,
			Hub:        frames,
			Status:     tracker,
			Log:        log,
			Address:    address,
			LogDir:     logDir,
			ConfigPath: cfgPath,
			ConfigFlag: opts.configPath,
			FFmpeg:     trayFFmpeg(fetcher),
			Printer:    i18n.NewPrinter(cfg.Language()),
			OnQuit:     stop,
		})
	}

	stop()
	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
	case <-time.After(5 * time.Second):
		log.Warn("the HTTP server did not shut down in time")
	}

	// Quitting during a download cancels the transfer, but the goroutine still
	// has to delete the part-downloaded archive. Returning without waiting for
	// that leaves a hundred-odd megabytes in the settings folder, and nothing
	// left running to clean it up.
	if fetcher != nil {
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
		fetcher.Wait(waitCtx)
		cancelWait()
	}

	log.Info("ptcambridge stopped")
	return nil
}

// ffmpegOption and trayFFmpeg hand the fetcher over, or nothing at all.
//
// Assigning an absent one straight into the interface field would not be
// nothing: an interface holding a nil pointer is itself non-nil, so the
// endpoint would be routed and the menu entry shown, both of them calling
// through a pointer that is not there.
func ffmpegOption(m *ffmpegfetch.Manager) server.FFmpegFetcher {
	if m == nil {
		return nil
	}
	return m
}

func trayFFmpeg(m *ffmpegfetch.Manager) tray.FFmpegFetcher {
	if m == nil {
		return nil
	}
	return m
}

// resolveConfigPath falls back to the per-user location when no path is given.
func resolveConfigPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return config.Path()
}

// applyFlags overlays the command line, which outranks everything else.
func applyFlags(cfg *config.Config, o options) {
	if o.listen != "" {
		cfg.Server.Listen = o.listen
	}
	if o.sourceType != "" {
		cfg.Source.Type = o.sourceType
	}
	if o.device != "" {
		cfg.Source.UVC.Device = o.device
	}
	if o.serialPort != "" {
		cfg.Source.Serial.Port = o.serialPort
	}
	if o.mjpegURL != "" {
		cfg.Source.MJPEG.URL = o.mjpegURL
	}
	if o.logLevel != "" {
		cfg.Log.Level = o.logLevel
	}
}

func setupLogging(cfg config.Config, console bool) (*slog.Logger, io.Closer, error) {
	dir, err := cfg.LogDir()
	if err != nil {
		// Console-only logging beats none, but it only counts as logging if
		// the console is actually switched on: a tray launch does not pass
		// -console, and without this every later error goes to io.Discard.
		fmt.Fprintln(os.Stderr, "ptcambridge: logging to the console only:", err)
		dir, console = "", true
	}
	return logging.Setup(logging.Options{Dir: dir, Level: cfg.Log.Level, Console: console})
}

// connectAddress turns a listen address into one the client can dial.
//
// A wildcard bind resolves to something like "[::]:18080", which is a valid
// thing to listen on and a useless thing to connect to. The client runs on
// this machine, so loopback is the address it wants.
func connectAddress(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// describeBindFailure turns "address already in use" into an answer to the
// question the user actually has: is PTCamBridge already running?
func describeBindFailure(addr string, err error) error {
	if !errors.Is(err, syscall.EADDRINUSE) && !isAddrInUse(err) {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	if running, version := probeExistingInstance(addr); running {
		return fmt.Errorf("ptcambridge %s is already running on %s", version, addr)
	}
	return fmt.Errorf("%s is already in use by another program; set server.listen to a free port", addr)
}

// probeExistingInstance asks whoever holds the port whether they are us.
func probeExistingInstance(addr string) (bool, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/stats")
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()

	var stats server.Stats
	if err := decodeJSON(resp.Body, &stats); err != nil || stats.Version == "" {
		return false, ""
	}
	return true, stats.Version
}

func listDevices(opts options) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The settings are read here too: an installation that points at ffmpeg
	// with source.uvc.ffmpeg_path rather than bundling it would otherwise get
	// an empty list from a command whose whole job is to find the camera. The
	// language comes from the same read, falling back to the system when the
	// file cannot be had.
	var ffmpegPath string
	p := i18n.NewPrinter(i18n.Detect())
	if cfgPath, err := resolveConfigPath(opts.configPath); err != nil {
		fmt.Fprintln(os.Stderr, p.S(i18n.CLINoSettingsPath), err)
	} else if cfg, err := config.Load(cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, p.S(i18n.CLINoSettingsRead), err)
	} else {
		ffmpegPath = cfg.Source.UVC.FFmpegPath
		p = i18n.NewPrinter(cfg.Language())
	}

	cameras, err := source.ListDevices(ctx, ffmpegPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, p.S(i18n.CLINoDevices), err)
	}
	fmt.Println(p.S(i18n.CLICaptureDevices))
	if len(cameras) == 0 {
		fmt.Println(p.S(i18n.CLINoneFound))
	}
	for _, d := range cameras {
		if d.Alternative != "" {
			fmt.Printf("  %s\n    %s\n", d.Name, d.Alternative)
			continue
		}
		fmt.Printf("  %s\n", d.Name)
	}

	ports, err := source.ListSerialPorts()
	if err != nil {
		fmt.Fprintln(os.Stderr, p.S(i18n.CLINoSerialPorts), err)
	}
	fmt.Println("\n" + p.S(i18n.CLISerialPorts))
	if len(ports) == 0 {
		fmt.Println(p.S(i18n.CLINoneFound))
	}
	for _, p := range ports {
		switch {
		case p.Vendor != "":
			fmt.Printf("  %s  [%s %s:%s]\n", p.Name, p.Vendor, p.VID, p.PID)
		case p.VID != "":
			fmt.Printf("  %s  [%s:%s]\n", p.Name, p.VID, p.PID)
		default:
			fmt.Printf("  %s\n", p.Name)
		}
	}
	return nil
}

// isAddrInUse covers the Windows spelling of the error, which does not map
// onto the POSIX constant.
func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}
	var sysErr *os.SyscallError
	if !errors.As(opErr.Err, &sysErr) {
		return false
	}
	// WSAEADDRINUSE
	const wsaEAddrInUse = syscall.Errno(10048)
	return errors.Is(sysErr.Err, syscall.EADDRINUSE) || errors.Is(sysErr.Err, wsaEAddrInUse)
}

// decodeJSON is a small wrapper so the probe does not need the encoding/json
// import at the top of an otherwise wiring-only file.
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(v)
}

// restoreCacheQuietly puts the client's address back if the bridge ever
// changed it, saying nothing when there is nothing to undo.
//
// This runs on every start with write_cache off, so "no backup here" and "no
// PaperTracker here" are ordinary answers rather than failures: they describe
// a machine the bridge has not touched, and logging them as errors would cry
// wolf on every boot.
func restoreCacheQuietly(log *slog.Logger, installDir string) {
	restored, err := restoreEverywhereItWas(installDir)
	for _, dir := range restored {
		log.Info("PaperTracker address cache restored", "dir", dir)
	}
	if err != nil {
		log.Error("could not restore the PaperTracker address cache", "error", err)
	}
}

// restoreEverywhereItWas puts back every client the bridge changed, and returns
// the folders it restored. Finding nothing to do is not an error and shows up
// as an empty list.
//
// Every folder, not the first one: install_dir can be changed while
// write_cache is on -- the client is reinstalled or moved -- and the bridge
// then leaves a record in the old folder as well as the new one. Stopping at
// the first success is what leaves the other copy of the client pointing at a
// bridge that is no longer running, with nothing left to notice it.
//
// The search asks which folders hold the bridge's own record, not which ones
// hold a client, so it cannot touch an installation the bridge never changed.
func restoreEverywhereItWas(installDir string) ([]string, error) {
	found, err := papertracker.FindRestoreDirs()
	if err != nil {
		return nil, err
	}

	// The folder the settings name goes first and is tried even when the search
	// did not turn it up, so an installation somewhere unusual is still undone.
	var dirs []string
	add := func(dir string) {
		if dir != "" && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	add(installDir)
	// The bridge's own note of where it has written covers the folder that no
	// setting names any more: install_dir pointed somewhere unusual and has
	// since been cleared, which is exactly what returning to the defaults does.
	remembered, rememberErr := rememberedWrittenDirs()
	for _, dir := range remembered {
		add(dir)
	}
	for _, dir := range found {
		add(dir)
	}

	var restored []string
	// A note that could not be read is reported, not swallowed: it may have
	// held the only mention of a folder the search cannot reach.
	errs := []error{rememberErr}
	for _, dir := range dirs {
		switch err := papertracker.RestoreCache(dir); {
		case err == nil:
			restored = append(restored, dir)
		case errors.Is(err, papertracker.ErrNoBackup):
			// Nothing here to undo, which is the ordinary answer for the folder
			// the settings name on a machine the bridge wrote to elsewhere.
		default:
			errs = append(errs, err)
		}
	}
	return restored, errors.Join(errs...)
}

// rememberWrittenDir records a folder the bridge has pointed at itself, beside
// the bridge's own settings.
func rememberWrittenDir(installDir string) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	return papertracker.RememberWrittenDir(dir, installDir)
}

// rememberedWrittenDirs reads that record back.
//
// It lives with the bridge's settings, so deleting those by hand loses it --
// and with it the only way to find an installation the search does not cover.
// Running -restore-cache before removing the folder is what the flag is for.
func rememberedWrittenDirs() ([]string, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return papertracker.WrittenDirs(dir)
}

// restoreCache puts the PaperTracker client back on the address it had before
// the bridge first wrote to its cache.
//
// This exists for uninstalling. Turning write_cache off restores it on the
// next start, but someone removing PTCamBridge deletes the settings file and
// the executable together, and there is no next start to notice.
func restoreCache(opts options) error {
	// Read without the ordinary Load, which writes a default settings file when
	// there is none. This command is what somebody runs while uninstalling, so
	// putting the settings folder back on a machine they are clearing is the
	// one thing it must not do.
	p := i18n.NewPrinter(i18n.Detect())
	if cfgPath, err := resolveConfigPath(opts.configPath); err == nil {
		p = i18n.NewPrinter(config.LanguageWithoutLoading(cfgPath, os.Getenv))
	}

	restored, err := restoreEverywhereItWas(configuredInstallDir(opts))
	for _, dir := range restored {
		fmt.Println(p.F(i18n.CLIRestored, dir))
	}
	if err != nil {
		return err
	}
	if len(restored) == 0 {
		// The search covers every usual folder, so this says the bridge has not
		// written to any of them.
		fmt.Println(p.S(i18n.CLINothingToRestore))
		fmt.Println(p.S(i18n.CLIRestoreHint))
	}
	return nil
}

// configuredInstallDir is the folder the settings name, or empty when there is
// nothing to read. Restoring falls back to searching, so the flag still works
// once the settings file has been deleted, which is the situation it is for.
//
// The file is only read if it is already there. config.Load writes a default
// one when it is not, and creates %APPDATA%\PTCamBridge to hold it -- so the
// command meant to be run while uninstalling would put back the folder the
// user was in the middle of removing.
// Only that one setting is read, and it is read without validating anything
// else. A file the bridge would refuse to start on -- a misspelled key, a
// baud rate out of range, an environment variable that does not parse -- still
// names the folder perfectly well, and refusing to look would send someone
// uninstalling PTCamBridge away with "nothing was changed" while their client
// still points at it.
func configuredInstallDir(opts options) string {
	// The environment outranks the file here as it does everywhere else. A
	// folder named only by PTCAMBRIDGE_PAPERTRACKER_DIR is one the bridge has
	// been writing to, and it is very likely not among the usual places the
	// search covers -- so ignoring the variable would mean saying "nothing was
	// changed" about the one client that was.
	if dir := strings.TrimSpace(os.Getenv(config.EnvInstallDir)); dir != "" {
		return dir
	}
	cfgPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(cfgPath); err != nil {
		return ""
	}
	dir, err := config.InstallDirFromFile(cfgPath)
	if err != nil {
		// Said out loud rather than swallowed: the search below still runs, and
		// it covers the usual folders, but not one named only in a file that
		// cannot be read.
		fmt.Fprintln(os.Stderr, "ptcambridge:", err)
		return ""
	}
	return dir
}
