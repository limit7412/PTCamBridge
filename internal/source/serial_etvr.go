package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// AutoPort は、ポートを明示せず USB のベンダー ID から選ぶようドライバに
// 指示する値です。
const AutoPort = "auto"

// DefaultSerialBaud は、Babble の有線ファームウェアが動作する速度です。
const DefaultSerialBaud = 3000000

// serialReadTimeout は、ループがコンテキストのキャンセルに気づけるよう、
// ブロッキング読み取りに上限を設けるものです。停滞の検出器ではありません。
const serialReadTimeout = 200 * time.Millisecond

// serialStallTimeout は、ドライバがボードを失われたとみなして再接続するまでに、
// ポートがフレームを出さずにいられる時間です。var にしているのはテストがこれを
// 使い切らずに済むようにするためです。テストが見るのは停滞が何を報告するかであり、
// 毎回 5 秒待っても分かることは増えません。
var serialStallTimeout = 5 * time.Second

// previewBytes は、何も解析できなかったストリームをログにどれだけ載せるかです。
// 前置きと長さフィールドの先頭が見える程度で、行が丸ごと 16 進ダンプになるほどでは
// ない量です。
const previewBytes = 16

// maxWarnedStreams は、1 つのポートについて何種類の「解析できないストリーム」まで
// 警告し、それ以降を debug に落とすかの上限です。
//
// 上限が必要なのは、「バイト列が変わった」ことが常に新しい知らせとは限らないから
// です。流れているストリームを載せたポートは、読み手がどこで加わったかによって開く
// たびに違う先頭バイトを返します。ストリームだけを鍵にすると再接続のたびに警告する
// ことになり、それはこの記憶が防ごうとしている氾濫そのものです。バイト列が変動して
// いると分かるには数回で十分ですし、debug 行にはその全部が残ります。
const maxWarnedStreams = 3

// knownCameraVIDs は、Babble や OpenIris のボードに載っているブリッジや MCU の
// USB ベンダー ID です。Espressif、Silicon Labs、QinHeng、FTDI、Raspberry Pi。
var knownCameraVIDs = map[string]string{
	"303A": "Espressif",
	"10C4": "Silicon Labs CP210x",
	"1A86": "QinHeng CH340",
	"0403": "FTDI",
	"2E8A": "Raspberry Pi",
}

// ErrNoSerialPort は、カメラボードであり得るものが探索で 1 つも見つからなかった
// ことを表します。
var ErrNoSerialPort = errors.New("serial: no port matched a known camera vendor ID; set source.serial.port explicitly")

// SerialConfig は、有線 Babble ボードのドライバの設定です。
type SerialConfig struct {
	// Port は "COM5" のようなポート名、またはベンダー ID で探させる AutoPort です。
	Port string
	// Baud は 0 なら DefaultSerialBaud になります。
	Baud int
	// Header は、文書化された 0xFF 0xA0 0xFF 0xA1 と異なるファームウェア向けに
	// パケットの前置きを上書きします。
	Header []byte
	// MaxFrameSize は JPEG 1 枚の上限です。0 なら core の既定値を使います。
	MaxFrameSize int
}

// Serial は、シリアルポートから OpenIris/ETVR の有線パケットストリームを読みます。
type Serial struct {
	cfg      SerialConfig
	parser   core.ETVRParser
	log      *slog.Logger
	reporter Reporter

	// tried は、AutoPort が既に選び、そして戻ってきたポートを覚えています。再接続の
	// たびに同じ候補を返すのではなく、探索が先へ進むようにするためです。proven は
	// 実際にフレームを出した最後のポートで、巡回に先んじて 1 回の再試行を得ます。
	//
	// どちらに触れるのも Run だけで、Run は単一スレッドです。
	tried  map[string]struct{}
	proven string

	// tail は、直近のパケットの終端より後ろにあるとパーサーが最後に報告した量です。
	// なぜここに持つのかは splitPackets を参照してください。
	tail int

	// warned は、どの解析できないストリームを既に報告したかをポートごとに覚えて
	// います。再接続のたびに同じ苦情を繰り返さないためです。
	//
	// ポート単位かつストリーム単位なのは、両者が独立に変わるからです。"auto" は
	// 候補の間を巡回するので、最後のストリームだけを覚えていると、巡回が一周する
	// たびにまた警告することになります。またファームウェアの更新や、同じ COM 番号に
	// 現れた別のデバイスは、改めて言うべき新しい事柄です。そのポートがフレームを
	// 出せばエントリは捨てるので、動いていたポートが後で壊れたら、それはまた新しい
	// 知らせになります。
	warned map[string][]string

	// listPorts は ListSerialPorts で、テストでは差し替えます。確認する価値が
	// あるのは巡回の部分であり、列挙が何を返すかを操作できなければそこに到達
	// できません。
	listPorts func() ([]SerialPort, error)
	// openPort は serial.Open で、同じ理由でテストでは差し替えます。停滞した
	// セッションが線について何を語るかは、そこから何が出てくるかを決められなければ
	// 確認できません。
	openPort func(name string, baud int) (serialPort, error)
}

