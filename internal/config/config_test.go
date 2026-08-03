package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/i18n"
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
		"PTCAMBRIDGE_LISTEN":        "0.0.0.0:9000",
		"PTCAMBRIDGE_SOURCE_TYPE":   "serial",
		"PTCAMBRIDGE_SERIAL_BAUD":   "115200",
		"PTCAMBRIDGE_WRITE_CACHE":   "true",
		"PTCAMBRIDGE_LOG_LEVEL":     "debug",
		"PTCAMBRIDGE_UVC_FRAMERATE": "60",
	}
	cfg := Default()
	if err := cfg.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}

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

// Skipping a malformed variable starts the bridge on a setting the user did
// not choose, with nothing anywhere saying their variable was thrown away.
func TestApplyEnvRejectsUnparsableValues(t *testing.T) {
	cases := map[string]string{
		"PTCAMBRIDGE_SERIAL_BAUD":   "fast",
		"PTCAMBRIDGE_UVC_FRAMERATE": "lots",
		"PTCAMBRIDGE_WRITE_CACHE":   "maybe",
	}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			cfg := Default()
			err := cfg.ApplyEnv(func(k string) string {
				if k == key {
					return value
				}
				return ""
			})
			if err == nil {
				t.Fatalf("ApplyEnv() = nil, want an error for %s=%q", key, value)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %v, want it to name %s", err, key)
			}
		})
	}
}

// One typo must not hide the next: every variable is still attempted.
func TestApplyEnvReportsEveryBadValue(t *testing.T) {
	cfg := Default()
	err := cfg.ApplyEnv(func(k string) string {
		switch k {
		case "PTCAMBRIDGE_SERIAL_BAUD":
			return "fast"
		case "PTCAMBRIDGE_WRITE_CACHE":
			return "maybe"
		}
		return ""
	})
	if err == nil {
		t.Fatal("ApplyEnv() = nil, want an error")
	}
	for _, key := range []string{"PTCAMBRIDGE_SERIAL_BAUD", "PTCAMBRIDGE_WRITE_CACHE"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error = %v, want it to name %s", err, key)
		}
	}
}

// Load has to fail on it too, or the check above never reaches a user.
func TestLoadRejectsABadEnvironmentValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Save(path, Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("PTCAMBRIDGE_SERIAL_BAUD", "fast")

	if _, err := Load(path); err == nil {
		t.Fatal("Load() = nil, want the bad environment value rejected")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"bad listen", func(c *Config) { c.Server.Listen = "not-an-address" }, "server.listen"},
		// Port 0 is a different port on every start, so the client's cached
		// address goes stale and the bind stops being the single-instance
		// guard: a second copy takes a port of its own and rewrites the cache.
		{"ephemeral port", func(c *Config) { c.Server.Listen = "127.0.0.1:0" }, "server.listen"},
		{"ephemeral port on a wildcard bind", func(c *Config) { c.Server.Listen = ":0" }, "server.listen"},
		// net.Listen reads all of these as port 0 as well, so rejecting only the
		// literal "0" would leave the same hole open behind a different spelling.
		{"padded zero port", func(c *Config) { c.Server.Listen = "127.0.0.1:00" }, "server.listen"},
		{"signed zero port", func(c *Config) { c.Server.Listen = "127.0.0.1:+0" }, "server.listen"},
		{"missing port", func(c *Config) { c.Server.Listen = "127.0.0.1:" }, "server.listen"},
		// A service name resolves too, and the address is written into another
		// application's settings file, so it has to say the same thing there.
		{"service name", func(c *Config) { c.Server.Listen = "127.0.0.1:http" }, "server.listen"},
		{"port out of range", func(c *Config) { c.Server.Listen = "127.0.0.1:70000" }, "server.listen"},
		{"bad boundary", func(c *Config) { c.Server.Boundary = "has space" }, "server.boundary"},
		{"bad source", func(c *Config) { c.Source.Type = "webcam" }, "source.type"},
		{"bad rotate", func(c *Config) { c.Transform.Rotate = 45 }, "transform"},
		{"bad quality", func(c *Config) { c.Transform.ReencodeQuality = 200 }, "transform"},
		{"bad level", func(c *Config) { c.Log.Level = "verbose" }, "log.level"},
		{"bad header byte", func(c *Config) { c.Source.Serial.Header = []int{0x1FF} }, "source.serial.header"},
		{"negative frame size", func(c *Config) { c.Source.MaxFrameSize = -1 }, "source.max_frame_size"},
		{"negative baud", func(c *Config) { c.Source.Serial.Baud = -1 }, "source.serial.baud"},
		{"negative framerate", func(c *Config) { c.Source.UVC.Framerate = -1 }, "source.uvc.framerate"},
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

// Zero framerate hands the choice to the device, which is a real answer.
func TestValidateAcceptsAZeroFramerate(t *testing.T) {
	cfg := Default()
	cfg.Source.UVC.Framerate = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want a zero framerate accepted as the device default", err)
	}
}

