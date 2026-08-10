// Package config は、PTCamBridge の設定を読み込み、検証し、保存します。
//
// 値はコマンドライン、環境変数 (PTCAMBRIDGE_*)、TOML ファイル、組み込みの既定値の
// 順に解決します。ファイルはユーザーの他のアプリケーションデータと同じ場所に置かれ、
// 初回起動時に既定値で書き出されます。
package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/i18n"
)

// defaultFile は、初回起動時に置かれる設定ファイルです。値は Default() と同じで、
// それぞれが何のためのものかを説明するコメントが付いています。構造体から組み立てず
// ファイルとして持っているのは、コメントこそが要点だからです。両者がずれないよう、
// テストで Default() に縛り付けています。
//
//go:embed default.toml
var defaultFile string

// DefaultFile は、初回起動時に書き出される注釈付きの設定ファイルを返します。
func DefaultFile() string { return defaultFile }

// AppName は、ユーザーの設定ディレクトリとデータディレクトリの下で使うフォルダ名です。
const AppName = "PTCamBridge"

// FileName は、そのフォルダ内に書く設定ファイルの名前です。
const FileName = "ptcambridge.toml"

// DefaultSerialBaud は Babble の有線ファームウェアが動作する速度で、
// source.serial.baud が 0 のときに使われます。
const DefaultSerialBaud = 3000000

// source.type と管理 API が受け付けるソース種別の識別子。
const (
	SourceUVC    = "uvc"
	SourceSerial = "serial"
	SourceMJPEG  = "mjpeg"
)

// Config は設定ツリー全体です。
type Config struct {
	Server       Server       `toml:"server" json:"server"`
	Source       Source       `toml:"source" json:"source"`
	Output       Output       `toml:"output" json:"output"`
	Transform    Transform    `toml:"transform" json:"transform"`
	UI           UI           `toml:"ui" json:"ui"`
	PaperTracker PaperTracker `toml:"papertracker" json:"papertracker"`
	Log          Log          `toml:"log" json:"log"`
}

// Server は、外向きの MJPEG エンドポイントを設定します。
type Server struct {
	// Listen は bind するアドレスです。ループバックならストリームは LAN に出ません。
	// それ以外に bind した場合は管理 API も無効になります。
	Listen string `toml:"listen" json:"listen"`
	// Boundary は multipart の区切りです。PaperTracker クライアントが受け取る形は
	// 版によって違い得るので、これと ExtraHeaders は再ビルド無しにワイヤ形式を
	// 調整するために存在します。
	Boundary string `toml:"boundary" json:"boundary"`
	// ExtraHeaders は、multipart の各パートに追加されます。
	ExtraHeaders map[string]string `toml:"extra_headers" json:"extra_headers"`
	// HoldOnSourceLoss は、カメラの再接続中にストリームの接続を閉じず開いたまま
	// 保ちます。どちらにせよクライアントは 1 秒ほどで再接続しますが、断絶が短い
	// 場合は保った方が速く復帰します。
	HoldOnSourceLoss bool `toml:"hold_on_source_loss" json:"hold_on_source_loss"`
}

// Source は、稼働させるキャプチャドライバを選び、設定します。
type Source struct {
	Type   string `toml:"type" json:"type"`
	UVC    UVC    `toml:"uvc" json:"uvc"`
	Serial Serial `toml:"serial" json:"serial"`
	MJPEG  MJPEG  `toml:"mjpeg" json:"mjpeg"`
	// MaxFrameSize は JPEG 1 枚の上限をバイトで指定します。0 なら core の既定値です。
	MaxFrameSize int `toml:"max_frame_size" json:"max_frame_size"`
}

// UVC は、ffmpeg を後ろに置いたカメラドライバを設定します。
type UVC struct {
	Device     string `toml:"device" json:"device"`
	Size       string `toml:"size" json:"size"`
	Framerate  int    `toml:"framerate" json:"framerate"`
	FFmpegPath string `toml:"ffmpeg_path" json:"ffmpeg_path"`
}

