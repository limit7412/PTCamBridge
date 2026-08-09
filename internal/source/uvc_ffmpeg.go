package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
)

// UVC のキャプチャは、ネイティブのバインディングではなく ffmpeg の子プロセスを
// 通して行います。Windows には cgo 無しで使えるカメラ API が無く、外部プロセスに
// 委ねることで CGO_ENABLED=0 の単一実行ファイルとしてビルドできる構成を保てます。

// readChunk は標準出力の読み取り単位です。240x240 の JPEG は数キロバイトなので、
// メモリを無駄にせず 1 回の読み取りで数フレーム分を収められます。
const readChunk = 64 << 10

// stderrTail は、失敗を説明するために残しておく ffmpeg の診断出力の量です。
const stderrTail = 4 << 10

// defaultUVCStallTimeout は、子プロセスを殺して試行をやり直すまでに、ffmpeg が
// フレームを出さずにいられる時間です。
//
// 詰まった USB カメラや DirectShow フィルタは、ffmpeg を終了させるのではなく
// 動いたまま黙らせます。その標準出力に対するブロッキング読み取りは決して返って
// きません。再接続ループはその読み取りの下流にあるので、これが無ければブリッジは
// ユーザーが再起動するまで死んだままです。この窓は ffmpeg 自身の起動も覆う必要が
// あります。Windows では最初のフレームまでに 1〜2 秒かかります。
const defaultUVCStallTimeout = 10 * time.Second

// UVCConfig は、ffmpeg を後ろに置いたカメラドライバの設定です。
type UVCConfig struct {
	// Device はプラットフォームのキャプチャデバイスです。Windows では DirectShow の
	// フレンドリ名、Linux では /dev/video* のパス、macOS では AVFoundation の番号です。
	Device string
	// Size は "240x240" のような WxH 文字列です。空ならデバイスに任せます。
	Size string
	// Framerate は要求するキャプチャ速度です。0 ならデバイスに任せます。
	Framerate int
	// FFmpegPath は同梱のバイナリを上書きします。
	FFmpegPath string
	// MaxFrameSize は JPEG 1 枚の上限です。0 なら core の既定値を使います。
	MaxFrameSize int
	// StallTimeout は、殺して試行をやり直すまでに ffmpeg がフレームを出さずに
	// いられる時間です。0 なら defaultUVCStallTimeout を使います。
	StallTimeout time.Duration
}

// UVC は、ffmpeg の MJPEG 出力を読むことでカメラからキャプチャします。
type UVC struct {
	cfg      UVCConfig
	log      *slog.Logger
	reporter Reporter
	// copyCodec は、カメラに MJPEG を要求してそのまま流す指定です。そのまま流して
	// 何も得られなければ下ろし、再エンコードでも何も得られなければまた立てます。
	// chooseCodec を参照。
	copyCodec bool
	// reencodeWorked は、このカメラから再エンコードで少なくとも一度フレームが
	// 得られたことを記録します。それだけではカメラに MJPEG が無い証拠にはなりません。
	// 直前のそのまま流す試みは、単にデバイスが使用中の瞬間に当たっただけで、再
	// エンコードが走る頃には空いていたのかもしれないからです。
	reencodeWorked bool
	// reencodeReal は、その時点で動作すると分かっているデバイスに対して、そのまま
	// 流す試みが 2 度目も失敗したことを記録します。2 つのモードはこの比較のために
	// あり、これが問いに決着をつけます。
	reencodeReal bool

	// modes は、このカメラが申告したモードの一覧を、一度調べた結果です。
	// modesAsked は、調べようとしたかどうか (失敗も含む) です。
	//
	// 覚えておくのは、再接続ループが数秒おきに戻ってくるからです。失敗のたびに
	// 調べ直すと、カメラを開く回数が倍になります。答えは配線が変わらない限り
	// 変わらないので、1 回で足ります。
	modes      []Mode
	modesAsked bool
}