// Only zero asks for the default. A negative rate has to reach Validate as
// written, or a typo comes back as a port that opens and never sends anything.
func TestNormaliseLeavesANegativeBaudForValidate(t *testing.T) {
	cfg := Default()
	cfg.Source.Serial.Baud = -1
	cfg.Normalise()

	if cfg.Source.Serial.Baud != -1 {
		t.Fatalf("Baud = %d, want the bad value left alone", cfg.Source.Serial.Baud)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() = nil, want a negative baud rejected")
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
	if cfg.Server.Boundary != "ptcambridge" {
		t.Errorf("Boundary = %q, want the default", cfg.Server.Boundary)
	}
	if cfg.Source.Serial.Port != "auto" || cfg.Source.Serial.Baud != DefaultSerialBaud {
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

// A positive max_frame_size below the smallest possible JPEG passes as "not
// negative" and then silently drops every frame, since the parsers use it as a
// hard ceiling and a source that reads happily but publishes nothing never
// looks like a failure.
func TestValidateRejectsATinyMaxFrameSize(t *testing.T) {
	for _, size := range []int{1, 2, 3} {
		cfg := Default()
		cfg.Source.MaxFrameSize = size
		if err := cfg.Validate(); err == nil {
			t.Errorf("max_frame_size = %d was accepted", size)
		}
	}
	for _, size := range []int{0, core.MinJPEGSize, 1 << 20} {
		cfg := Default()
		cfg.Source.MaxFrameSize = size
		if err := cfg.Validate(); err != nil {
			t.Errorf("max_frame_size = %d rejected: %v", size, err)
		}
	}
}

// A misspelled key would decode into nothing and leave the default in place,
// so the bridge would start on an address or a source the user did not ask for
// while their file looked accepted.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	body := "[server]\nlsiten = \"127.0.0.1:9\"\n\n[source.uvc]\ndevcie = \"cam\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write the settings file: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected misspelled keys to be rejected")
	}
	for _, want := range []string{"server.lsiten", "source.uvc.devcie"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// Strict decoding makes the shipped sample a liability if it ever drifts from
// the struct, so it is checked here rather than by whoever copies it.
func TestSampleConfigLoads(t *testing.T) {
	if _, err := Load("../../configs/ptcambridge.toml"); err != nil {
		t.Fatalf("the sample settings file does not load: %v", err)
	}
}

// What Save writes has to be what Load accepts, or the first settings change
// would leave a file the next run refuses.
func TestSavedConfigLoadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	cfg := Default()
	cfg.Server.ExtraHeaders = map[string]string{"X-Frame-Source": "ptcambridge"}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("a saved config did not load back: %v", err)
	}
}

// Saving builds on the file, so LoadFile has to report the file alone. Folding
// the environment in here would write a variable meant for one run back as a
// permanent choice.
func TestLoadFileLeavesTheEnvironmentOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	cfg := Default()
	cfg.Source.UVC.Device = "from the file"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("PTCAMBRIDGE_UVC_DEVICE", "from the environment")

	fromFile, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := fromFile.Source.UVC.Device; got != "from the file" {
		t.Errorf("LoadFile device = %q, want the file's value", got)
	}

	// Load still layers it on, which is what the running config wants.
	effective, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := effective.Source.UVC.Device; got != "from the environment" {
		t.Errorf("Load device = %q, want the environment to win", got)
	}
}