// Serial は、有線 Babble ボードのドライバを設定します。
type Serial struct {
	// Port は "COM5" のようなポート名、またはベンダー ID で探させる "auto" です。
	Port string `toml:"port" json:"port"`
	Baud int    `toml:"baud" json:"baud"`
	// Header はパケットの前置きです。ファームウェアによって異なるため上書きできます。
	Header []int `toml:"header" json:"header"`
}

// MJPEG は、上流 HTTP ストリームの中継を設定します。
type MJPEG struct {
	URL string `toml:"url" json:"url"`
}

// Output は、HTTP ストリーム以外の配信先の設定です。既定ではどれも無効で、
// フレームが出ていく先は今までどおり MJPEG-over-HTTP だけになります。
type Output struct {
	Serial OutputSerial `toml:"serial" json:"serial"`
}

// OutputSerial は、同じフレームを ETVR のパケットとしてシリアルポートへ書き出す
// 設定です。有線トラッカーしか受け付けないクライアントのための出口です。
type OutputSerial struct {
	Enabled bool `toml:"enabled" json:"enabled"`
	// Port は "COM7" のような書き込み先のポート名です。[source.serial] と違って
	// "auto" は受け付けません。理由は internal/output を参照してください。
	Port string `toml:"port" json:"port"`
	Baud int    `toml:"baud" json:"baud"`
	// Header はパケットの前置きです。読む側と同じ理由で上書きできます。
	Header []int `toml:"header" json:"header"`
}

// Transform は、任意の幾何変換と再エンコードの設定です。すべて 0 なら入力バイトを
// そのまま流します。
type Transform struct {
	Rotate          int  `toml:"rotate" json:"rotate"`
	FlipH           bool `toml:"flip_h" json:"flip_h"`
	FlipV           bool `toml:"flip_v" json:"flip_v"`
	CropSquare      bool `toml:"crop_square" json:"crop_square"`
	ReencodeQuality int  `toml:"reencode_quality" json:"reencode_quality"`
}

// PaperTracker は、クライアント連携の補助機能を設定します。
type PaperTracker struct {
	// InstallDir は、wifi_cache.txt を持つ PaperTracker クライアントのフォルダです。
	InstallDir string `toml:"install_dir" json:"install_dir"`
	// WriteCache は、起動時にそのキャッシュファイルをこのブリッジへ向けます。
	WriteCache bool `toml:"write_cache" json:"write_cache"`
}

// UI は、人が読む部分の設定です。
type UI struct {
	// Language は "auto"・"en"・"ja" のいずれかです。auto は OS に従います。
	Language string `toml:"language" json:"language"`
}

// Log はログの設定です。
type Log struct {
	Level string `toml:"level" json:"level"`
	Dir   string `toml:"dir" json:"dir"`
}

// Default は、初回起動時に書き出される設定を返します。
func Default() Config {
	return Config{
		Server: Server{
			Listen:   "127.0.0.1:18080",
			Boundary: core.DefaultBoundary,
		},
		Source: Source{
			Type: SourceUVC,
			// UVC の解像度とフレームレートは空 (デバイス既定値) です。値を決めて
			// しまうと、それを持っていないカメラは一切開きません。DirectShow は
			// 要求したモードが無いと入力を開けないので、汎用の webcam を挿した人が
			// 「カメラを選んだだけ」で理由の分からない失敗を受け取ります。決めない
			// 方を既定にして、特定のボード向けの値は明示指定にします。
			UVC: UVC{},
			Serial: Serial{
				Port:   "auto",
				Baud:   DefaultSerialBaud,
				Header: []int{0xFF, 0xA0, 0xFF, 0xA1},
			},
		},
		// 書き込み先のポート名は空です。既定を持てません — このアプリケーションが
		// 作れるポートは 1 つも無く、どの COM 番号が空いているかも、そこに何が
		// 繋がっているかも分からないからです。有効化する人が名前を書きます。
		Output: Output{Serial: OutputSerial{
			Baud:   DefaultSerialBaud,
			Header: []int{0xFF, 0xA0, 0xFF, 0xA1},
		}},
		UI:  UI{Language: string(i18n.Auto)},
		Log: Log{Level: "info"},
	}
}