// NewUVC はドライバを組み立てます。デバイスの指定は必須です。
func NewUVC(cfg UVCConfig, log *slog.Logger, reporter Reporter) (*UVC, error) {
	if strings.TrimSpace(cfg.Device) == "" {
		return nil, ErrNoDevice
	}
	if reporter == nil {
		reporter = NopReporter{}
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = defaultUVCStallTimeout
	}
	return &UVC{cfg: cfg, log: log, reporter: reporter, copyCodec: true}, nil
}

// Name は Source を実装します。
func (u *UVC) Name() string { return "uvc" }

// Run は Source を実装します。
func (u *UVC) Run(ctx context.Context, out chan<- core.Frame) error {
	return runWithBackoff(ctx, u.log, u.Name(), u.reporter, func(ctx context.Context) error {
		// まだ挿さっていないだけのカメラと、永遠に存在しないカメラは同じことを
		// 報告してくる。だからここでは何も致命的として扱わない。ドライバは再試行を
		// 続け、後から繋がれたカメラを拾う。ソースが機能しているかを決めるのは
		// ブリッジの側であり、呼び出し側が答えを必要とするときに最初のフレームを
		// 待つことでそれを判断する。
		frames, diag, err := u.capture(ctx, out, u.copyCodec)
		u.chooseCodec(frames, diag, err)
		return u.explainRefusedMode(ctx, diag, err)
	})
}

// modeRefusedSigns は、「デバイスは見つかったが、要求した解像度やフレームレートを
// 持っていない」ことを意味する ffmpeg の診断です。
//
// 文言の一致なので、これで再試行を止めたり、コーデックの選び方を変えたりはしません
// (deviceUnavailableSigns の注意書きと同じ理由です)。使うのは、既に起きた失敗に
// 説明を足すためだけです。取りこぼしても、これまでどおりのエラーが出ます。
var modeRefusedSigns = []string{
	"could not set video options", // dshow
}

// explainRefusedMode は、要求したモードが拒まれた失敗に、そのカメラが実際に持って
// いるモードを添えます。
//
// これが要るのは、元の診断が行き止まりだからです。"Could not set video options" は
// 何が悪かったかを言いますが、代わりに何を書けばよいかは言いません。設定ファイルを
// 開いた人も、ログを読んだ人も、そこで止まります。
//
// 調べるのは 1 回だけです。再接続ループは数秒おきに戻ってくるので、毎回調べると
// カメラを開く回数が倍になります。答えは配線が変わらない限り変わりません。
func (u *UVC) explainRefusedMode(ctx context.Context, diag string, err error) error {
	if err == nil || !refusedMode(diag) {
		return err
	}
	// 何も要求していないのに拒まれたのなら、それはモードの話ではない。添えられる
	// ことは何も無いし、「空を指定したのが悪い」と読ませてしまう。
	if u.cfg.Size == "" && u.cfg.Framerate == 0 {
		return err
	}

	if !u.modesAsked {
		u.modesAsked = true
		modes, listErr := ListModes(ctx, u.cfg.FFmpegPath, u.cfg.Device)
		if listErr != nil {
			u.log.Debug("could not list the camera modes to explain the failure", "device", u.cfg.Device, "error", listErr)
		}
		u.modes = modes
	}
	if len(u.modes) == 0 {
		return err
	}
	return fmt.Errorf("%w; %s offers %s", err, u.cfg.Device, describeModes(u.modes))
}

// refusedMode は、診断が「そのモードは無い」と言っているかどうかを返します。
func refusedMode(diag string) bool {
	lower := strings.ToLower(diag)
	for _, sign := range modeRefusedSigns {
		if strings.Contains(lower, sign) {
			return true
		}
	}
	return false
}