// serialPort は、このドライバが使う go.bug.st/serial.Port の部分です。
type serialPort interface {
	Read(p []byte) (int, error)
	SetReadTimeout(t time.Duration) error
	Close() error
}

// NewSerial はドライバを組み立て、パケットヘッダーを先に検証します。そうしないと
// 誤ったヘッダーは再接続のたびにまったく同じ失敗を繰り返すことになります。
func NewSerial(cfg SerialConfig, log *slog.Logger, reporter Reporter) (*Serial, error) {
	if cfg.Baud <= 0 {
		cfg.Baud = DefaultSerialBaud
	}
	if strings.TrimSpace(cfg.Port) == "" {
		cfg.Port = AutoPort
	}
	parser, err := core.NewETVRParser(cfg.Header, cfg.MaxFrameSize)
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	if reporter == nil {
		reporter = NopReporter{}
	}
	return &Serial{
		cfg:       cfg,
		parser:    parser,
		log:       log,
		reporter:  reporter,
		tried:     map[string]struct{}{},
		warned:    map[string][]string{},
		listPorts: ListSerialPorts,
		openPort: func(name string, baud int) (serialPort, error) {
			return serial.Open(name, &serial.Mode{BaudRate: baud})
		},
	}, nil
}

// Name は Source を実装します。
func (s *Serial) Name() string { return "serial" }

// Run は Source を実装します。
func (s *Serial) Run(ctx context.Context, out chan<- core.Frame) error {
	return runWithBackoff(ctx, s.log, s.Name(), s.reporter, func(ctx context.Context) error {
		return s.session(ctx, out)
	})
}

// session はポートを開き、失敗するかコンテキストが終わるまで読み続けます。
func (s *Serial) session(ctx context.Context, out chan<- core.Frame) error {
	name, err := s.resolvePort()
	if err != nil {
		return err
	}
	port, err := s.openPort(name, s.cfg.Baud)
	if err != nil {
		return fmt.Errorf("serial: open %s at %d baud: %w", name, s.cfg.Baud, err)
	}
	defer port.Close()

	if err := port.SetReadTimeout(serialReadTimeout); err != nil {
		return fmt.Errorf("serial: set read timeout: %w", err)
	}
	s.log.Info("serial port opened", "port", name, "baud", s.cfg.Baud)

	assembler := newFrameAssembler(s.splitPackets, s.cfg.MaxFrameSize)
	buf := make([]byte, readChunk)
	lastFrame := time.Now()
	var count uint64
	// 直前のフレームからのバイト数 (停滞を測るのと同じ窓で数える) と、何も解析
	// できていない間のストリームの先頭。何のためにあるかは stalled を参照。
	var sinceFrame int64
	var preview []byte

	for {
		if ctx.Err() != nil {
			return nil
		}
		// バイトではなくフレームで測り、読み取りがタイムアウトしたときだけでなく
		// 毎回確認する。"auto" は、パケットを形作らないまま喋り続ける別のシリアル
		// デバイスに当たることがある。到着を鍵にすると、そのポートを永遠に開いた
		// まま探索をやり直さないので、後から挿された本物のボードは決して
		// 見つからない。
		if time.Since(lastFrame) > serialStallTimeout {
			return s.stalled(name, count, sinceFrame, preview)
		}
		n, err := port.Read(buf)
		if err != nil {
			return fmt.Errorf("serial: read from %s: %w", name, err)
		}
		if n == 0 {
			// エラーではなく読み取りのタイムアウト。
			continue
		}
		sinceFrame += int64(n)
		if count == 0 && len(preview) < previewBytes {
			take := previewBytes - len(preview)
			if take > n {
				take = n
			}
			preview = append(preview, buf[:take]...)
		}

		frames := assembler.feed(buf[:n])
		for _, f := range frames {
			lastFrame = time.Now()
			if count == 0 {
				// このポートは実力を示した。自動探索は巡回で通り過ぎるのではなく、
				// まずここへ戻ってくるべき。
				s.proven = name
				// 今は動いている。後で解析できなくなれば、以前どう言われていようと
				// それは改めて新しい知らせになる。
				delete(s.warned, name)
				s.reporter.Connected(s.Name())
			}
			count++
			if err := send(ctx, out, f); err != nil {
				return nil
			}
		}
		if len(frames) > 0 {
			// 0 にはしない。1 回の読み取りがフレームとその後のバイト列を同時に
			// 運ぶことがあり、それらは停滞の起点となったフレームより後に届いた
			// ものだから。捨ててしまうと、実際にはパケットの途中まで来ている、
			// あるいは解析できない何かを送っているボードについて、「無言のポート」
			// と報告することになる。
			//
			// assembler が抱えている量ではなくパーサーから取る。assembler が持つ
			// のはまだパケットになり得る分だけで、ヘッダーが短ければそれはほとんど
			// 無く、1 バイトのヘッダーならまったく無い。
			sinceFrame = int64(s.tail)
		}
	}
}

