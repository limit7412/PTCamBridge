// Package config loads, validates and persists the PaperBridge settings.
//
// Values are resolved in the order command line, environment
// (PAPERBRIDGE_*), TOML file, built-in default. The file lives next to the
// user's other application data and is written with defaults on first run.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// AppName is the folder name used under the user's config and data directories.
const AppName = "PaperBridge"

// FileName is the settings file written inside that folder.
const FileName = "paperbridge.toml"

// Source type identifiers accepted by source.type and by the management API.
const (
	SourceUVC    = "uvc"
	SourceSerial = "serial"
	SourceMJPEG  = "mjpeg"
)

// Config is the whole settings tree.
type Config struct {
	Server       Server       `toml:"server" json:"server"`
	Source       Source       `toml:"source" json:"source"`
	Transform    Transform    `toml:"transform" json:"transform"`
	PaperTracker PaperTracker `toml:"papertracker" json:"papertracker"`
	Log          Log          `toml:"log" json:"log"`
}

// Server configures the outward-facing MJPEG endpoint.
type Server struct {
	// Listen is the address to bind. Loopback keeps the stream off the LAN;
	// binding anywhere else also disables the management API.
	Listen string `toml:"listen" json:"listen"`
	// Boundary is the multipart delimiter. The PaperTracker client is closed
	// source and has changed parsers between releases, so this and
	// ExtraHeaders exist to adjust the wire format without a rebuild.
	Boundary string `toml:"boundary" json:"boundary"`
	// ExtraHeaders are added to every multipart part.
	ExtraHeaders map[string]string `toml:"extra_headers" json:"extra_headers"`
	// HoldOnSourceLoss keeps stream connections open while the camera is
	// reconnecting instead of closing them. The client reconnects after about
	// a second either way; holding is faster when the outage is short.
	HoldOnSourceLoss bool `toml:"hold_on_source_loss" json:"hold_on_source_loss"`
}

// Source selects and configures the active capture driver.
type Source struct {
	Type   string `toml:"type" json:"type"`
	UVC    UVC    `toml:"uvc" json:"uvc"`
	Serial Serial `toml:"serial" json:"serial"`
	MJPEG  MJPEG  `toml:"mjpeg" json:"mjpeg"`
	// MaxFrameSize bounds a single JPEG in bytes; zero uses the core default.
	MaxFrameSize int `toml:"max_frame_size" json:"max_frame_size"`
}

// UVC configures the ffmpeg-backed camera driver.
type UVC struct {
	Device     string `toml:"device" json:"device"`
	Size       string `toml:"size" json:"size"`
	Framerate  int    `toml:"framerate" json:"framerate"`
	FFmpegPath string `toml:"ffmpeg_path" json:"ffmpeg_path"`
}

// Serial configures the wired Babble board driver.
type Serial struct {
	// Port is a port name such as "COM5", or "auto" to search by vendor ID.
	Port string `toml:"port" json:"port"`
	Baud int    `toml:"baud" json:"baud"`
	// Header is the packet preamble, overridable because firmware differs.
	Header []int `toml:"header" json:"header"`
}

// MJPEG configures the upstream HTTP stream proxy.
type MJPEG struct {
	URL string `toml:"url" json:"url"`
}

// Transform is the optional geometry and re-encode step. All-zero means the
// input bytes are forwarded untouched.
type Transform struct {
	Rotate          int  `toml:"rotate" json:"rotate"`
	FlipH           bool `toml:"flip_h" json:"flip_h"`
	FlipV           bool `toml:"flip_v" json:"flip_v"`
	CropSquare      bool `toml:"crop_square" json:"crop_square"`
	ReencodeQuality int  `toml:"reencode_quality" json:"reencode_quality"`
}

// PaperTracker configures the client integration helper.
type PaperTracker struct {
	// InstallDir is the PaperTracker client folder holding wifi_cache.txt.
	InstallDir string `toml:"install_dir" json:"install_dir"`
	// WriteCache points that cache file at this bridge on startup.
	WriteCache bool `toml:"write_cache" json:"write_cache"`
}