// chooseCodec は、次の試行をどのモードで走らせるかを選びます。
//
// そのまま流す試みが失敗したことは、カメラに MJPEG 出力が無い証拠にはなりません。
// 使用中のデバイスや、サインイン直後でまだ落ち着いていないデバイスも同じように
// 失敗し、しかもこちらが認識できるとは期待できない言葉でそう言います。診断の文言は
// ffmpeg の版、バックエンド、ドライバによって違います。文言の一致で判定しようとすると、
// 推測をより見えにくい場所へ移すだけです。
//
// 両者を区別するのは、その次に起きることです。再エンコードは同じデバイスに別の出力
// 形式を要求します。それでも何も得られなければ、最初から問題はデバイスの側にあった
// ということなので、そのまま流す方をもう一度試します。後で復帰したカメラが、以降
// ずっとデコードと再エンコードにかけられずに済みます。
//
// 再エンコードが成功した場合も、それで終わりではありません。そのまま流す試みの
// ときだけカメラが使用中で、この試みの頃には空いていたのかもしれません。2 つの結果は
// 数分離れており、その間にデバイスの状態は変わり得ます。そこで次の試行では、動作
// すると分かったデバイスに対してもう一度そのまま流す方を試し、2 度目の失敗で初めて
// 決着とします。本当に MJPEG を持たないカメラでは最初の切断の後に 1 回分の試行を
// 余計に払うことになりますが、これが、一瞬の競合のせいで以降のすべてのフレームが
// デコードと再エンコードになるのを防いでいます。
func (u *UVC) chooseCodec(frames uint64, diag string, err error) {
	if frames > 0 {
		switch {
		case u.copyCodec:
			// そのまま流せている。つまり以前おかしかったのはデバイスの側。
			u.reencodeWorked = false
		case !u.reencodeWorked:
			// 知る価値はあるが、まだ信じる段階ではない。カメラがフレームを出せる
			// こと自体は示されたので、この試行が終わったらもう一度そのまま流す方を
			// 試す。
			u.reencodeWorked = true
			u.copyCodec = true
			u.log.Debug("re-encoding worked; passthrough gets one more try on the next attempt", "device", u.cfg.Device)
		}
		return
	}
	if err == nil {
		return
	}

	if u.copyCodec {
		if deviceUnavailable(diag) {
			// デバイスがそもそも開かなかったので、その形式について何も分かって
			// いない。再エンコードを試すのは無意味であるうえに誤解を招く。
			return
		}
		if u.reencodeWorked {
			// これで 2 度目。しかもその間に、同じカメラからフレームを届けた再
			// エンコードを挟んでいる。ここで得られる最も確かな答えがこれ。
			u.copyCodec = false
			u.reencodeReal = true
			u.log.Info("camera has no MJPEG output of its own, re-encoding from here on", "device", u.cfg.Device)
			return
		}
		u.copyCodec = false
		u.log.Debug("passthrough produced no frames, trying re-encoding", "device", u.cfg.Device)
		return
	}
	if u.reencodeReal {
		return
	}
	u.copyCodec = true
	u.log.Debug("re-encoding produced no frames either, going back to passthrough", "device", u.cfg.Device)
}

// capture は ffmpeg のプロセスを 1 つ最後まで走らせ、得られたフレーム数と ffmpeg の
// 診断出力を返します。
func (u *UVC) capture(ctx context.Context, out chan<- core.Frame, copyCodec bool) (uint64, string, error) {
	path, err := u.ffmpegPath()
	if err != nil {
		// 致命的なのは、設定されたパスが機能しない場合だけ。設定が特定のファイルを
		// 名指しており、それが存在しないのだから、いくら再試行しても直らない。
		// 1 つも見つからないのは別の話で、ブリッジが動いている最中にトレイから
		// 取得でき、それを拾うのが再試行ループ。ここで諦めると、取得が終わっても
		// ユーザーが再起動するまでカメラは死んだままになる。
		if errors.Is(err, ErrNoFFmpeg) {
			return 0, "", err
		}
		return 0, "", fatalf(err)
	}
	args := u.args(copyCodec)
	u.log.Debug("starting ffmpeg", "path", path, "args", strings.Join(args, " "))

	// 読み取りを解除するのは子プロセスを殺すこと。黙ったまま動き続ける ffmpeg は
	// パイプを開いたままにするので、読む側にはタイムアウトさせるものが無い。
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stall := time.AfterFunc(u.cfg.StallTimeout, cancelRun)
	defer stall.Stop()

	cmd := exec.CommandContext(runCtx, path, args...)
	configureChildProcess(cmd)
	// 猶予を入れないと、殺された ffmpeg がパイプを開いたまま残り、Wait が詰まる。
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, "", fmt.Errorf("uvc: stdout pipe: %w", err)
	}
	diag := &tailWriter{max: stderrTail}
	cmd.Stderr = diag

	if err := cmd.Start(); err != nil {
		return 0, "", fatalf(fmt.Errorf("uvc: start ffmpeg: %w", err))
	}

	frames, readErr := u.pump(ctx, stdout, out, func() {
		stall.Reset(u.cfg.StallTimeout)
	})
	waitErr := cmd.Wait()
	stderr := diag.String()

	// 子プロセスを待つことが、ソース切替が再び開く前にデバイスが解放されている
	// ことを保証する。UVC のアクセスは排他的。
	if ctx.Err() != nil {
		return frames, stderr, nil
	}
	switch {
	case runCtx.Err() != nil:
		return frames, stderr, fmt.Errorf("uvc: %s produced no frame for %s (ffmpeg: %s)", u.cfg.Device, u.cfg.StallTimeout, stderr)
	case readErr != nil:
		return frames, stderr, fmt.Errorf("uvc: %w (ffmpeg: %s)", readErr, stderr)
	case waitErr != nil:
		return frames, stderr, fmt.Errorf("uvc: ffmpeg exited: %w (%s)", waitErr, stderr)
	default:
		return frames, stderr, fmt.Errorf("uvc: ffmpeg exited without error (%s)", stderr)
	}
}

