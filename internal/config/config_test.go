package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// 初回起動は、ユーザーが何も書かなくても使える設定ファイルを生み出さなければ
// ならない。
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
	set, err := cfg.ApplyEnv(func(k string) string { return env[k] })
	if err != nil {
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

	want := []string{
		"server.listen",
		"source.type",
		"source.uvc.framerate",
		"source.serial.baud",
		"papertracker.write_cache",
		"log.level",
	}
	slices.Sort(set)
	slices.Sort(want)
	if !slices.Equal(set, want) {
		t.Errorf("ApplyEnv named %v, want %v", set, want)
	}
}

// 上書きされた葉の名前は、値が変わったかどうかではなく、指定されたかどうかを
// 答えなければなりません。ファイルと同じ値を指定した上書きも上書きです。ファイルを
// 書き換えても、次の起動ではやはり環境変数が勝ちます。
func TestApplyEnvNamesOverridesThatMatchTheFile(t *testing.T) {
	cfg := Default()
	cfg.UI.Language = "en"

	set, err := cfg.ApplyEnv(func(k string) string {
		if k == EnvLanguage {
			return "en" // ファイルと同じ値
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if !slices.Contains(set, "ui.language") {
		t.Errorf("ApplyEnv named %v, want ui.language; the override is there even though nothing moved", set)
	}
}

// 設定画面は設定を JSON で読み書きするので、2^53 を超える整数は読んだ時点で
// 別の値になります。黙って丸めた値がファイルへ書き戻されるより、受け取らない方が
// ユーザーに分かります。
func TestValidateRejectsWholeNumbersJSONCannotHold(t *testing.T) {
	const tooBig = 1<<53 + 1

	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"source.max_frame_size", func(c *Config) { c.Source.MaxFrameSize = tooBig }},
		{"source.uvc.framerate", func(c *Config) { c.Source.UVC.Framerate = tooBig }},
		{"source.serial.baud", func(c *Config) { c.Source.Serial.Baud = tooBig }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Normalise()
			tc.change(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want %s = %d rejected", tc.name, tooBig)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error = %v, want it to name %s", err, tc.name)
			}

			// 境目のちょうど内側は通る。狭めているのは表現できない範囲だけで、
			// 意味のある大きさをこちらの見立てで決めてはいない。
			tc.change(&cfg)
			cfg.Source.MaxFrameSize = int(maxExactJSONInt)
			cfg.Source.UVC.Framerate = int(maxExactJSONInt)
			cfg.Source.Serial.Baud = int(maxExactJSONInt)
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() = %v, want the largest exactly representable value accepted", err)
			}
		})
	}
}

// 解釈できなかった変数は上書きとして数えません。値が設定に入っていない以上、
// 数えると効いていない指定を「効いている」と言うことになります。
func TestApplyEnvDoesNotNameValuesItCouldNotRead(t *testing.T) {
	cfg := Default()
	set, err := cfg.ApplyEnv(func(k string) string {
		if k == "PTCAMBRIDGE_SERIAL_BAUD" {
			return "fast"
		}
		return ""
	})
	if err == nil {
		t.Fatal("ApplyEnv() = nil, want an error")
	}
	if slices.Contains(set, "source.serial.baud") {
		t.Errorf("ApplyEnv named %v, want source.serial.baud left out; the value never landed", set)
	}
}