// Log configures logging.
type Log struct {
	Level string `toml:"level" json:"level"`
	Dir   string `toml:"dir" json:"dir"`
}

// Default returns the settings written on first run.
func Default() Config {
	return Config{
		Server: Server{
			Listen:   "127.0.0.1:18080",
			Boundary: core.DefaultBoundary,
		},
		Source: Source{
			Type: SourceUVC,
			UVC: UVC{
				Size:      "240x240",
				Framerate: 30,
			},
			Serial: Serial{
				Port:   "auto",
				Baud:   3000000,
				Header: []int{0xFF, 0xA0, 0xFF, 0xA1},
			},
		},
		Log: Log{Level: "info"},
	}
}

// Dir is the per-user folder holding the settings file and the log folder.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the user config directory: %w", err)
	}
	return filepath.Join(base, AppName), nil
}

// Path is the default settings file location.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the settings file, creating it with defaults when absent, and
// then layers the environment on top. The returned config is validated.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if writeErr := Save(path, cfg); writeErr != nil {
			return cfg, fmt.Errorf("write the default settings file: %w", writeErr)
		}
	case err != nil:
		return cfg, fmt.Errorf("read %s: %w", path, err)
	default:
		if _, err := toml.Decode(string(data), &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	cfg.ApplyEnv(os.Getenv)
	cfg.Normalise()
	return cfg, cfg.Validate()
}

// ErrNotSaved marks a settings change that took effect but could not be
// written to disk, and so will be lost on the next restart. Callers wrap it so
// that the difference from "the change was rejected" survives the trip out to
// the management API and the tray.
var ErrNotSaved = errors.New("the settings are active but could not be saved")

// Save writes the settings file, creating the folder if needed. The file is
// written to a temporary name and renamed, so an interrupted write cannot
// leave a truncated settings file behind.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), FileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(cfg); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// envLookup matches os.Getenv and is injected so the mapping can be tested.
type envLookup func(string) string

// ApplyEnv overlays PAPERBRIDGE_* variables, which take precedence over the
// file. Only the settings worth scripting are exposed.
func (c *Config) ApplyEnv(get envLookup) {
	setString(get, "PAPERBRIDGE_LISTEN", &c.Server.Listen)
	setString(get, "PAPERBRIDGE_BOUNDARY", &c.Server.Boundary)
	setString(get, "PAPERBRIDGE_SOURCE_TYPE", &c.Source.Type)
	setString(get, "PAPERBRIDGE_UVC_DEVICE", &c.Source.UVC.Device)
	setString(get, "PAPERBRIDGE_UVC_SIZE", &c.Source.UVC.Size)
	setInt(get, "PAPERBRIDGE_UVC_FRAMERATE", &c.Source.UVC.Framerate)
	setString(get, "PAPERBRIDGE_FFMPEG_PATH", &c.Source.UVC.FFmpegPath)
	setString(get, "PAPERBRIDGE_SERIAL_PORT", &c.Source.Serial.Port)
	setInt(get, "PAPERBRIDGE_SERIAL_BAUD", &c.Source.Serial.Baud)
	setString(get, "PAPERBRIDGE_MJPEG_URL", &c.Source.MJPEG.URL)
	setString(get, "PAPERBRIDGE_PAPERTRACKER_DIR", &c.PaperTracker.InstallDir)
	setBool(get, "PAPERBRIDGE_WRITE_CACHE", &c.PaperTracker.WriteCache)
	setString(get, "PAPERBRIDGE_LOG_LEVEL", &c.Log.Level)
	setString(get, "PAPERBRIDGE_LOG_DIR", &c.Log.Dir)
}

func setString(get envLookup, key string, dst *string) {
	if v := get(key); v != "" {
		*dst = v
	}
}