// deviceUnavailableSigns は、「デバイスがそもそも開けなかった」ことを意味する
// ffmpeg の診断です。「開いたが MJPEG を出せなかった」場合とは異なります。文言の
// 一致は経験則なので、コーデックのフォールバックだけを左右し、再試行をやめる判断には
// 決して使いません。取りこぼしても余計な再エンコードを 1 回払うだけであり、それは
// 以前は無条件に起きていたことです。
var deviceUnavailableSigns = []string{
	"could not find video device",       // dshow
	"could not enumerate video devices", // dshow
	"cannot open video device",          // v4l2
	"could not open video device",
	"no such file or directory", // v4l2
	"video device not found",    // avfoundation
}

// deviceUnavailable は、ffmpeg の診断が、要求した形式ではなくデバイスの側を
// 咎めているかどうかを返します。
func deviceUnavailable(diag string) bool {
	lower := strings.ToLower(diag)
	for _, sign := range deviceUnavailableSigns {
		if strings.Contains(lower, sign) {
			return true
		}
	}
	return false
}

// pump は ffmpeg の MJPEG 標準出力を読み、完結した画像を順に転送します。フレーム
// ごとに alive を呼ぶので、呼び出し側は遅いカメラと詰まったカメラを区別できます。
func (u *UVC) pump(ctx context.Context, stdout io.Reader, out chan<- core.Frame, alive func()) (uint64, error) {
	assembler := newFrameAssembler(core.SplitJPEGStream, u.cfg.MaxFrameSize)
	buf := make([]byte, readChunk)
	var count uint64

	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			for _, f := range assembler.feed(buf[:n]) {
				// バイトではなくフレームで測る。画像として組み上がらない何かを
				// 書き続ける ffmpeg は、何も書かない ffmpeg と同じく死んでいる。
				// そうしなければ気づけるのは片方だけになる。
				alive()
				if count == 0 {
					u.reporter.Connected(u.Name())
				}
				count++
				if sendErr := send(ctx, out, f); sendErr != nil {
					return count, sendErr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return count, errors.New("ffmpeg closed its output")
			}
			return count, err
		}
	}
}

// args は、現在のプラットフォーム向けの ffmpeg コマンドラインを組み立てます。
// カメラが既に MJPEG を出しているなら、そのまま流すことでデコードとエンコードの
// 往復を避けられます。
func (u *UVC) args(copyCodec bool) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}

	format, input := platformInput(u.cfg.Device)
	args = append(args, "-f", format)
	if u.cfg.Size != "" {
		args = append(args, "-video_size", u.cfg.Size)
	}
	if u.cfg.Framerate > 0 {
		args = append(args, "-framerate", fmt.Sprint(u.cfg.Framerate))
	}
	if copyCodec {
		// フレームをそのまま流せるよう、デバイス自身に MJPEG を要求する。
		switch format {
		case "v4l2":
			args = append(args, "-input_format", "mjpeg")
		default:
			args = append(args, "-vcodec", "mjpeg")
		}
	}
	args = append(args, "-i", input, "-an")

	if copyCodec {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "mjpeg", "-q:v", "4")
	}
	return append(args, "-f", "mjpeg", "pipe:1")
}