// Language は、設定された画面の言語を解決し、駄目ならシステムの設定に従います。
// 未知の値は Validate が既に弾いているので、ここで失敗するのはゼロ値の場合だけで、
// それは Auto を意味します。
func (c Config) Language() i18n.Lang {
	lang, err := i18n.ParseLang(c.UI.Language)
	if err != nil {
		return i18n.Detect()
	}
	return lang
}

// Dir は、設定ファイルとログフォルダを収めるユーザーごとのフォルダです。
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the user config directory: %w", err)
	}
	return filepath.Join(base, AppName), nil
}

// Path は、設定ファイルの既定の場所です。
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load は設定ファイルを読み、その上に環境変数を重ねます。返す設定は検証済みです。
func Load(path string) (Config, error) {
	cfg, err := LoadFile(path)
	if err != nil {
		return cfg, err
	}
	if _, err := cfg.ApplyEnv(os.Getenv); err != nil {
		return cfg, err
	}
	cfg.Normalise()
	return cfg, cfg.Validate()
}

// LoadFile は設定ファイルだけを読み、無ければ既定値で作成します。環境変数を
// 適用しないのは意図的です。これはファイルのありのままの姿であり、一度きりの実行の
// ために指定した PTCAMBRIDGE_* を恒久的な選択として書き戻さないためには、保存は
// これを土台にしなければなりません。
func LoadFile(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// cfg を符号化したものではなく、注釈付きのテンプレートを置く。両者は同じ
		// ことを述べており (テストがそう縛っている)、しかし空のフィールドに何を
		// 書けばよいかを読み手に伝えるのは片方だけ。そしてこれは、トレイの
		// 「設定を編集」が開くファイルでもある。初回起動はカメラが指定されない限り
		// 始まらないのだから、現れるファイルはそう告げるものでなければならない。
		if writeErr := writeAtomic(path, []byte(defaultFile)); writeErr != nil {
			return cfg, fmt.Errorf("write the default settings file: %w", writeErr)
		}
	case err != nil:
		return cfg, fmt.Errorf("read %s: %w", path, err)
	default:
		md, err := toml.Decode(string(data), &cfg)
		if err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
		// そうしないと、綴りを誤ったキーは何にもデコードされず既定値が残るので、
		// ファイルは受理されたように見えるのに、ブリッジはユーザーが求めていない
		// アドレスやソースで起動することになる。
		if unknown := md.Undecoded(); len(unknown) > 0 {
			names := make([]string, 0, len(unknown))
			for _, key := range unknown {
				names = append(names, key.String())
			}
			return cfg, fmt.Errorf("%s has settings that do not exist: %s", path, strings.Join(names, ", "))
		}
	}

	cfg.Normalise()
	return cfg, nil
}

// EnvInstallDir は papertracker.install_dir を上書きします。
//
// 公開しているのは、クライアントのアドレスを元に戻す処理が、通常の層を通らずに
// この設定だけを単独で読むからです。そちらも同じ上書きを尊重しなければなりません。
// この変数だけで指定されたフォルダも、ブリッジが書き込むフォルダである以上、
// ブリッジが元に戻せなければならないフォルダだからです。
const EnvInstallDir = "PTCAMBRIDGE_PAPERTRACKER_DIR"

// InstallDirFromFile は papertracker.install_dir だけを読み、他は読みません。
//
// クライアントのアドレスを元に戻す処理は、ブリッジが起動を拒否するような設定
// ファイルの上でも動かなければなりません。上の厳格な読み取りは、綴りを誤ったキーや
// 範囲外の値がどこかに 1 つでもあればファイルを拒否しますが、他人のアプリケーションに
// 対してブリッジがしたことを取り消す場面は、それを言い張る場所ではありません。
// フォルダはファイルの中にちゃんと書かれているのですし、代わりに起きるのは、
// 撤去しようとしているブリッジをクライアントがまだ指したまま、「何も変更していない」と
// ユーザーに伝えることです。
//
// まったく解析できないファイルはやはりエラーです。そこから読み取れるフォルダは
// 存在せず、推測するよりそう言う方がましです。
func InstallDirFromFile(path string) (string, error) {
	var doc struct {
		PaperTracker struct {
			InstallDir string `toml:"install_dir"`
		} `toml:"papertracker"`
	}
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return "", fmt.Errorf("read papertracker.install_dir from %s: %w", path, err)
	}
	return strings.TrimSpace(doc.PaperTracker.InstallDir), nil
}

