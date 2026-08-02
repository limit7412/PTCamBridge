package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the built-in defaults do not validate: %v", err)
	}
}

// The first run has to produce a usable settings file without the user
// writing one.
func TestLoadCreatesTheFileWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", FileName)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != Default().Server.Listen {
		t.Errorf("Listen = %q, want the default", cfg.Server.Listen)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the settings file was not created: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Source.Serial.Baud != cfg.Source.Serial.Baud {
		t.Errorf("baud did not survive the round trip: %d then %d", cfg.Source.Serial.Baud, reloaded.Source.Serial.Baud)
	}
}

func TestLoadMergesPartialFileOntoDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	body := `
[source]
type = "mjpeg"

[source.mjpeg]
url = "http://192.168.1.50/"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Source.Type != SourceMJPEG || cfg.Source.MJPEG.URL != "http://192.168.1.50/" {
		t.Errorf("the file was not applied: %+v", cfg.Source)
	}
	if cfg.Server.Listen != Default().Server.Listen {
		t.Errorf("Listen = %q, want the default to remain", cfg.Server.Listen)
	}
	if cfg.Source.Serial.Baud != 3000000 {
		t.Errorf("Baud = %d, want the default to remain", cfg.Source.Serial.Baud)
	}
}

func TestLoadReportsAParseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("this is not = valid = toml"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestApplyEnvOverridesTheFile(t *testing.T) {
	env := map[string]string{
		"PAPERBRIDGE_LISTEN":        "0.0.0.0:9000",
		"PAPERBRIDGE_SOURCE_TYPE":   "serial",
		"PAPERBRIDGE_SERIAL_BAUD":   "115200",
		"PAPERBRIDGE_WRITE_CACHE":   "true",
		"PAPERBRIDGE_LOG_LEVEL":     "debug",
		"PAPERBRIDGE_UVC_FRAMERATE": "60",
	}
	cfg := Default()
	cfg.ApplyEnv(func(k string) string { return env[k] })

	if cfg.Server.Listen != "0.0.0.0:9000" {
		t.Errorf("Listen = %q", cfg.Server.Listen)
	}
	if cfg.Source.Type != SourceSerial {
		t.Errorf("Type = %q", cfg.Source.Type)
	}
	if cfg.Source.Serial.Baud != 115200 {
		t.Errorf("Baud = %d", cfg.Source.Serial.Baud)
	}
	if cfg.Source.UVC.Framerate != 60 {
		t.Errorf("Framerate = %d", cfg.Source.UVC.Framerate)
	}
	if !cfg.PaperTracker.WriteCache {
		t.Error("WriteCache was not applied")
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Level = %q", cfg.Log.Level)
	}
}

// A malformed number in the environment should leave the file's value alone
// rather than zeroing it.
func TestApplyEnvIgnoresUnparsableValues(t *testing.T) {
	cfg := Default()
	cfg.ApplyEnv(func(k string) string {
		if k == "PAPERBRIDGE_SERIAL_BAUD" {
			return "fast"
		}
		return ""
	})
	if cfg.Source.Serial.Baud != 3000000 {
		t.Errorf("Baud = %d, want the default to survive", cfg.Source.Serial.Baud)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"bad listen", func(c *Config) { c.Server.Listen = "not-an-address" }, "server.listen"},
		{"bad boundary", func(c *Config) { c.Server.Boundary = "has space" }, "server.boundary"},
		{"bad source", func(c *Config) { c.Source.Type = "webcam" }, "source.type"},
		{"bad rotate", func(c *Config) { c.Transform.Rotate = 45 }, "transform"},
		{"bad quality", func(c *Config) { c.Transform.ReencodeQuality = 200 }, "transform"},
		{"bad level", func(c *Config) { c.Log.Level = "verbose" }, "log.level"},
		{"bad header byte", func(c *Config) { c.Source.Serial.Header = []int{0x1FF} }, "source.serial.header"},
		{"negative frame size", func(c *Config) { c.Source.MaxFrameSize = -1 }, "source.max_frame_size"},
		{"cache without dir", func(c *Config) { c.PaperTracker.WriteCache = true }, "install_dir"},
		{"header injection", func(c *Config) { c.Server.ExtraHeaders = map[string]string{"X": "a\r\nY: b"} }, "extra_headers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNormaliseFillsBlanks(t *testing.T) {
	cfg := Config{Server: Server{Listen: " 127.0.0.1:1 "}, Source: Source{Type: "  UVC  "}}
	cfg.Normalise()

	if cfg.Source.Type != SourceUVC {
		t.Errorf("Type = %q, want it lowercased and trimmed", cfg.Source.Type)
	}
	if cfg.Server.Listen != "127.0.0.1:1" {
		t.Errorf("Listen = %q, want it trimmed", cfg.Server.Listen)
	}
	if cfg.Server.Boundary != "paperbridge" {
		t.Errorf("Boundary = %q, want the default", cfg.Server.Boundary)
	}
	if cfg.Source.Serial.Port != "auto" || cfg.Source.Serial.Baud != 3000000 {
		t.Errorf("serial defaults not filled in: %+v", cfg.Source.Serial)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Level = %q, want info", cfg.Log.Level)
	}
}

// The management API is only served on loopback, so this decides whether an
// unauthenticated control surface is exposed.
func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:18080":   true,
		"localhost:18080":   true,
		"[::1]:18080":       true,
		"0.0.0.0:18080":     false,
		"192.168.1.10:8080": false,
		":18080":            false,
		"garbage":           false,
	}
	for listen, want := range cases {
		cfg := Default()
		cfg.Server.Listen = listen
		if got := cfg.IsLoopback(); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", listen, got, want)
		}
	}
}

func TestSerialHeaderConversion(t *testing.T) {
	cfg := Default()
	got := cfg.SerialHeader()
	want := []byte{0xFF, 0xA0, 0xFF, 0xA1}
	if len(got) != len(want) {
		t.Fatalf("SerialHeader() has %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SerialHeader() = % X, want % X", got, want)
		}
	}

	cfg.Source.Serial.Header = nil
	if cfg.SerialHeader() != nil {
		t.Error("an unset header should be nil so the core default applies")
	}
}

// Extra headers come from a map, so their order has to be imposed somewhere or
// the wire format would differ between runs.
func TestStreamHeadersAreSorted(t *testing.T) {
	cfg := Default()
	cfg.Server.ExtraHeaders = map[string]string{"X-Zulu": "1", "X-Alpha": "2", "X-Mike": "3"}

	headers := cfg.StreamHeaders()
	if len(headers) != 3 {
		t.Fatalf("got %d headers, want 3", len(headers))
	}
	for i := 1; i < len(headers); i++ {
		if headers[i-1].Name > headers[i].Name {
			t.Fatalf("headers are not sorted: %q before %q", headers[i-1].Name, headers[i].Name)
		}
	}
}

func TestCoreTransformMapping(t *testing.T) {
	cfg := Default()
	cfg.Transform = Transform{Rotate: 180, FlipH: true, CropSquare: true, ReencodeQuality: 70}

	tr := cfg.CoreTransform()
	if tr.Rotate != 180 || !tr.FlipH || tr.FlipV || !tr.CropSquare || tr.Quality != 70 {
		t.Errorf("CoreTransform() = %+v, does not match the settings", tr)
	}
	if Default().CoreTransform().IsNoop() != true {
		t.Error("the default transform should be a no-op so frames pass through untouched")
	}
}

func TestSaveIsAtomicAndReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	cfg := Default()
	cfg.Source.UVC.Device = "USB Camera"

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d files, want only the settings file (a temporary file was left behind)", len(entries))
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Source.UVC.Device != "USB Camera" {
		t.Errorf("Device = %q, want it to survive the round trip", reloaded.Source.UVC.Device)
	}
}

func TestValidateRejectsReservedExtraHeaders(t *testing.T) {
	cfg := Default()
	cfg.Server.ExtraHeaders = map[string]string{"content-length": "0"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an extra header that shadows Content-Length to be rejected")
	}
}