// platformInput は、設定されたデバイスを、動作中の OS に応じた ffmpeg の入力形式と
// 引数に対応付けます。
func platformInput(device string) (format, input string) {
	switch runtime.GOOS {
	case "windows":
		return "dshow", "video=" + device
	case "darwin":
		return "avfoundation", device
	default:
		return "v4l2", device
	}
}

// ffmpegPath はバイナリを解決します。設定による上書き、実行ファイルの隣にある
// コピー、PATH、そしてブリッジ自身が取得したもの、の順です。
//
// 取得したコピーを最後にしているのは意図的です。それは PTCamBridge が管理するもの
// であり、つまりユーザーが避けにくいものでもあります。PATH より前に置くと、特定の
// ffmpeg を意図して指しているインストールが、誰かがトレイの項目を一度押した途端に
// それを黙って使わなくなります。
func (u *UVC) ffmpegPath() (string, error) {
	if u.cfg.FFmpegPath != "" {
		if _, err := os.Stat(u.cfg.FFmpegPath); err != nil {
			return "", fmt.Errorf("uvc: configured ffmpeg_path %q is not usable: %w", u.cfg.FFmpegPath, err)
		}
		return u.cfg.FFmpegPath, nil
	}
	if exe, err := os.Executable(); err == nil {
		alongside := filepath.Join(filepath.Dir(exe), ffmpegBinaryName())
		if _, statErr := os.Stat(alongside); statErr == nil {
			return alongside, nil
		}
	}
	if path, err := exec.LookPath(ffmpegBinaryName()); err == nil {
		return path, nil
	}
	if path, ok := ffmpegfetch.Installed(); ok {
		return path, nil
	}
	return "", ErrNoFFmpeg
}

// ErrNoDevice は、カメラが指定されていないことを意味します。
//
// これは初回起動が始まる時点の状態です。推測できるカメラ名など無いので、設定ファイルは
// このフィールドを空にして書き出されます。つまり、新しいユーザーがこのプログラムから
// 最初に目にするエラーである可能性が最も高いものです。だから症状だけでなく対処の全体を
// 述べます。名前を調べるために何を実行するのか、そして見つけた名前をどこに書くのか。
var ErrNoDevice = errors.New(`uvc: no camera configured. Run "ptcambridge -list-devices" to see the cameras attached, then put one of the names in [source.uvc] device in the settings file (or set PTCAMBRIDGE_UVC_DEVICE)`)

// ErrNoFFmpeg は、ドライバが探すどこにも ffmpeg が無いことを意味します。
//
// 意図的に致命的にしていません。打ち間違えた ffmpeg_path と違い、これはブリッジを
// 再起動しなくても機械が抜け出せる状態です。ユーザーがトレイから ffmpeg を取得するか、
// PATH に入れれば済みます。それに気づくのが再試行ループです。まだ挿さっていない
// カメラと同じ理屈です。
var ErrNoFFmpeg = errors.New("uvc: ffmpeg not found next to the executable, on PATH, or in the settings folder; fetch it from the tray menu or set source.uvc.ffmpeg_path")

func ffmpegBinaryName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

// Device は、トレイメニューと管理 API を通じてユーザーに提示するキャプチャ
// デバイスです。
type Device struct {
	Name string `json:"name"`
	// Alternative は DirectShow のデバイスパスです。フレンドリ名と違い、再起動を
	// またいでも変わりません。
	Alternative string `json:"alternative,omitempty"`
}