// EnvLanguage は、他のすべての PTCAMBRIDGE_* と同様に ui.language を上書きします。
//
// 公開しているのは、通常の層を通らずに言語を読む処理も同じ順序を守らなければならず、
// 名前の写しがここと ApplyEnv の 2 箇所にあれば、いずれずれるからです。
const EnvLanguage = "PTCAMBRIDGE_LANGUAGE"

// LanguageWithoutLoading は、何も作らず、設定の残りを検証もせずに画面の言語を
// 解決します。
//
// InstallDirFromFile と同じ理屈です。-restore-cache はアンインストールの最中に
// 走らせるもので、そのとき設定は半分消えているか丸ごと無いかもしれません。通常の
// 読み取りは、ユーザーが片付けている機械に新しい設定ファイルとそのフォルダを
// 書き戻してしまいます。
//
// 順序は文書化されたとおり — 環境変数、ファイル、システム — です。他のすべての
// コマンドで効く変数がこのコマンドだけ黙って効かないのは、変数を用意していないより
// 悪いからです。読めない値や未知の値は失敗させずに次へ落とします。これは設定が
// 悪い状態にあるときに走ることこそが存在理由のコマンドです。
func LanguageWithoutLoading(path string, getenv func(string) string) i18n.Lang {
	if getenv != nil {
		if lang, err := i18n.ParseLang(getenv(EnvLanguage)); err == nil && strings.TrimSpace(getenv(EnvLanguage)) != "" {
			return lang
		}
	}

	var doc struct {
		UI struct {
			Language string `toml:"language"`
		} `toml:"ui"`
	}
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return i18n.Detect()
	}
	lang, err := i18n.ParseLang(doc.UI.Language)
	if err != nil {
		return i18n.Detect()
	}
	return lang
}

// ErrNotSaved は、反映はされたがディスクに書けなかった設定変更を表します。次の
// 再起動で失われます。呼び出し側はこれを包んで返すので、「変更が拒否された」場合との
// 違いが、管理 API とトレイまで届く間に失われません。
var ErrNotSaved = errors.New("the settings are active but could not be saved")

// ErrRevisionMismatch は、変更が土台にした設定が、もう動作中のものではないことを
// 表します。
//
// 設定は全体で 1 つの値として受け渡されるので、呼び出し側は必ず「読んで、変えたい
// 葉を重ねて、書く」という往復をします。その 2 つの要求の間に別のクライアントが
// 変更を確定させると、後から書いた側が黙ってそれを消します。版を添えた要求だけが、
// その消し方を拒めます。
var ErrRevisionMismatch = errors.New("the settings changed since they were read")

// Token は、この設定そのものを指す札です。同じ設定なら同じ札、違えば違う札に
// なります。
//
// 数え上げではなく中身から導くのは、**プロセスをまたいでも意味を保つため**です。
// 起動のたびに 1 から数え直す札は、再起動を挟むと別の設定に同じ札が付きます。
// それを条件にした変更は、読んだものとは違う設定の上に、通ってよいものとして
// 載ってしまいます。
//
// 中身から導くと、A → B → A と戻った設定は元の札に戻ります。それでよいのは、
// 札が指すのが「設定の中身」だからです。戻した側の変更は自分で取り消されていて、
// 読んだ人が上書きしてしまうものは何も残っていません (これは ETag の意味その
// ものでもあります)。
func Token(cfg Config) string {
	h := fnv.New64a()
	// Config は JSON にできる型だけでできているので、ここは失敗しません。
	// 失敗したときは空のまま進めます — すべての設定が同じ札になり、条件が
	// 効かなくなるだけで、誤って通すことはありません (どの札とも一致しない
	// のではなく、どれとも一致してしまう点には注意が要りますが、その状態には
	// 到達しません)。
	if data, err := json.Marshal(cfg); err == nil {
		_, _ = h.Write(data)
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

// Save は設定ファイルを書きます。必要ならフォルダも作ります。
//
// これは現在の設定をそのまま符号化したものなので、生成されたファイルが最初に持って
// いたコメントは、何かが保存された時点で失われます。TOML をその場で編集するパーサーを
// 持つ代わりに、設定の表現を 1 つに保つことの代償です。トレイや API がここへ書く頃には、
// ファイルは自らを説明するという役目を既に果たし終えています。
func Save(path string, cfg Config) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	return writeAtomic(path, buf.Bytes())
}

// writeAtomic は、一時的な名前で書いてから rename することで設定ファイルを書きます。
// 中断された書き込みが、切り詰められた設定ファイルを残さないようにするためです。
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), FileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// envLookup は os.Getenv と同じ形で、対応付けをテストできるよう注入します。
type envLookup func(string) string

