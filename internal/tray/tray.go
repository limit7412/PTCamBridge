// Package tray はシステムトレイの表側です。ソースの選択、一時停止、状態表示、
// 自動起動の切り替えを担います。
//
// メニューは PTCamBridge が持つ唯一の UI です。それ以上のものは、管理 API と話す
// 別プロセスの領分です。ここの controller インターフェースがその API の公開内容を
// 写しているのは、そのためです。
package tray

import (
	"context"
	_ "embed"
	"reflect"
	"sync"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/i18n"
	"github.com/limit7412/PTCamBridge/internal/status"

	"log/slog"
)

//go:embed icon.ico
var iconICO []byte

// sourceChoice は、ソースのサブメニューの 1 項目です。ラベルはテキストではなく
// メッセージのキーなので、ユーザーが選んだ言語で解決されます。
type sourceChoice struct {
	kind  string
	label i18n.Key
}

var sourceChoices = []sourceChoice{
	{config.SourceUVC, i18n.MenuSourceUVC},
	{config.SourceSerial, i18n.MenuSourceSerial},
	{config.SourceMJPEG, i18n.MenuSourceMJPEG},
}

// Controller は、メニューが操作するブリッジの一部です。
type Controller interface {
	Snapshot() config.Config
	Switch(ctx context.Context, sourceType string) error
	Paused() bool
	SetPaused(paused bool) error
}

// FFmpegFetcher は、メニューが操作する ffmpeg ダウンロードの一部です。
type FFmpegFetcher interface {
	State() ffmpegfetch.State
	Start() error
}

// ffmpegPrompt は、何かがダウンロードされる前にユーザーへ見せる内容です。
//
// PTCamBridge は ffmpeg を同梱していないので、メニュー項目をクリックすると、
// ユーザーの機械が第三者のバイナリを取ってくることになります。誰が作ったのか、
// どれくらいの大きさか、どのライセンスなのか。同意するために必要なのはこの 3 つ
// なので、README ではなく、最初の 1 バイトが動く前の画面に出します。
func ffmpegPrompt(p i18n.Printer, build ffmpegfetch.Build) string {
	return p.F(i18n.DialogFFmpegBody,
		build.Publisher, build.URL, build.Size/(1000*1000), build.License)
}

// ffmpegStatusLine は、メニュー項目そのものに出すダウンロードの説明です。
func ffmpegStatusLine(p i18n.Printer, state ffmpegfetch.State) string {
	switch {
	case state.Downloading && state.Total > 0:
		return p.F(i18n.MenuFFmpegProgress, state.Received*100/state.Total)
	case state.Downloading:
		return p.S(i18n.MenuFFmpegBusy)
	case state.Installed:
		return p.S(i18n.MenuFFmpegInstalled)
	case state.LastError != "":
		return p.S(i18n.MenuFFmpegRetry)
	default:
		return p.S(i18n.MenuFFmpegGet)
	}
}

// Options はトレイを設定します。
type Options struct {
	Controller Controller
	Hub        *hub.Hub
	Status     *status.Tracker
	Log        *slog.Logger
	// Address は bind した listen アドレスです。メニューに表示し、「スナップ
	// ショットを開く」項目にも使います。
	Address string
	// LogDir は「ログフォルダを開く」項目が開く場所です。空なら項目を隠します。
	LogDir string
	// ConfigPath は「設定を編集」項目が開くファイルです。空なら項目を隠します。
	ConfigPath string
	// FFmpeg は「ffmpeg を取得」項目の実体です。nil なら項目を隠します。
	FFmpeg FFmpegFetcher
	// ConfigFlag は、ユーザーがコマンドラインで指定した設定ファイルのパスです
	// (指定があれば)。自動起動の切り替えはこれを登録するので、サインイン時の起動も
	// 同じファイルを使います。空なら既定の場所を意味します。
	ConfigFlag string
	// Printer はメニューの文字列を組み立てます。ゼロ値なら英語になります。
	Printer i18n.Printer
	// OnQuit は、ユーザーが終了を選んだとき、トレイが終わる前に呼ばれます。
	OnQuit func()
}