// stalled は、フレームを出さなくなったセッションについて説明し、何も読み取れな
// かったときに線上に何があったかを述べます。
//
// 停滞はバイトではなくフレームで数えます (ループを参照)。そのためそれだけでは、
// 無言のポートと、よく喋るがこのパーサーの知らないパケットを送るポートを区別
// できません。この 2 つは正反対の対処を要します。前者はストリームを出していない
// ボード、後者はファームウェアと合っていないヘッダーかボーレートです。両者を
// 分けるのがバイトの総量です。後者については、決め手になるのはバイト列そのもの
// であり、だからこそログに出します。パケットヘッダーを設定可能にしているのは
// まさにファームウェアの版が異なるからで、設定すべき値はその行の中にあります。
func (s *Serial) stalled(name string, frames uint64, sinceFrame int64, preview []byte) error {
	err := fmt.Errorf("serial: %s produced no frame for %s (%d bytes received in that time)", name, serialStallTimeout, sinceFrame)
	if frames > 0 || sinceFrame == 0 {
		// 動いていたボードが黙ったか、ポートが無言かのどちらか。どちらもエラーが
		// 説明しており、見せるものは無い。少し前までフレームを出していたポートに、
		// パケット形式の問題があるわけではない。
		return err
	}

	head := hexPreview(preview)
	// 1 ストリームにつき 1 回、1 ポートにつき決まった回数だけ言う。再接続ループは
	// 数秒ごとに戻ってくるので、既に述べた苦情を繰り返しても何も足さない。
	// ストリームが動き続けるポートを見ている人のために、バイト列は debug 行に残る。
	seen := s.warned[name]
	if slices.Contains(seen, head) || len(seen) >= maxWarnedStreams {
		s.log.Debug("still nothing that parses on the serial port", "port", name, "first_bytes", head)
		return err
	}
	s.warned[name] = append(seen, head)
	s.log.Warn("bytes are arriving on the serial port but no packet matched; check the firmware's preamble against source.serial.header, and the baud rate",
		"port", name,
		"baud", s.cfg.Baud,
		"expected_header", hexPreview(s.parser.Header()),
		"first_bytes", head,
		"bytes_received", sinceFrame,
	)
	return err
}

// hexPreview は、プロトコル文書が書くのと同じ形式でバイト列を表します。ログに
// 出たものを設定のヘッダーと目で見比べられるようにするためです。
func hexPreview(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02x", c)
	}
	return sb.String()
}

// splitPackets は、パーサーを frameAssembler のシグネチャに合わせます。パーサーは
// 自身のペイロード上限を持っているので、maxSize はそちらで既に適用されています。
//
// パーサーの tail は返さずドライバに置いています。assembler のシグネチャにその
// 余地が無く、広げれば UVC と MJPEG のドライバにも及ぶからです。あちらは同じ
// シグネチャを共有していて、これを必要としていません。上の tried や proven と同じく、
// Run が単一スレッドなので安全です。
func (s *Serial) splitPackets(buf []byte, _ int) ([][]byte, []byte) {
	frames, rest, tail := s.parser.Parse(buf)
	s.tail = tail
	return frames, rest
}