// 壊れた変数を飛ばすと、ユーザーが選んでいない設定でブリッジが起動する。しかも
// その変数が捨てられたことは、どこにも書かれない。
func TestApplyEnvRejectsUnparsableValues(t *testing.T) {
	cases := map[string]string{
		"PTCAMBRIDGE_SERIAL_BAUD":   "fast",
		"PTCAMBRIDGE_UVC_FRAMERATE": "lots",
		"PTCAMBRIDGE_WRITE_CACHE":   "maybe",
	}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			cfg := Default()
			_, err := cfg.ApplyEnv(func(k string) string {
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

// 1 つの打ち間違いが次を隠してはいけない。すべての変数を試す。
func TestApplyEnvReportsEveryBadValue(t *testing.T) {
	cfg := Default()
	_, err := cfg.ApplyEnv(func(k string) string {
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

// Load も失敗しなければならない。さもないと上の検査はユーザーまで届かない。
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
		// ポート 0 は起動のたびに別のポートになるので、クライアントがキャッシュ
		// したアドレスは古くなり、bind は単一起動の番人でなくなる。2 つ目の実体は
		// 自分のポートを取り、キャッシュを書き換える。
		{"ephemeral port", func(c *Config) { c.Server.Listen = "127.0.0.1:0" }, "server.listen"},
		{"ephemeral port on a wildcard bind", func(c *Config) { c.Server.Listen = ":0" }, "server.listen"},
		// net.Listen はこれらもすべてポート 0 として読むので、リテラルの "0" だけを
		// 拒否しても、同じ穴が別の綴りの陰に開いたまま残る。
		{"padded zero port", func(c *Config) { c.Server.Listen = "127.0.0.1:00" }, "server.listen"},
		{"signed zero port", func(c *Config) { c.Server.Listen = "127.0.0.1:+0" }, "server.listen"},
		{"missing port", func(c *Config) { c.Server.Listen = "127.0.0.1:" }, "server.listen"},
		// サービス名も解決される。このアドレスは別のアプリケーションの設定ファイルに
		// 書き込まれるので、あちらでも同じことを言うものでなければならない。
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

// framerate が 0 なら選択をデバイスに委ねるということであり、それは実のある答え。
func TestValidateAcceptsAZeroFramerate(t *testing.T) {
	cfg := Default()
	cfg.Source.UVC.Framerate = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want a zero framerate accepted as the device default", err)
	}
}

// 既定値を求めているのは 0 だけ。負の速度は書かれたまま Validate まで届かなければ
// ならない。さもないと打ち間違いは「開くのに何も送らないポート」として返ってくる。
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

// 管理 API を提供するのはループバックのときだけなので、これは「認証の無い操作面を
// 晒すかどうか」を決めている。
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

// 追加ヘッダーは map から来るので、どこかで順序を与えないとワイヤ形式が実行ごとに
// 変わってしまう。
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

// あり得る最小の JPEG より小さい正の max_frame_size は「負ではない」として通り、
// その後すべてのフレームを黙って捨てる。パーサーはこれを絶対的な上限として使うし、
// 順調に読んでいるのに何も配信しないソースは、決して失敗のようには見えない。
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

// 綴りを誤ったキーは何にもデコードされず既定値が残るので、ファイルは受理された
// ように見えるのに、ブリッジはユーザーが求めていないアドレスやソースで起動する。
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

// 厳格なデコードのもとでは、同梱の見本が構造体からずれた瞬間にそれは負債になる。
// だから、それを写した人にではなく、ここで確認する。
//
// 見るのは埋め込みの default.toml。初回起動が書き出すのがこれで、ユーザーが
// 実際に手にする唯一の見本だから。以前は configs/ 以下にもう 1 部あったが、
// それは誰も読まないまま [ui] を落としてずれていた。見本が 2 つあれば、
// いずれ片方だけが古くなる。
func TestSampleConfigLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(DefaultFile()), 0o644); err != nil {
		t.Fatalf("write the sample: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("the sample settings file does not load: %v", err)
	}
}

// Save が書くものは Load が受け入れるものでなければならない。さもないと最初の
// 設定変更が、次回の起動が拒否するファイルを残すことになる。
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

// 保存はファイルを土台にするので、LoadFile はファイルだけを報告しなければならない。
// ここで環境変数を畳み込むと、一度きりの実行のための変数が恒久的な選択として
// 書き戻される。
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

	// Load はそれを重ねる。動作中の設定が欲しいのはそちら。
	effective, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := effective.Source.UVC.Device; got != "from the environment" {
		t.Errorf("Load device = %q, want the environment to win", got)
	}
}

// クライアントのアドレスを元に戻す処理は、ブリッジ自身が起動を拒否するような設定
// ファイルの上でも動かなければならない。フォルダはファイルの中にちゃんと書かれて
// いるし、代わりに起きるのは、PTCamBridge を撤去している人に対して、クライアントが
// まだそれを指したまま「何も変更していない」と伝えることだ。
func TestInstallDirFromFileIgnoresTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	settings := "[server]\nlsiten = 'oops'\nboundary = 'has space'\n\n" +
		"[source.serial]\nbaud = -1\n\n[papertracker]\ninstall_dir = 'C:\\PaperTracker'\n"
	if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 厳格な読み取りはこれを拒否する。起動時の判断としてはそれで正しい。
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

// そもそも TOML ですらないファイルにフォルダは入っていない。推測するよりそう言う
// 方がましだ。
func TestInstallDirFromFileReportsAnUnparsableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	if err := os.WriteFile(path, []byte("[server\nlisten ="), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := InstallDirFromFile(path); err == nil {
		t.Error("expected an error for a file that cannot be parsed")
	}
}

// 生成されるファイルと Default() は同じ設定についての 2 つの言明なので、一致して
// いなければならない。分けてあるのはコメントを持つのがファイルだけだからで、カメラが
// 指定されるまで始まらない初回起動には、そう告げるファイルが要る。ただし両者がずれれば、
// 新しいユーザーは誰も選んでいない値を手にすることになる。
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

// 既定はカメラのモードを決めない。決めてしまうと、それを持っていないカメラは
// 一切開かない。DirectShow は要求したモードが無いと入力を開けないので、汎用の
// webcam を挿した人が「カメラを選んだだけ」で、再試行を繰り返すだけの起動を
// 手にする。決めなければカメラが自分の既定モードを選び、それは必ず存在する。
func TestDefaultDoesNotPinACameraMode(t *testing.T) {
	cfg := Default()
	if cfg.Source.UVC.Size != "" {
		t.Errorf("Default().Source.UVC.Size = %q, want it left to the camera", cfg.Source.UVC.Size)
	}
	if cfg.Source.UVC.Framerate != 0 {
		t.Errorf("Default().Source.UVC.Framerate = %d, want it left to the camera", cfg.Source.UVC.Framerate)
	}

	// Normalise がここを埋め戻してもいけない。埋めれば既定を決めたのと同じ。
	cfg.Normalise()
	if cfg.Source.UVC.Size != "" || cfg.Source.UVC.Framerate != 0 {
		t.Errorf("Normalise filled in a camera mode: size=%q framerate=%d",
			cfg.Source.UVC.Size, cfg.Source.UVC.Framerate)
	}
}

// これは初回起動が見つめることになるファイルなので、その起動が先へ進むために
// 欠かせない 2 つの事柄は、この中で答えられていなければならない。
func TestDefaultFileSaysHowToNameACamera(t *testing.T) {
	for _, want := range []string{"-list-devices", "device"} {
		if !strings.Contains(DefaultFile(), want) {
			t.Errorf("the embedded default file does not mention %q", want)
		}
	}
}

// 初回起動が手にするのは、構造体を符号化したものではなく注釈付きのファイルで
// なければならない。次に何をすべきかをユーザーに伝えるのはコメントだけだから。
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

	// auto はシステムに従い、上の環境変数によりそれは日本語に固定される。
	cfg := Default()
	cfg.Normalise()
	if got := cfg.Language(); got != i18n.Japanese {
		t.Errorf("the default language = %q, want the system's", got)
	}

	cfg.UI.Language = "en"
	if got := cfg.Language(); got != i18n.English {
		t.Errorf("Language() = %q, want the configured English", got)
	}

	// 空は auto を意味するので、このセクションを持たない古い設定ファイルでも
	// 英語に落ちずシステムの言語を拾う。
	cfg.UI.Language = ""
	cfg.Normalise()
	if cfg.UI.Language != string(i18n.Auto) {
		t.Errorf("Normalise left ui.language = %q, want auto", cfg.UI.Language)
	}
	if got := cfg.Language(); got != i18n.Japanese {
		t.Errorf("Language() = %q, want the system's for a blank setting", got)
	}
}

// 誰もテキストを持たない言語は、無視せず拒否する。そうしないと "jp" と書いた
// ユーザーは、説明の無いまま英語のメニューを見ることになる。
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
	if _, err := cfg.ApplyEnv(func(name string) string {
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

// -restore-cache はアンインストールの最中に走らせるものなので、言語を読むことが、
// 片付けている機械に設定フォルダを戻すことになってはいけない。
func TestLanguageWithoutLoadingCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone", FileName)

	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "ja_JP.UTF-8")

	if got := LanguageWithoutLoading(path, nil); got != i18n.Japanese {
		t.Errorf("LanguageWithoutLoading of a missing file = %q, want the system's", got)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("the settings folder was created (stat error %v)", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the settings file was created (stat error %v)", err)
	}
}

// 読むのは 1 つの設定だけなので、ブリッジ自身が起動を拒否するファイルからでも
// 答えは得られる。
func TestLanguageWithoutLoadingIgnoresTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("[ui]\nlanguage = 'ja'\n\n[server]\nlsiten = 'typo'\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := LanguageWithoutLoading(path, nil); got != i18n.Japanese {
		t.Errorf("LanguageWithoutLoading = %q, want Japanese despite the unknown key", got)
	}
}

// 文書化された順序は環境変数、ファイル、システムであり、通常の層を通らずに言語を
// 読むコマンドでもそれは成り立たなければならない。ここ以外のどこでも効く変数は、
// 用意していないより悪い。
func TestLanguageWithoutLoadingPrefersTheEnvironment(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "en_US.UTF-8")

	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("[ui]\nlanguage = 'en'\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	env := func(name string) string {
		if name == EnvLanguage {
			return "ja"
		}
		return ""
	}
	if got := LanguageWithoutLoading(path, env); got != i18n.Japanese {
		t.Errorf("LanguageWithoutLoading = %q, want the variable to beat the file", got)
	}

	// 空の変数は選択ではないので、決めるのは引き続きファイル。
	blank := func(string) string { return "" }
	if got := LanguageWithoutLoading(path, blank); got != i18n.English {
		t.Errorf("LanguageWithoutLoading = %q, want the file's English", got)
	}

	// 使えない値も同じ。失敗させずに次へ落とす。これは設定が既に悪い状態にある
	// ときに走るものだから。
	bad := func(name string) string {
		if name == EnvLanguage {
			return "jp"
		}
		return ""
	}
	if got := LanguageWithoutLoading(path, bad); got != i18n.English {
		t.Errorf("LanguageWithoutLoading = %q, want it to fall through to the file", got)
	}
}