// ApplyEnv は PTCAMBRIDGE_* の変数を重ねます。これらはファイルより優先されます。
// 公開しているのは、スクリプトから指定する価値のある設定だけです。
//
// 解釈できない値は飛ばすのではなくエラーにします。黙って捨てるとファイルの値が
// そのまま残って起動してしまうので、PTCAMBRIDGE_SERIAL_BAUD=abc と書いた人は、
// 求めていない速度で動くブリッジを手にすることになり、変数が無視されたことは
// どこにも書かれません。それでも全変数を試すので、1 つの打ち間違いが次を隠すことは
// ありません。
//
// 返す名前は、環境変数が実際に指定した設定の葉です。値が変わったかどうかではなく、
// 指定されたかどうかを答えます。ファイルと同じ値を指定することもあり、そのときも
// 上書きは存在します — 呼び出し側がファイルを書き換えても、次の起動ではやはり
// 環境変数が勝ちます。値の差から推測すると、この場合を見落とします。
func (c *Config) ApplyEnv(get envLookup) ([]string, error) {
	var errs []error
	var set []string
	mark := func(leaf string, applied bool) {
		if applied {
			set = append(set, leaf)
		}
	}
	fail := func(applied bool, err error) bool {
		if err != nil {
			errs = append(errs, err)
		}
		return applied
	}

	mark("server.listen", setString(get, "PTCAMBRIDGE_LISTEN", &c.Server.Listen))
	mark("server.boundary", setString(get, "PTCAMBRIDGE_BOUNDARY", &c.Server.Boundary))
	mark("source.type", setString(get, "PTCAMBRIDGE_SOURCE_TYPE", &c.Source.Type))
	mark("source.uvc.device", setString(get, "PTCAMBRIDGE_UVC_DEVICE", &c.Source.UVC.Device))
	mark("source.uvc.size", setString(get, "PTCAMBRIDGE_UVC_SIZE", &c.Source.UVC.Size))
	mark("source.uvc.framerate", fail(setInt(get, "PTCAMBRIDGE_UVC_FRAMERATE", &c.Source.UVC.Framerate)))
	mark("source.uvc.ffmpeg_path", setString(get, "PTCAMBRIDGE_FFMPEG_PATH", &c.Source.UVC.FFmpegPath))
	mark("source.serial.port", setString(get, "PTCAMBRIDGE_SERIAL_PORT", &c.Source.Serial.Port))
	mark("source.serial.baud", fail(setInt(get, "PTCAMBRIDGE_SERIAL_BAUD", &c.Source.Serial.Baud)))
	mark("source.mjpeg.url", setString(get, "PTCAMBRIDGE_MJPEG_URL", &c.Source.MJPEG.URL))
	mark("output.serial.enabled", fail(setBool(get, "PTCAMBRIDGE_OUTPUT_SERIAL_ENABLED", &c.Output.Serial.Enabled)))
	mark("output.serial.port", setString(get, "PTCAMBRIDGE_OUTPUT_SERIAL_PORT", &c.Output.Serial.Port))
	mark("output.serial.baud", fail(setInt(get, "PTCAMBRIDGE_OUTPUT_SERIAL_BAUD", &c.Output.Serial.Baud)))
	mark("papertracker.install_dir", setString(get, EnvInstallDir, &c.PaperTracker.InstallDir))
	mark("papertracker.write_cache", fail(setBool(get, "PTCAMBRIDGE_WRITE_CACHE", &c.PaperTracker.WriteCache)))
	mark("ui.language", setString(get, EnvLanguage, &c.UI.Language))
	mark("log.level", setString(get, "PTCAMBRIDGE_LOG_LEVEL", &c.Log.Level))
	mark("log.dir", setString(get, "PTCAMBRIDGE_LOG_DIR", &c.Log.Dir))

	return set, errors.Join(errs...)
}