// Mode は、カメラが 1 つの出力形式について申告する組み合わせです。
//
// 最小と最大を別々に持つのは、ffmpeg がそう報告するからです。多くのカメラは
// 両者が同じ値の行を形式ごとに並べますが、範囲で答えるカメラもあり、その場合は
// 間のどの大きさも使えます。片方に丸めると、使える値を隠すか、使えない値を
// 勧めることになります。
type Mode struct {
	// Format は "mjpeg" のようなコーデック名か、"yuyv422" のようなピクセル形式です。
	Format string `json:"format,omitempty"`
	// MinSize と MaxSize は "640x480" の形です。等しいこともあります。
	MinSize string `json:"min_size"`
	MaxSize string `json:"max_size"`
	// MinFPS と MaxFPS は、その大きさでカメラが受け付けるフレームレートの幅です。
	MinFPS float64 `json:"min_fps,omitempty"`
	MaxFPS float64 `json:"max_fps,omitempty"`
}

// String は、設定ファイルに書く値がそのまま読み取れる形にします。
func (m Mode) String() string {
	size := m.MinSize
	if m.MaxSize != m.MinSize {
		size += "-" + m.MaxSize
	}
	fps := trimFloat(m.MaxFPS)
	if m.MinFPS != m.MaxFPS {
		fps = trimFloat(m.MinFPS) + "-" + fps
	}

	out := size
	if fps != "0" {
		out += " @ " + fps + "fps"
	}
	if m.Format != "" {
		out += " (" + m.Format + ")"
	}
	return out
}

// trimFloat は 30 を "30"、29.97 を "29.97" にします。設定に書くのは整数なので、
// "30.000000" と出して人に読み替えさせる理由がありません。
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// dshowModeLine は ffmpeg の -list_options の出力に一致します。次の形です。
//
//	[dshow @ 0000...]   vcodec=mjpeg  min s=1280x720 fps=5 max s=1280x720 fps=30
//	[dshow @ 0000...]   pixel_format=yuyv422  min s=640x480 fps=5 max s=640x480 fps=30
var dshowModeLine = regexp.MustCompile(
	`(?:vcodec|pixel_format)=(\S+)\s+min s=(\d+x\d+)\s+fps=([\d.]+)\s+max s=(\d+x\d+)\s+fps=([\d.]+)`)

// parseDshowModes は、ffmpeg が申告したモードを取り出します。
//
// 同じ行が複数回出ることがある (ピンが複数あるカメラなど) ので重複は畳みます。
// 順序は ffmpeg が並べたとおりに保ちます。カメラが先に挙げるものには意味があり、
// 並べ替えるとその手がかりを捨てることになります。
func parseDshowModes(out string) []Mode {
	var modes []Mode
	seen := map[Mode]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		m := dshowModeLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		minFPS, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		maxFPS, err := strconv.ParseFloat(m[5], 64)
		if err != nil {
			continue
		}
		mode := Mode{Format: m[1], MinSize: m[2], MaxSize: m[4], MinFPS: minFPS, MaxFPS: maxFPS}
		if _, dup := seen[mode]; dup {
			continue
		}
		seen[mode] = struct{}{}
		modes = append(modes, mode)
	}
	return modes
}

// dshowDeviceLine は ffmpeg のデバイス一覧に一致します。たとえば次の形です。
//
//	[dshow @ 0000...] "HD Webcam" (video)
var (
	dshowDeviceLine = regexp.MustCompile(`"([^"]+)"\s*\((video|audio)\)`)
	dshowAltLine    = regexp.MustCompile(`Alternative name\s*"([^"]+)"`)
)

// ListDevices は映像キャプチャデバイスを列挙します。Windows では ffmpeg に
// DirectShow の一覧を要求します。ffmpeg はそれを標準エラー出力に書いたうえで
// 非ゼロで終了します。それ以外の環境では /dev/video* のノードを返します。
func ListDevices(ctx context.Context, ffmpegPath string) ([]Device, error) {
	if runtime.GOOS != "windows" {
		return listVideoNodes()
	}
	u := &UVC{cfg: UVCConfig{Device: "dummy", FFmpegPath: ffmpegPath}, log: slog.Default()}
	path, err := u.ffmpegPath()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	configureChildProcess(cmd)
	diag := &tailWriter{max: 64 << 10}
	cmd.Stderr = diag
	runErr := cmd.Run()

	if ctx.Err() != nil {
		return nil, fmt.Errorf("uvc: listing capture devices did not finish: %w", ctx.Err())
	}
	// ここで非ゼロ終了になるのは想定どおり。"dummy" は実在のデバイスではないし、
	// 一覧そのものが標準エラー出力に出る。終了ステータス以外のものが返ったなら
	// ffmpeg が動かなかったということであり、それは空のカメラ一覧を見せるより
	// 報告する価値がある。
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return nil, fmt.Errorf("uvc: run ffmpeg to list devices: %w", runErr)
	}

	return parseDshowDevices(diag.String()), nil
}