// resolvePort は設定されたポートを返し、AutoPort が指定されていれば探索します。
//
// 探索は単にリストの先頭を取るわけではありません。既知ベンダーのボードが同時に
// 2 つ挿さっていることがあります — Babble ボードと、無関係な CP210x のドングルなど。
// 常に先頭を返すと、誤ったデバイスを開いては停滞し、また開くことを繰り返し、その
// 隣にあるカメラは一度も試されません。そこで各候補は 1 回ずつ使い、次の再接続では
// その次へ進みます。
func (s *Serial) resolvePort() (string, error) {
	if !strings.EqualFold(s.cfg.Port, AutoPort) {
		return s.cfg.Port, nil
	}
	ports, err := s.listPorts()
	if err != nil {
		return "", fmt.Errorf("serial: enumerate ports: %w", err)
	}

	candidates, guessed := autoCandidates(ports)
	if len(candidates) == 0 {
		return "", ErrNoSerialPort
	}

	// 切断の後は、既にフレームを届けたことのあるポートを先に試す。ボードが移動した
	// より、ケーブルが引っ張られた可能性の方がはるかに高い。試行は 1 回だけなので、
	// 本当に居なくなっていれば巡回はそのまま続く。
	if s.proven != "" {
		name := s.proven
		s.proven = ""
		if slices.Contains(candidates, name) {
			clear(s.tried)
			s.tried[name] = struct{}{}
			s.log.Info("reopening the serial port that was working", "port", name)
			return name, nil
		}
	}

	name, ok := s.firstUntried(candidates)
	if !ok {
		// すべての候補が一巡した。諦めずに巡回をやり直す。ボードは抜き差しされ得る
		// ので、1 分前に失敗したポートが今は正解かもしれない。
		s.log.Info("every candidate serial port has been tried, starting over", "ports", len(candidates))
		clear(s.tried)
		name, _ = s.firstUntried(candidates)
	}
	s.tried[name] = struct{}{}
	if guessed {
		// 省略せず全部言う。これは推測であり、前回の推測は誰かの午後を丸ごと
		// 奪ったから。そのポートは VR ヘッドセットで、ログはポートが
		// "auto-selected" されたとしか言っていなかった。この分岐で開くものは
		// そもそもカメラですらないかもしれないので、それが実際に何なのかを行に書く。
		s.log.Warn("no serial port matches a known camera board; trying the only port there is, which may not be a camera",
			"port", name, "device", describePort(ports, name))
	} else {
		s.log.Info("auto-selected serial port", "port", name, "candidates", len(candidates))
	}
	return name, nil
}

// describePort は、列挙がそのポートについて知っていることを文字列にします。
// 開くつもりの無かったデバイスを読み手が見分けられる必要のあるログ行のためです。
func describePort(ports []SerialPort, name string) string {
	for _, p := range ports {
		if p.Name != name {
			continue
		}
		parts := make([]string, 0, 3)
		if p.VID != "" || p.PID != "" {
			parts = append(parts, p.VID+":"+p.PID)
		}
		if p.Product != "" {
			parts = append(parts, p.Product)
		}
		if len(parts) == 0 {
			return "unknown"
		}
		return strings.Join(parts, " ")
	}
	return "unknown"
}

// firstUntried は、このドライバがまだ開いていない最初の候補を返します。
func (s *Serial) firstUntried(candidates []string) (string, bool) {
	for _, name := range candidates {
		if _, seen := s.tried[name]; !seen {
			return name, true
		}
	}
	return "", false
}

// autoCandidates は、AutoPort が開く気のあるポートを、有望なものから順に並べます。
//
// 得られる積極的な根拠は既知のベンダー ID だけなので、それらが先に来ます。順序は
// ListSerialPorts が並べたままです。機械に 1 つしかないポートは最後の頼みです。
// 他にあり得るものが無い以上、試す価値があります。
func autoCandidates(ports []SerialPort) (names []string, guessed bool) {
	for _, p := range ports {
		if p.Vendor != "" {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 && len(ports) == 1 {
		return []string{ports[0].Name}, true
	}
	return names, false
}

// SerialPort は、トレイメニューと管理 API に提示するポートを表します。
type SerialPort struct {
	Name string `json:"name"`
	// Vendor は、USB のベンダー ID が「Babble のファームウェアが載ることで知られる
	// ボード系列」のものであるときに設定されます。
	Vendor  string `json:"vendor,omitempty"`
	VID     string `json:"vid,omitempty"`
	PID     string `json:"pid,omitempty"`
	Product string `json:"product,omitempty"`
}

// ListSerialPorts はシリアルポートを列挙します。既知のカメラボードを先頭に並べる
// ので、呼び出し側はリストの先頭を取れば済みます。
func ListSerialPorts() ([]SerialPort, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	var known, others []SerialPort
	for _, d := range details {
		p := SerialPort{Name: d.Name, Product: d.Product}
		if d.IsUSB {
			p.VID, p.PID = strings.ToUpper(d.VID), strings.ToUpper(d.PID)
			if vendor, ok := knownCameraVIDs[p.VID]; ok {
				p.Vendor = vendor
			}
		}
		if p.Vendor != "" {
			known = append(known, p)
		} else {
			others = append(others, p)
		}
	}
	return append(known, others...), nil
}