// Restoring the client's address has to work on a settings file the bridge
// itself would refuse to start on. The folder is right there in the file, and
// the alternative is telling someone uninstalling PTCamBridge that nothing was
// changed while their client still points at it.
func TestInstallDirFromFileIgnoresTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	settings := "[server]\nlsiten = 'oops'\nboundary = 'has space'\n\n" +
		"[source.serial]\nbaud = -1\n\n[papertracker]\ninstall_dir = 'C:\\PaperTracker'\n"
	if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The strict reading refuses it, which is right for starting up.
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted a file with a key that does not exist")
	}

	got, err := InstallDirFromFile(path)
	if err != nil {
		t.Fatalf("InstallDirFromFile: %v", err)
	}
	if got != `C:\PaperTracker` {
		t.Errorf("InstallDirFromFile() = %q, want the folder named in the file", got)
	}
}

// A file that is not TOML at all has no folder in it, and saying so beats
// guessing.
func TestInstallDirFromFileReportsAnUnparsableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	if err := os.WriteFile(path, []byte("[server\nlisten ="), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := InstallDirFromFile(path); err == nil {
		t.Error("expected an error for a file that cannot be parsed")
	}
}

// The generated file and Default() are two statements of the same settings, so
// they have to agree. They are kept apart because only the file carries the
// comments, and a first run that cannot start until a camera is named needs a
// file that says so -- but a drift between them would hand new users values
// nobody chose.
func TestDefaultFileMatchesDefault(t *testing.T) {
	var fromFile Config
	md, err := toml.Decode(DefaultFile(), &fromFile)
	if err != nil {
		t.Fatalf("decode the embedded default file: %v", err)
	}
	if unknown := md.Undecoded(); len(unknown) > 0 {
		t.Errorf("the embedded default file has settings that do not exist: %v", unknown)
	}
	if !reflect.DeepEqual(Default(), fromFile) {
		t.Errorf("the embedded default file does not match Default()\n file: %+v\n code: %+v", fromFile, Default())
	}
}

// It is the file a first run is left staring at, so the two things that run
// cannot proceed without have to be answered in it.
func TestDefaultFileSaysHowToNameACamera(t *testing.T) {
	for _, want := range []string{"-list-devices", "device"} {
		if !strings.Contains(DefaultFile(), want) {
			t.Errorf("the embedded default file does not mention %q", want)
		}
	}
}

// A first run must end up with the annotated file, not an encoding of the
// struct: the comments are the only thing telling the user what to do next.
func TestLoadFileCreatesTheAnnotatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(Default(), cfg) {
		t.Errorf("LoadFile returned %+v, want the defaults %+v", cfg, Default())
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the created file: %v", err)
	}
	if string(written) != DefaultFile() {
		t.Errorf("the created file is not the annotated default:\n%s", written)
	}
}

func TestLanguageSetting(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "ja_JP.UTF-8")

	// Auto follows the system, which the environment above pins to Japanese.
	cfg := Default()
	cfg.Normalise()
	if got := cfg.Language(); got != i18n.Japanese {
		t.Errorf("the default language = %q, want the system's", got)
	}

	cfg.UI.Language = "en"
	if got := cfg.Language(); got != i18n.English {
		t.Errorf("Language() = %q, want the configured English", got)
	}

	// Blank means auto, so an older settings file without the section still
	// picks up the system language rather than falling to English.
	cfg.UI.Language = ""
	cfg.Normalise()
	if cfg.UI.Language != string(i18n.Auto) {
		t.Errorf("Normalise left ui.language = %q, want auto", cfg.UI.Language)
	}
	if got := cfg.Language(); got != i18n.Japanese {
		t.Errorf("Language() = %q, want the system's for a blank setting", got)
	}
}

// A language nobody has text for is refused rather than ignored: a user who
// wrote "jp" would otherwise see an English menu and no explanation.
func TestValidateRejectsAnUnknownLanguage(t *testing.T) {
	cfg := Default()
	cfg.UI.Language = "jp"
	cfg.Normalise()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted an unknown language")
	}
	if !strings.Contains(err.Error(), "ui.language") {
		t.Errorf("error = %v, want it to name the setting", err)
	}
}

func TestLanguageFromTheEnvironment(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyEnv(func(name string) string {
		if name == "PTCAMBRIDGE_LANGUAGE" {
			return "ja"
		}
		return ""
	}); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if got := cfg.Language(); got != i18n.Japanese {
		t.Errorf("Language() = %q, want the variable to win", got)
	}
}