// actionPause は、watchClicks が一時停止のクリックを報告する際のキーです。ソース
// 種別の名前 — uvc、serial、mjpeg — と名前空間を共有します。
const actionPause = "pause"

// watchClicks は、ブリッジを変更するすべてのメニュー項目からのクリックを、届いた
// 順に 1 本のチャネルへ転送します。
//
// 項目ごとに goroutine を立ててイベントループに case を分ける代わりに、1 つの
// goroutine がすべてのチャネルを見ます。前者はどちらも、順序をユーザー以外の何かに
// 委ねることになります。goroutine を分ければ、2 つのクリックは独立に受け取られてから
// 転送を競います。select の case を分ければ、Go は準備完了のものからランダムに選び
// ます。いずれにせよ「ソースを選んでから一時停止」が逆順で届き得ます。そして一時停止中は
// ソースを変更できないので、それは新しいソースではなく古いソースが一時停止される
// という結果になります。
//
// クリックはブロックせずに渡し、すぐ監視へ戻ります。systray は select と default で
// 送るので、クリックが届くのは、ちょうどそのチャネルで受信側が待っている瞬間だけで、
// それ以外は捨てられます。ここで何かに手間取れば、順序が入れ替わるのではなくクリックが
// 失われることになり、そちらの方が悪い結果です。
func watchClicks(ctx context.Context, log *slog.Logger, entries map[string]<-chan struct{}, out chan<- string) {
	kinds := make([]string, 0, len(entries))
	cases := make([]reflect.SelectCase, 0, len(entries)+1)
	for kind, clicked := range entries {
		kinds = append(kinds, kind)
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(clicked),
		})
	}
	// 最後に置くので、添字は len(kinds) になる。
	cases = append(cases, reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(ctx.Done()),
	})

	for {
		chosen, _, ok := reflect.Select(cases)
		if chosen == len(kinds) || !ok {
			// 終了処理中か、メニュー項目が取り除かれたか。
			return
		}
		select {
		case out <- kinds[chosen]:
		default:
			// ここに来るのは、ワーカーがバッファを埋めるほど遅れている場合だけで、
			// それには検証タイムアウトに達するほど遅い設定変更が要る。代わりに
			// ブロックすると他の項目の監視が止まるし、systray は誰も待っていない
			// クリックを捨てる。
			log.Warn("ignoring a menu click, earlier ones are still being applied", "action", kinds[chosen])
		}
	}
}

// commandQueueDepth は、適用を待つメニュー操作の上限です。クリックは人の速さで
// 届きますし、ワーカーが遅れるのは切替の検証中だけなので、数個あれば実際のユーザーが
// 生み出す量を上回ります。
const commandQueueDepth = 8

// commandQueue は、メニューの操作を投入された順に 1 つずつ、専用の goroutine で
// 適用します。
//
// イベントループから外しているのは、繋がっていないソースには検証タイムアウトまで
// 実力を示す機会が与えられ、それをループ上で走らせるとメニューが一切応答しなくなる
// からです。終了も含めて。そして切替が固まっているとき、ユーザーが手を伸ばすのは
// まさにそれです。
//
// クリックごとに goroutine を立てず 1 つのワーカーにしているのは、これらの操作が
// すべてブリッジのロックで直列化されるため、goroutine を分けると順序をスケジューラに
// 委ねることになるからです。UVC を選んでから MJPEG を選ぶと、競争に勝った方に
// 落ち着くので、メニューが一方のソースを表示し、ブリッジがもう一方で動くという
// ことが起こり得ます。
type commandQueue struct {
	cmds chan func()
	log  *slog.Logger
}

func newCommandQueue(log *slog.Logger, depth int) *commandQueue {
	q := &commandQueue{cmds: make(chan func(), depth), log: log}
	go func() {
		for cmd := range q.cmds {
			cmd()
		}
	}()
	return q
}

