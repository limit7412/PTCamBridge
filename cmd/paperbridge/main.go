// Command paperbridge bridges a Baballonia-compatible mouth tracking camera to
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
	"syscall"
	"time"

	"github.com/limit7412/PTCamBridge/internal/autostart"
	"github.com/limit7412/PTCamBridge/internal/bridge"
	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/console"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
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
		fmt.Fprintln(os.Stderr, "paperbridge:", err)
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
	flag.StringVar(&o.configPath, "config", "", "settings file (default: the per-user paperbridge.toml)")
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
		fmt.Println("paperbridge", Version)
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

	log.Info("paperbridge starting",
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
	tracker := status.New()
	app := bridge.New(cfg, cfgPath, frames, tracker, log)
	// Saving starts from what the file said, not from the effective settings:
	// a -device or a PAPERBRIDGE_* is for this run, and must not be written
	// back the first time the tray changes something unrelated. fileCfg has
	// had neither layer applied.
	app.SetPersistBase(fileCfg)

	admin := cfg.IsLoopback()
	if !admin {
		// The management API can change the source and rewrite settings, and
		// it has no authentication. Off the loopback it is not offered.
		log.Warn("listening off loopback, the management API is disabled", "address", address)
	}
	srv, err := server.New(server.Options{
		Hub:              frames,
		Status:           tracker,
		Encoder:          encoder,
		Logger:           log,
		Controller:       app,
		EnableAdmin:      admin,
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

	log.Info("paperbridge stopped")
	return nil
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
		fmt.Fprintln(os.Stderr, "paperbridge: logging to the console only:", err)
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
// question the user actually has: is PaperBridge already running?
func describeBindFailure(addr string, err error) error {
	if !errors.Is(err, syscall.EADDRINUSE) && !isAddrInUse(err) {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	if running, version := probeExistingInstance(addr); running {
		return fmt.Errorf("paperbridge %s is already running on %s", version, addr)
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
	// an empty list from a command whose whole job is to find the camera.
	var ffmpegPath string
	if cfgPath, err := resolveConfigPath(opts.configPath); err != nil {
		fmt.Fprintln(os.Stderr, "could not locate the settings file:", err)
	} else if cfg, err := config.Load(cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "could not read the settings file:", err)
	} else {
		ffmpegPath = cfg.Source.UVC.FFmpegPath
	}

	cameras, err := source.ListDevices(ctx, ffmpegPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not list capture devices:", err)
	}
	fmt.Println("Capture devices:")
	if len(cameras) == 0 {
		fmt.Println("  (none found)")
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
		fmt.Fprintln(os.Stderr, "could not list serial ports:", err)
	}
	fmt.Println("\nSerial ports:")
	if len(ports) == 0 {
		fmt.Println("  (none found)")
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
	dir := installDir
	if dir == "" {
		// The folder holding the bridge's own backup, not merely the first one
		// that looks like PaperTracker: restoring into the wrong copy of the
		// client would report nothing to undo and leave the one that was really
		// changed pointing at a bridge that is no longer running.
		found, err := papertracker.FindRestoreDir()
		if err != nil {
			return
		}
		dir = found
	}
	switch err := papertracker.RestoreCache(dir); {
	case err == nil:
		log.Info("PaperTracker address cache restored", "dir", dir)
	case errors.Is(err, papertracker.ErrNoBackup):
	default:
		log.Error("could not restore the PaperTracker address cache", "dir", dir, "error", err)
	}
}

// restoreCache puts the PaperTracker client back on the address it had before
// the bridge first wrote to its cache.
//
// This exists for uninstalling. Turning write_cache off restores it on the
// next start, but someone removing PaperBridge deletes the settings file and
// the executable together, and there is no next start to notice.
func restoreCache(opts options) error {
	dir, err := restoreDir(opts)
	if err != nil {
		if errors.Is(err, papertracker.ErrNoBackup) {
			// The search covers every usual folder, so this says the bridge has
			// not written to any of them -- the same answer as finding the
			// folder and finding no backup in it.
			fmt.Println("Nothing to restore: PaperBridge has not changed the PaperTracker address cache.")
			fmt.Println("If the client is installed somewhere unusual, set papertracker.install_dir or pass -config.")
			return nil
		}
		return err
	}
	if err := papertracker.RestoreCache(dir); err != nil {
		if errors.Is(err, papertracker.ErrNoBackup) {
			fmt.Println("Nothing to restore: PaperBridge has not changed the PaperTracker address cache.")
			return nil
		}
		return err
	}
	fmt.Println("PaperTracker address cache restored in", dir)
	return nil
}

// restoreDir finds the client folder, preferring what the settings say. The
// search is the fallback so the flag still works once the settings file has
// been deleted, which is the situation it is for.
//
// The file is only read if it is already there. config.Load writes a default
// one when it is not, and creates %APPDATA%\PaperBridge to hold it -- so the
// command meant to be run while uninstalling would put back the folder the
// user was in the middle of removing.
func restoreDir(opts options) (string, error) {
	if cfgPath, err := resolveConfigPath(opts.configPath); err == nil {
		if _, err := os.Stat(cfgPath); err == nil {
			if cfg, err := config.Load(cfgPath); err == nil && cfg.PaperTracker.InstallDir != "" {
				return cfg.PaperTracker.InstallDir, nil
			}
		}
	}
	// The search asks which folder the bridge wrote to, not which folder holds
	// a client. Several installations can sit on one machine, and only one of
	// them has the address that needs putting back.
	dir, err := papertracker.FindRestoreDir()
	if err != nil {
		return "", err
	}
	return dir, nil
}