// ListModes は、1 台のカメラが申告する出力形式の組み合わせを返します。
//
// ListDevices とは別にしてあり、そちらからは呼びません。-list_options はデバイスを
// 開いて問い合わせるので、カメラ 1 台につき ffmpeg を 1 回起動します。デバイス一覧は
// デバイス一覧は画面を開くたび、トレイのメニューを開くたびに読まれるものなので、
// そこに混ぜると「どんなカメラがあるか」を見るたびに全カメラを掴みに行くことに
// なります。
//
// Windows 以外では何も返しません。この関数があるのは DirectShow が「持っていない
// モードを要求されたらデバイスを開かない」ためで、その診断が要るのも Windows です。
func ListModes(ctx context.Context, ffmpegPath, device string) ([]Mode, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	if strings.TrimSpace(device) == "" {
		return nil, errors.New("uvc: cannot list the modes of a camera with no name")
	}
	u := &UVC{cfg: UVCConfig{Device: device, FFmpegPath: ffmpegPath}, log: slog.Default()}
	path, err := u.ffmpegPath()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	_, input := platformInput(device)
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-f", "dshow", "-list_options", "true", "-i", input)
	configureChildProcess(cmd)
	diag := &tailWriter{max: 64 << 10}
	cmd.Stderr = diag
	runErr := cmd.Run()

	if ctx.Err() != nil {
		return nil, fmt.Errorf("uvc: listing the modes of %q did not finish: %w", device, ctx.Err())
	}
	// ListDevices と同じく、非ゼロ終了は想定どおり。一覧を書いてから ffmpeg は
	// 「入力が無い」と言って終わる。
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return nil, fmt.Errorf("uvc: run ffmpeg to list the modes of %q: %w", device, runErr)
	}

	modes := parseDshowModes(diag.String())
	if len(modes) == 0 {
		// 開けなかったか、この ffmpeg が別の形で書いている。どちらにせよ黙って
		// 空を返すと、呼び出し側は「モードが 1 つも無いカメラ」と受け取る。
		return nil, fmt.Errorf("uvc: ffmpeg listed no modes for %q (ffmpeg: %s)", device, diag.String())
	}
	return modes, nil
}

// describeModes は、モードの一覧を 1 行にします。
func describeModes(modes []Mode) string {
	names := make([]string, 0, len(modes))
	for _, m := range modes {
		names = append(names, m.String())
	}
	return strings.Join(names, ", ")
}

// parseDshowDevices は、ffmpeg のデバイス一覧から映像の項目を取り出します。
func parseDshowDevices(out string) []Device {
	var devices []Device
	for _, line := range strings.Split(out, "\n") {
		if alt := dshowAltLine.FindStringSubmatch(line); alt != nil {
			if len(devices) > 0 && devices[len(devices)-1].Alternative == "" {
				devices[len(devices)-1].Alternative = alt[1]
			}
			continue
		}
		m := dshowDeviceLine.FindStringSubmatch(line)
		if m == nil || m[2] != "video" {
			continue
		}
		devices = append(devices, Device{Name: m[1]})
	}
	return devices
}

// listVideoNodes は V4L2 のキャプチャノードを列挙します。DirectShow の一覧に
// 相当する Linux 版です。Windows 以外のビルドでもトレイメニューが埋まるように
// するためのものです。
func listVideoNodes() ([]Device, error) {
	matches, err := filepath.Glob("/dev/video*")
	if err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(matches))
	for _, m := range matches {
		devices = append(devices, Device{Name: m})
	}
	return devices, nil
}

// tailWriter は、書き込まれた末尾 max バイトだけを保持します。子プロセスを走らせ
// 続けても、その診断出力が無制限に太らないようにするためです。
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(string(w.buf))
}