// submit は操作をキューに入れ、受け付けられたかどうかを返します。
//
// 送信は決してブロックしません。ワーカーが追いつくまでイベントループを止めることこそ、
// 避けたいことだからです。キューが一杯なら操作は捨てられます。それは、何も起きなかった
// クリックのように見せるのではなく、はっきり伝えます。そして呼び出し側にも返します。
// 自分が何を要求したかを追っている呼び出し側が、実際には行われなかった要求を数えては
// いけないからです。
func (q *commandQueue) submit(what string, cmd func()) bool {
	select {
	case q.cmds <- cmd:
		return true
	default:
		q.log.Warn("ignoring a menu action, earlier ones are still being applied", "action", what)
		return false
	}
}

func (q *commandQueue) close() { close(q.cmds) }

// inFlight は、同じ対象に対する操作が同時に 1 つだけ走るようにします。
//
// commandQueue とは反対の答えです。あちらは操作を並べて順番に実行しますが、こちらは
// 実行中のものがある間、同じ対象への要求を捨てます。並べるのが正しいのは順序に意味が
// ある操作、つまりブリッジの設定を変えるものだけです。開く操作はどれとも競合しないので
// 順序を守る理由が無く、並べれば 1 つの遅い呼び出しが後続すべてを足止めします。
//
// 捨てる方を選ぶ理由は、対象が応答しないときに現れます。開く先が固まっていると、
// クリックのたびに新しい goroutine が OS スレッドを固定したままタイムアウトを待ち、
// 再クリックのぶんだけ積み上がります。そして相手が回復した瞬間、溜まっていた要求が
// 一斉に同じものを何枚も開きます。どちらもユーザーの目には「効かないから押し直した」
// だけの話で、その報いとしては重すぎます。
//
// 対象ごとに見るので、ログフォルダが固まっていても設定ファイルは開けます。同時に走る
// 数は対象の種類だけ、つまりメニューの項目数で頭打ちになります。
type inFlight struct {
	mu      sync.Mutex
	targets map[string]struct{}
}

func newInFlight() *inFlight {
	return &inFlight{targets: make(map[string]struct{})}
}

// begin は対象を実行中として記録し、それが受け付けられたかどうかを返します。
// false なら、その対象は既に実行中なので呼び出し側は何もしてはいけません。
func (f *inFlight) begin(target string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, busy := f.targets[target]; busy {
		return false
	}
	f.targets[target] = struct{}{}
	return true
}

// done は対象を解放します。begin が true を返した呼び出しは、成功したか失敗したかに
// かかわらず必ずこれを呼ばなければなりません。呼ばなければ、その対象は二度と開けません。
func (f *inFlight) done(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.targets, target)
}

// statusLine は、動いているかどうかを知るためにユーザーが読む 1 行です。
func statusLine(p i18n.Printer, snapshot status.Snapshot, paused bool, fps float64, clients int) string {
	source := snapshot.Source
	if source == "" {
		source = p.S(i18n.StatusNoSource)
	}
	switch {
	case paused:
		return p.F(i18n.StatusPaused, source)
	case !snapshot.Connected && snapshot.LastError != "":
		return p.F(i18n.StatusReconnecting, source, truncate(reason(p, snapshot), 60))
	case !snapshot.Connected:
		return p.F(i18n.StatusConnecting, source)
	default:
		return p.F(i18n.StatusRunning, source, fps, clients)
	}
}

// reason は、ソースが落ちている理由を組み立てます。
//
// 翻訳するのは、その失敗を tracker が「ユーザーの対処できる数少ないもの」の 1 つと
// 認識した場合だけです。それ以外はドライバ自身の文言、つまりログにあるのと同じ言葉に
// なります。誰も翻訳していないメッセージの方が、たまたま読み手の言語になっている
// 曖昧なメッセージより、読む人の役に立ちます。
func reason(p i18n.Printer, snapshot status.Snapshot) string {
	if snapshot.LastErrorKey != "" {
		return p.S(i18n.Key(snapshot.LastErrorKey))
	}
	return snapshot.LastError
}

// truncate は、メニュー項目に収まるよう理由を短くします。
//
// 数えるのはバイトではなくルーンです。日本語に訳した理由は 60 バイトを楽に超え、
// そこで切ると文字の途中に落ちます。トレイに届くのは不正な UTF-8 であり、短くなった
// 文ではなく置換文字として描かれます。
func truncate(s string, max int) string {
	if max < 3 {
		max = 3
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-3]) + "..."
}