func setInt(get envLookup, key string, dst *int) {
	if v := get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func setBool(get envLookup, key string, dst *bool) {
	if v := get(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

// Normalise fills in blanks that have an obvious answer, so validation only
// has to reject genuinely wrong values.
func (c *Config) Normalise() {
	c.Source.Type = strings.ToLower(strings.TrimSpace(c.Source.Type))
	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	c.Server.Listen = strings.TrimSpace(c.Server.Listen)
	if c.Server.Boundary == "" {
		c.Server.Boundary = core.DefaultBoundary
	}
	if c.Source.Type == "" {
		c.Source.Type = SourceUVC
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if strings.TrimSpace(c.Source.Serial.Port) == "" {
		c.Source.Serial.Port = "auto"
	}
	if c.Source.Serial.Baud <= 0 {
		c.Source.Serial.Baud = 3000000
	}
}

// Validate reports settings that would fail at runtime.
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return fmt.Errorf("server.listen %q is not a host:port address: %w", c.Server.Listen, err)
	}
	if err := core.ValidateBoundary(c.Server.Boundary); err != nil {
		return fmt.Errorf("server.boundary: %w", err)
	}
	for name, value := range c.Server.ExtraHeaders {
		if strings.ContainsAny(name, ":\r\n") || name == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("server.extra_headers contains an unusable entry %q", name)
		}
	}
	switch c.Source.Type {
	case SourceUVC, SourceSerial, SourceMJPEG:
	default:
		return fmt.Errorf("source.type %q must be one of %q, %q or %q", c.Source.Type, SourceUVC, SourceSerial, SourceMJPEG)
	}
	if c.Source.MaxFrameSize < 0 {
		return fmt.Errorf("source.max_frame_size must not be negative, got %d", c.Source.MaxFrameSize)
	}
	for i, b := range c.Source.Serial.Header {
		if b < 0 || b > 0xFF {
			return fmt.Errorf("source.serial.header[%d] = %d is not a byte value", i, b)
		}
	}
	if err := c.CoreTransform().Validate(); err != nil {
		return fmt.Errorf("transform: %w", err)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q must be debug, info, warn or error", c.Log.Level)
	}
	if c.PaperTracker.WriteCache && strings.TrimSpace(c.PaperTracker.InstallDir) == "" {
		return errors.New("papertracker.write_cache is on but papertracker.install_dir is empty")
	}
	return nil
}

// CoreTransform converts the settings into the transform the core applies.
func (c Config) CoreTransform() core.Transform {
	return core.Transform{
		Rotate:     c.Transform.Rotate,
		FlipH:      c.Transform.FlipH,
		FlipV:      c.Transform.FlipV,
		CropSquare: c.Transform.CropSquare,
		Quality:    c.Transform.ReencodeQuality,
	}
}

// SerialHeader converts the configured preamble into bytes, returning nil when
// unset so the core default applies.
func (c Config) SerialHeader() []byte {
	if len(c.Source.Serial.Header) == 0 {
		return nil
	}
	out := make([]byte, len(c.Source.Serial.Header))
	for i, b := range c.Source.Serial.Header {
		out[i] = byte(b)
	}
	return out
}

// StreamHeaders converts the configured extra part headers into core form.
func (c Config) StreamHeaders() []core.StreamHeader {
	if len(c.Server.ExtraHeaders) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Server.ExtraHeaders))
	for name := range c.Server.ExtraHeaders {
		names = append(names, name)
	}
	// Map iteration order is random; sort so the wire format is stable.
	slices.Sort(names)

	out := make([]core.StreamHeader, 0, len(names))
	for _, name := range names {
		out = append(out, core.StreamHeader{Name: name, Value: c.Server.ExtraHeaders[name]})
	}
	return out
}

// IsLoopback reports whether the listen address stays on the local machine.
// The management API is only served when it does.
func (c Config) IsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost":
		return true
	case "":
		// An empty host means every interface.
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LogDir is the folder for log files, defaulting to a logs folder beside the
// settings file.
func (c Config) LogDir() (string, error) {
	if c.Log.Dir != "" {
		return c.Log.Dir, nil
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "logs"), nil
}