// set* は、変数が指定されていて適用したときに true を返します。解釈できなかった
// ものは指定されていなかったことにします。値が設定に入っていない以上、上書きとして
// 数えると、実際には効いていない指定を「効いている」と言うことになります。
func setString(get envLookup, key string, dst *string) bool {
	if v := get(key); v != "" {
		*dst = v
		return true
	}
	return false
}

func setInt(get envLookup, key string, dst *int) (bool, error) {
	v := get(key)
	if v == "" {
		return false, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a whole number", key, v)
	}
	*dst = n
	return true, nil
}

func setBool(get envLookup, key string, dst *bool) (bool, error) {
	v := get(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not true or false", key, v)
	}
	*dst = b
	return true, nil
}

// Normalise は、答えの明らかな空欄を埋めます。検証が本当に誤った値だけを拒否
// すれば済むようにするためです。
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
	c.UI.Language = strings.ToLower(strings.TrimSpace(c.UI.Language))
	if c.UI.Language == "" {
		c.UI.Language = string(i18n.Auto)
	}
	// 既定値を求めているのは 0 だけ。負の速度は誤りであり、黙って 3000000 に
	// してしまうとそれを隠すことになる。ユーザーは自分が書いていない値を読み
	// 返すことになるし、ボードが別の速度を欲していた場合、症状は「開くのに
	// フレームを 1 枚も出さないポート」だけになる。
	if c.Source.Serial.Baud == 0 {
		c.Source.Serial.Baud = DefaultSerialBaud
	}
	// 書き出す側も同じ既定値と同じ理屈です。ただしポート名は埋めません。
	// 何を書いても推測になり、推測したポートへ映像を流し込むのは、当てが外れた
	// ときに取り返しがつきません。
	c.Output.Serial.Port = strings.TrimSpace(c.Output.Serial.Port)
	if c.Output.Serial.Baud == 0 {
		c.Output.Serial.Baud = DefaultSerialBaud
	}
}

// maxExactJSONInt は、JSON の数値として往復できる最大の整数です。JSON の数値は
// 倍精度浮動小数点なので、これを超える整数は読んだ時点で別の値になります。
const maxExactJSONInt int64 = 1<<53 - 1

// fitsInJSON は、整数の設定が設定画面を通っても値を保つかどうかを確かめます。
//
// 拒むのは、丸めが黙っているからです。設定画面は設定を JSON で読み書きするので、
// 2^53 を超える max_frame_size は読んだ時点で近い値に化け、無関係な項目を保存した
// だけでファイルへ書き戻されます。しかも動作中の設定と差が出るので、ログの行を
// 直しただけのつもりでキャプチャが止まって立ち上げ直されます。
//
// 上限を「意味のある大きさ」ではなくここに置いたのは、これが我々の見立てではなく
// 表現できるかどうかの境目だからです。ユーザーが必要とする速度やフレーム長を、
// こちらの想像で狭めることにはなりません。
func fitsInJSON(name string, value int) error {
	if int64(value) > maxExactJSONInt {
		return fmt.Errorf("%s must be at most %d; larger whole numbers change value when the settings page reads them as JSON, got %d",
			name, maxExactJSONInt, value)
	}
	return nil
}

// Validate は、実行時に失敗する設定を報告します。
func (c Config) Validate() error {
	_, port, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		return fmt.Errorf("server.listen %q is not a host:port address: %w", c.Server.Listen, err)
	}
	// ポート 0 は「空いているポートをどれでも」と OS に頼むことであり、それは
	// 同時に 2 つの問題になる。2 つ目の実体が起動するのを止めているのは bind で
	// あり、ポートはこのアプリケーションの身元そのものだが、誰にも取られ得ない
	// ポートは身元として機能しない。2 つ目の実体は何事もなく bind し、クライアントの
	// キャッシュを自分のアドレスに書き換え、先に閉じられた方がどちらであれ、
	// クライアントは消えたポートを指したまま残される。加えて、アドレスは起動の
	// たびに変わるので、それを書き留めたものは次のサインインまでに間違いになる。
	//
	// 問題なのは数であって書き方ではない。"00"、"+0"、"127.0.0.1:" の空のポートは
	// いずれも 0 として net.Listen に届く。services ファイルの名前も解決されるが、
	// こちらを拒む理由はもっと退屈なもの — このアドレスは別のアプリケーションの
	// 設定ファイルに書き込まれるので、あちらでもここと同じことを言うべきだから。
	number, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("server.listen %q must end in a port number", c.Server.Listen)
	}
	if number <= 0 || number > 65535 {
		return fmt.Errorf("server.listen must name a fixed port between 1 and 65535, got %d: port 0 asks for a different one on every start, which leaves the client pointing at an address that no longer exists and lets a second copy of PTCamBridge run alongside this one", number)
	}
	if err := core.ValidateBoundary(c.Server.Boundary); err != nil {
		return fmt.Errorf("server.boundary: %w", err)
	}
	for name, value := range c.Server.ExtraHeaders {
		if err := core.ValidateStreamHeader(name, value); err != nil {
			return fmt.Errorf("server.extra_headers: %w", err)
		}
	}
	switch c.Source.Type {
	case SourceUVC, SourceSerial, SourceMJPEG:
	default:
		return fmt.Errorf("source.type %q must be one of %q, %q or %q", c.Source.Type, SourceUVC, SourceSerial, SourceMJPEG)
	}
	// 0 は「core の既定値を使う」という意味。それ以外で、あり得る最小の JPEG より
	// 小さい値は検証を通過したうえで全フレームを黙って捨てる。パーサーはこれを
	// 絶対的な上限として使うし、順調に読んでいるのに何も配信しないソースは、
	// どこを見ても失敗のようには見えない。
	if c.Source.MaxFrameSize < 0 {
		return fmt.Errorf("source.max_frame_size must not be negative, got %d", c.Source.MaxFrameSize)
	}
	if c.Source.MaxFrameSize > 0 && c.Source.MaxFrameSize < core.MinJPEGSize {
		return fmt.Errorf("source.max_frame_size %d is below the %d bytes of the smallest possible JPEG; use 0 for the default",
			c.Source.MaxFrameSize, core.MinJPEGSize)
	}
	// 0 は選択をデバイスに委ねるということであり、それは実のある答え。負の値は
	// 違う。ドライバは -framerate 引数を丸ごと落とすので、カメラは好きな速度で
	// 動き、Apply はフレームを見て成功と判断し、誤った値が保存される。
	if c.Source.UVC.Framerate < 0 {
		return fmt.Errorf("source.uvc.framerate must not be negative, got %d; use 0 for the device default",
			c.Source.UVC.Framerate)
	}
	// Normalise が 0 を既定値に変え終えているので、ここで 0 以下のまま残っている
	// ものは意図してそう書かれたものであり、誤り。
	if c.Source.Serial.Baud <= 0 {
		return fmt.Errorf("source.serial.baud must be positive, got %d; use 0 for the default of %d",
			c.Source.Serial.Baud, DefaultSerialBaud)
	}
	for i, b := range c.Source.Serial.Header {
		if b < 0 || b > 0xFF {
			return fmt.Errorf("source.serial.header[%d] = %d is not a byte value", i, b)
		}
	}
	if c.Output.Serial.Enabled {
		// ここだけは、有効なときにしか見ません。無効な出力の設定が誤っていても
		// 起動を止める理由になりませんし、止めれば「使っていない機能のせいで
		// ブリッジが上がらない」という、直し方の分からない失敗になります。
		switch {
		case c.Output.Serial.Port == "":
			return errors.New("output.serial.enabled is on but output.serial.port is empty; name the serial port to write to, for example COM7")
		case strings.EqualFold(c.Output.Serial.Port, "auto"):
			// 読む側の "auto" は当たりが外れても黙って聞いているだけですが、
			// 書く側で外すと、他人の機器へ毎秒何メガバイトも流し込むことになります。
			return errors.New(`output.serial.port cannot be "auto"; name the port explicitly, because writing a video stream into a port that turns out to belong to another device cannot be taken back`)
		}
	}
	// 有効かどうかに関わらず見ます。範囲外のバイトはどう解釈しても誤りで、
	// 有効にした日に初めて知らされるより、書いた日に言われた方がましです。
	for i, b := range c.Output.Serial.Header {
		if b < 0 || b > 0xFF {
			return fmt.Errorf("output.serial.header[%d] = %d is not a byte value", i, b)
		}
	}
	if c.Output.Serial.Baud <= 0 {
		return fmt.Errorf("output.serial.baud must be positive, got %d; use 0 for the default of %d",
			c.Output.Serial.Baud, DefaultSerialBaud)
	}
	for _, field := range []struct {
		name  string
		value int
	}{
		{"source.max_frame_size", c.Source.MaxFrameSize},
		{"source.uvc.framerate", c.Source.UVC.Framerate},
		{"source.serial.baud", c.Source.Serial.Baud},
		{"output.serial.baud", c.Output.Serial.Baud},
	} {
		if err := fitsInJSON(field.name, field.value); err != nil {
			return err
		}
	}
	if err := c.CoreTransform().Validate(); err != nil {
		return fmt.Errorf("transform: %w", err)
	}
	// 綴りを誤ったキーと同じ理由で、黙って無視せず拒否する。"jp" と書いた人は
	// 日本語のつもりであり、英語のままのメニューは「この設定は効かない」ように
	// 見える。
	if _, err := i18n.ParseLang(c.UI.Language); err != nil {
		return fmt.Errorf("ui.language: %w", err)
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

// CoreTransform は、設定を core が適用する変換に変換します。
func (c Config) CoreTransform() core.Transform {
	return core.Transform{
		Rotate:     c.Transform.Rotate,
		FlipH:      c.Transform.FlipH,
		FlipV:      c.Transform.FlipV,
		CropSquare: c.Transform.CropSquare,
		Quality:    c.Transform.ReencodeQuality,
	}
}

// SerialHeader は、設定された前置きをバイト列に変換します。未設定なら nil を
// 返し、core の既定値が使われます。
func (c Config) SerialHeader() []byte {
	return headerBytes(c.Source.Serial.Header)
}

// OutputSerialHeader は、書き出す側の前置きについて同じことをします。
func (c Config) OutputSerialHeader() []byte {
	return headerBytes(c.Output.Serial.Header)
}

func headerBytes(header []int) []byte {
	if len(header) == 0 {
		return nil
	}
	out := make([]byte, len(header))
	for i, b := range header {
		out[i] = byte(b)
	}
	return out
}

// StreamHeaders は、設定された追加パートヘッダーを core の形式に変換します。
func (c Config) StreamHeaders() []core.StreamHeader {
	if len(c.Server.ExtraHeaders) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Server.ExtraHeaders))
	for name := range c.Server.ExtraHeaders {
		names = append(names, name)
	}
	// map の反復順は不定なので、ワイヤ形式が安定するよう並べ替える。
	slices.Sort(names)

	out := make([]core.StreamHeader, 0, len(names))
	for _, name := range names {
		out = append(out, core.StreamHeader{Name: name, Value: c.Server.ExtraHeaders[name]})
	}
	return out
}

// IsLoopback は、listen アドレスがローカルマシン内に留まるかを返します。管理 API を
// 提供するのは、留まる場合だけです。
func (c Config) IsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost":
		return true
	case "":
		// ホストが空なら全インターフェースを意味する。
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LogDir はログファイルのフォルダです。既定では設定ファイルの隣の logs フォルダに
// なります。
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
