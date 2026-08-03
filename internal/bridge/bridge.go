// Package bridge は、キャプチャソースをフレーム hub に繋ぎ、その両方の生存期間を
// 受け持ちます。これにより、トレイメニューと管理 API は、ドライバがどう作られるかを
// 知らないまま実行時にソースを変更できます。
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/server"
	"github.com/limit7412/PTCamBridge/internal/source"
	"github.com/limit7412/PTCamBridge/internal/status"
)

// undecodableRecheckInterval は、そのソースからまだ 1 枚もデコードできていない間に、
// pump がデコードを再試行する間隔です。その間のフレームは捨てます。使える画像を
// まだ 1 枚も出していない上流から来たものであり、それをキャプチャ速度で全部デコード
// するのは、得られる答えに見合わないからです。
var undecodableRecheckInterval = 500 * time.Millisecond

// frameQueueDepth は、ドライバと変換段の間の枠です。1 フレームで足ります。その先の
// hub は決してブロックしませんし、キューを深くしても遅延が増えるだけです。
const frameQueueDepth = 1

// startVerifyTimeout は、Apply が新しいソースの実力を待ち、諦めるまでの時間です。
//
// 実力の証明は最初のフレームであって、早々にエラーが出ないことではありません。
// ドライバは自力で再接続するので、「まだ失敗していない」ことは何も語りません。
//
// この窓は最初の試行 1 回分より長くなければなりません。さもないと、動くはずだった
// ソースが遅いというだけで巻き戻されます。最悪のケースは 2 つあり、いずれも近い値
// です。
//
//   - MJPEG は接続タイムアウトを dial・TLS・応答ヘッダーのそれぞれに費やし、続いて
//     最初のフレームを待つ停滞タイムアウトを費やす。5 秒 x 4。
//   - UVC は最初の ffmpeg に停滞タイムアウトを費やし、ネイティブ MJPEG を持たない
//     カメラはさらにバックオフと 2 回目を費やす。10 秒 + 1 秒 + 10 秒。
//
// 動くソースがこれによって何かを払うことはありません。フレームが hub に届いた瞬間に
// 返るので、長く走るのは答えが「否」になるときだけです。
var startVerifyTimeout = 30 * time.Second

// StreamConfigurator は、設定変更のうち HTTP サーバが受け持ち、自分で適用しなければ
// ならない部分を受け取ります。
type StreamConfigurator interface {
	SetStreamOptions(encoder core.MultipartEncoder, holdOnSourceLoss bool)
}

// Bridge は稼働中のソースを所有し、そのフレームを hub へ配信し直します。
type Bridge struct {
	hub    *hub.Hub
	status *status.Tracker
	log    *slog.Logger

	// cfgPath は Apply が設定を保存する先です。空なら保存しません。
	cfgPath string

	// persistBase は、環境変数とコマンドラインを重ねる前に設定ファイルが述べていた
	// 内容です。保存はここから始めます。一度きりの実行のための -device や
	// PTCAMBRIDGE_* が、ユーザーが恒久的に選んだかのように書き戻されないためです。
	persistBase config.Config

	// unsaved は、ファイルまで届かなかった書き込みです。ファイルが最新である間は
	// nil です。これを保持していれば、再試行は失われた設定を書けます。そうしないと、
	// 既にその値を持っている動作中の設定と差分を取って「することが無い」と判断して
	// しまいます。
	unsaved *pendingSave

	// stream には、HTTP サーバが自分で適用し直す必要のある設定を伝えます。組み立て
	// 時に一度だけ設定し、それは何かが Apply を呼べるようになる前のことです。
	stream StreamConfigurator

	// view は、トレイが 1 秒ごとに読む状態の写しです。これを mu 越しに読むと、
	// 遅い Apply の間 — 最長で startVerifyTimeout — トレイのイベントループ全体が
	// 凍りつき、失敗しつつあるソースに機会を与えている最中、ユーザーは終了する
	// ことすらできなくなります。
	view atomic.Pointer[view]

	mu      sync.Mutex
	cfg     config.Config
	root    context.Context
	cancel  context.CancelFunc
	stopped chan struct{}
	paused  bool

	// died は、cfg のもとで起動したドライバが、キャンセルされたのではなく自力で
	// 止まったことを示します。404 を返す MJPEG の上流、ディスクに無い ffmpeg など。
	// atomic なのは、それを知るのがドライバの goroutine であり、その goroutine が
	// mu を取れないからです。検証中の Apply が、まさにその知らせを待ちながら mu を
	// 保持しています。
	//
	// 単独ではなく provenLocked を通して読んでください。起動と再開はフレームを
	// 待たずに立ち上げるので、記録できるのは「ドライバが動き始めた」ことだけです。
	// これは答えのもう半分であり、これが無いと、1 秒後に死んだソースも動いている
	// ものとして数えられます。そうなるとブリッジは、一時停止中に別のソースを渡す
	// ことを拒みます。この仕組み全体が開こうとしているのは、まさにその袋小路です。
	died atomic.Bool

	// proven は、cfg の設定がドライバを起動させたこと、そしてそれ以降それを覆す
	// ことが何も起きていないことを示します。
	//
	// これを必要とする判断が 2 つあり、どちらもこれが無いために誤っていました。
	// 失敗した変更の巻き戻しは、戻る先の設定が動いていたと決めてかかっていました。
	// 動いていなかった場合 — ブリッジが、そもそも組み立てられないソースで起動した
	// 場合であり、設定されていない UVC がまさにそれです — 巻き戻しも失敗し、何も
	// キャプチャしないまま残りました。一時停止中の変更を拒む処理は、守るべき動く
	// ソースがあると決めてかかり、無いときにも拒み続けていました。
	//
	// 一時停止はこれを下ろしません。意図的に止められたソースも依然として動く
	// ソースであり、上の拒否が守っているのはまさにそれだからです。
	proven bool
}

// pendingSave は、失敗してまだ行われていない設定の書き込みです。
//
// 両方の要素が必要です。want は設定全体なので、そのまま書き出せます。from は、この
// 保留中の書き込みが最初に組み立てられた時点でファイルが持っていた内容であり、
// want のどの部分がブリッジ自身の意図によるものか — 両者が異なる葉 — を示す唯一の
// 手がかりです。want のそれ以外は、その時点のファイルの写しでしかなく、それを
// その後ユーザーが編集したファイルの上に敷き直せば、編集を取り消すことになります。
type pendingSave struct {
	want config.Config
	from config.Config
}

// view は、呼び出し側が読むだけで決して変更しないもののロックフリーな写しです。
type view struct {
	cfg    config.Config
	paused bool
}

// provenLocked は、現在の設定の背後に動くソースがあるかどうかを返します。起動に
// 成功し、その後自力で止まっていないソースのことです。呼び出し側が mu を保持します。
func (b *Bridge) provenLocked() bool {
	return b.proven && !b.died.Load()
}

// publishView はロックフリーな写しを更新します。呼び出し側が mu を保持します。
func (b *Bridge) publishView() {
	b.view.Store(&view{cfg: b.cfg, paused: b.paused})
}

// New は、渡された設定でブリッジを組み立てます。キャプチャを始めるには Start を
// 呼ぶ必要があります。
func New(cfg config.Config, cfgPath string, h *hub.Hub, st *status.Tracker, log *slog.Logger) *Bridge {
	b := &Bridge{hub: h, status: st, log: log, cfgPath: cfgPath, cfg: cfg, persistBase: cfg}
	b.publishView()
	return b
}

// SetPersistBase は、ファイルが持っているとおりの設定を記録します。保存はこれを
// 土台にします。これが無いと、実効設定がそのまま保存され、一度きりの上書きが、
// 何かが Apply を呼んだ最初の瞬間に恒久的なものになります。
func (b *Bridge) SetPersistBase(cfg config.Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.persistBase = cfg
}

// SetStreamConfigurator は HTTP サーバを登録し、Apply がそちらの受け持つストリーム
// 設定を渡せるようにします。組み立て時、管理 API やトレイが Apply に到達できるように
// なる前に呼ぶ必要があります。
func (b *Bridge) SetStreamConfigurator(sc StreamConfigurator) {
	b.stream = sc
}

// Start はキャプチャを開始し、ctx がキャンセルされるまで続けます。このコンテキストは、
// 後から Apply や Switch で起動されるすべてのソースの生存期間も区切ります。
func (b *Bridge) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.root != nil {
		return errors.New("bridge: already started")
	}
	b.root = ctx
	// 起動時は検証しない。まだ挿さっていないカメラは、現れたときに拾えなければ
	// ならず、ドライバを畳んでしまうとそれができなくなる。
	return b.startLocked()
}

// Stop はキャプチャを止め、ドライバがデバイスを解放し終えるまで待ちます。
func (b *Bridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopLocked()
}

// Snapshot は現在有効な設定を返します。
//
// ロックフリーな写しを読むので、設定変更の最中でも答えられます。設定の読み取りと
// 変更を 1 つの操作にする必要がある呼び出し側は、自身で mu を保持しなければなりません。
// Switch を参照。
func (b *Bridge) Snapshot() config.Config {
	return b.view.Load().cfg
}

// Apply は新しい設定を採用してソースを再起動し、設定ファイルのパスが与えられて
// いれば保存します。設定を残すのは新しいソースが起動できた場合だけなので、誤った
// デバイス名を渡してもブリッジが何も動かない状態にはなりません。
//
// 起動時にしか効かない設定は保存しますが、動作中のブリッジには適用しません。
// その名前を返すので、呼び出し側は「保存したが今は効いていない」と言えます。
// deferRestartRequired を参照。保存の失敗は config.ErrNotSaved を包んだエラーとして
// 報告します。今変更したものが再起動を越えないことを、呼び出し側が知る必要が
// あるからです。
func (b *Bridge) Apply(ctx context.Context, cfg config.Config) ([]string, error) {
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return b.applyLocked(ctx, cfg)
}

// applyLocked は mu を保持済みの Apply です。先に現在の設定を読む必要のある
// 呼び出し側が、読み取り・変更・適用の全体を、間に何も挟ませずに行えるようにする
// ためのものです。cfg の正規化と検証は呼び出し側が済ませています。
func (b *Bridge) applyLocked(ctx context.Context, cfg config.Config) ([]string, error) {
	// ロック待ちは、別の呼び出し側の検証まるごと分の長さになり得るので、ここへ
	// 辿り着いたリクエストは既に居なくなっているかもしれない。この先で起きることは
	// どれも、そのリクエストの代わりに勝手にやってよいものではない。サーバ設定
	// だけを触る変更は検証に到達しないので、そうしなければ、タイムアウトを告げ
	// られた呼び出し側のために適用され書き出されてしまう。
	if ctx != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("bridge: the request ended before its change was applied: %w", ctx.Err())
	}

	previous := b.cfg
	// 要求された内容は、これから畳む前に取っておく。保存するのはこちら。cfg の方は
	// この後、動作中のブリッジが実際に使うものへ削られる。
	requested := cfg
	// 何かを畳む前に読む。ここから先このフラグは「試している設定」を追うことに
	// なるし、置き換えられる側が動いていたかどうかだけが、巻き戻しが成功し得ると
	// 言える根拠だから。
	provenBefore := b.provenLocked()
	// 起動時にしか読まれない設定は、ここで cfg から取り除かれる。保存はされるが
	// 動作中のブリッジは古い値のまま動き続ける。deferRestartRequired を参照。
	deferred := deferRestartRequired(previous, &cfg)

	// Apply の保証は「新しいソースが起動できた場合にのみ設定を残す」ことだが、
	// 一時停止中は何も起動できない。確認できるのはせいぜいドライバのオブジェクトを
	// 構築できることまでで、それはデバイスの存在について何も語らない。それでも変更を
	// 受け入れると、既知の良い設定を未検証の設定と引き換えにして書き出すことになる。
	// resume も検証はしない。サインイン後に挿されたカメラを拾う必要があるので、
	// resume は起動時とまったく同じくドライバに再試行させるだけ。だったら、そう
	// 言ってしまう方がどれよりましだ。
	//
	// 以上はすべて「既知の良い設定がある」ことを前提にしている。それが無い場合 —
	// 何も起動していないか、起動したものが既に失敗しているか — この拒否は何も守らず、
	// 残された唯一の手 (別のソースを選ぶこと) を奪うだけになる。既に落ちている
	// ブリッジを一時停止した人は、設定ファイルを編集しなければ戻せなくなる。
	if b.paused && b.provenLocked() && !captureUnchanged(previous, cfg) {
		return nil, errors.New("capture is paused, so a new source cannot be tried: resume first, then change it")
	}

	// 何かを畳む前にエンコーダを組み立てる。使えない boundary のせいで、今動いて
	// いるソースをユーザーから奪うべきではない。
	encoder, err := core.NewMultipartEncoder(cfg.Server.Boundary, cfg.StreamHeaders())
	if err != nil {
		return nil, fmt.Errorf("server.boundary: %w", err)
	}

	if captureUnchanged(previous, cfg) && b.captureAsExpectedLocked() {
		// 動いたのはサーバ側の設定だけなので、カメラには触れない。再起動すれば
		// 何の得も無くストリームが途切れるし、カメラがたまたま再接続中であれば
		// 下の検証が変更を丸ごと拒否してしまう。まさにその状況のための設定である
		// hold_on_source_loss さえも。
		b.cfg = cfg
		b.publishView()
	} else {
		b.stopLocked()
		b.cfg = cfg
		b.publishView()

		if err := b.verifyStartLocked(ctx); err != nil {
			b.log.Error("new settings could not start a source, reverting", "error", err)
			b.cfg = previous
			b.publishView()

			// 元のソースは、改めて実力を示させることなく戻す。動いていた場合は
			// それが正しいし、動いていなかった場合もやる価値がある。新しいソースを
			// 畳んだ時点でステータスは消えており、古いソースと、それが動いていな
			// かった理由をトレイと /healthz に戻すのがこれだから。省略すれば、
			// 失敗したカメラ変更への答えとして、具体的な訴えを「ソース無し」に
			// 置き換えることになる。
			if revertErr := b.startLocked(); revertErr != nil {
				if !provenBefore {
					// この変更の前も動いていなかったので、この 2 つ目の失敗は
					// 新しい知らせではないし、呼び出し側が求めたこととは無関係。
					// 2 つ並べて報告すれば、関係のある方が埋もれる。
					b.log.Warn("nothing is capturing: the settings in place before this change had not started a source either", "error", revertErr)
					return nil, err
				}
				return nil, fmt.Errorf("apply failed (%w) and the previous source could not be restored: %v", err, revertErr)
			}
			return nil, err
		}
	}

	if b.stream != nil {
		b.stream.SetStreamOptions(encoder, cfg.Server.HoldOnSourceLoss)
	}

	if b.cfgPath != "" {
		// 変更を重ねる先は、ファイルの最後に判明している内容ではなく、まだ書き
		// 込みを待っているものの方。保存に失敗した後、この 2 つは同じではないし、
		// ファイルを土台にすると先の変更を取り落とす。呼び出し側がエラーを受けて
		// まったく同じ設定を送り直してきた場合も同様で、それは動作中の設定との
		// 差分が無いため、そうしなければ古いファイルを書き直して成功を報告する
		// ことになる。
		base, baseErr := b.saveBaseLocked()
		// 保存するのは要求された内容。取り除いた葉もここに含まれる。それこそが
		// 「次の起動で反映される」ということ。
		saved := mergeChanges(base, previous, requested)
		err := baseErr
		if err == nil {
			err = config.Save(b.cfgPath, saved)
		}
		if err != nil {
			// 動作中の設定は既に正しいので何も畳まない。呼び出し側には伝える。
			// 変更が一時的なものであると言えるようにするため。保留中の書き込みは
			// 保持しておき、ファイルが再び書けるようになったときに、再試行か次の
			// 変更がそれを書く。
			b.holdUnsavedLocked(base, previous, requested)
			b.log.Error("settings applied but could not be saved", "path", b.cfgPath, "error", err)
			return deferred, fmt.Errorf("%w to %s: %w", config.ErrNotSaved, b.cfgPath, err)
		}
		b.persistBase = saved
		b.unsaved = nil
	}
	if len(deferred) > 0 {
		b.log.Info("settings saved but not applied, they are only read at startup", "settings", deferred)
	}
	return deferred, nil
}

// captureAsExpectedLocked は、キャプチャが現在の設定の求める状態にあるかを返します。
// それが、手を触れずにおいて安全である根拠です。
//
// ドライバは自力で止まることがあります。404 を返す MJPEG の上流や、まだディスクに
// 無い ffmpeg のように、再試行では直らないものに対して Run は FatalError を返します。
// それを再起動するものは無く、それを生んだ設定は b.cfg にそのまま残っています。原因に
// 対処したうえで同じソースを選び直すのが、明らかな復帰の手順です。設定が変わって
// いないからと再起動を省けば、/healthz が 503 のままなのに成功を返すことになります。
//
// 一時停止中・停止済み・未起動は、いずれも「想定どおり」に数えます。動いているべき
// ものが無いのだから、正すべきものも無いからです。
func (b *Bridge) captureAsExpectedLocked() bool {
	if b.root == nil || b.paused || b.root.Err() != nil {
		return true
	}
	if b.stopped == nil {
		return false
	}
	select {
	case <-b.stopped:
		// キャプチャの goroutine が両方とも終了している。
		return false
	default:
		return true
	}
}

// holdUnsavedLocked は、書けなかった変更を記録します。後の保存がそれを果たせる
// ようにするためです。
//
// 既に手元にある保留中の書き込みは、ファイルから作り直さずその場で拡張します。その
// 基点は動かしてはいけません。それこそが、ブリッジ自身の変更と、たまたま want に
// 入っているファイルの内容とを分けているものであり、前へ動かせば、その間にユーザーが
// 編集したものまで、ブリッジが書き戻すつもりの値の集合に畳み込んでしまいます。
func (b *Bridge) holdUnsavedLocked(base, previous, cfg config.Config) {
	if b.unsaved != nil {
		b.unsaved.want = mergeChanges(b.unsaved.want, previous, cfg)
		return
	}
	// base は今読んだままのファイルなので、それとこの変更が生むものとの差が、
	// そのままブリッジの求めているもの。読み取りに失敗した場合は最後に判明して
	// いる内容が入る。得られる中では最善の推測であり、何も書かないという代案より
	// 悪くはない。
	b.unsaved = &pendingSave{want: mergeChanges(base, previous, cfg), from: base}
}

// saveBaseLocked は、次の保存が土台にすべきものを返します。今この瞬間の設定
// ファイルと、以前の保存が書き損ねたものを合わせたものです。
//
// 読み直しが効いてくるのは、ファイルを書くのがここだけではないからです。トレイには
// 「設定を編集」があり、再起動を要する設定はその方法でしか変えられません。つまり
// server.listen を編集した後、再起動する前にトレイで何かを触ったユーザーは、起動時に
// 捉えた基点によってその編集を上書きされてしまいます。
//
// 存在するのに読めない、あるいは解析できないファイルは、フォールバックの理由では
// なくエラーです。そうなる最もありがちな経緯はユーザーが編集の途中であることで、
// その上に書けば、既に有効になっていて後からでも書ける変更を保存するために、編集を
// 破壊することになります。
//
// そもそもファイルが無い場合は話が違います。失われるものは無いので、最後に判明して
// いる内容が正しい基点であり、Save はそこからファイルを作り直します。
// 読み取りの失敗は報告しますが、保留中の書き込みを飛ばしはしません。呼び出し側は
// 返ってきたものから次の保留中の書き込みを組み立てるので、以前の変更を含まない
// 基点はそれらを黙って落とします。そうしないと、ファイルが解析できない間に行われた
// 2 つの変更は、再び読めるようになったとき 2 つ目しか書かれません。1 つ目は既に
// 動作中の設定に入っており、もはや差分として現れないからです。
func (b *Bridge) saveBaseLocked() (config.Config, error) {
	// ファイルについて分かっている最後のもの。新しい順。保留中の書き込みは、それが
	// 組み立てられた時点でファイルが持っていた内容を記録しており、起動時の写しより
	// 新しい。そして作り直すファイルはそこから組み立てなければならない。さもないと、
	// ファイルが消える前に行われた編集が取り消された形で戻ってくる。
	base := b.persistBase
	if b.unsaved != nil {
		base = b.unsaved.from
	}
	var readErr error
	if _, err := os.Stat(b.cfgPath); err == nil {
		onDisk, err := config.LoadFile(b.cfgPath)
		if err != nil {
			readErr = fmt.Errorf("re-read %s before saving: %w", b.cfgPath, err)
		} else {
			base = onDisk
		}
	}
	if b.unsaved != nil {
		// 保留中の書き込みを、今ファイルが述べている内容の上に敷き直す。写すのは
		// 実際に変えるつもりだった葉だけ。want の残りは当時のファイルの写しであり、
		// それを書き戻せば、その後の編集を取り消すことになる。
		base = mergeChanges(base, b.unsaved.from, b.unsaved.want)
	}
	return base, readErr
}

// mergeChanges は、この変更が実際に触れた値を next から取って base に反映した
// ものを返します。
//
// 要点は、何に触れないかです。呼び出し側が変更しなかったフィールドは設定ファイルが
// 持っていた値のまま残るので、この実行にだけ適用される上書きが、ツリーの別の場所の
// 無関係な変更によって書き戻されることはありません。
func mergeChanges(base, previous, next config.Config) config.Config {
	out := base
	mergeChanged(reflect.ValueOf(&out).Elem(), reflect.ValueOf(previous), reflect.ValueOf(next))
	return out
}

// mergeChanged は設定ツリーを歩き、異なる葉を写します。
func mergeChanged(out, previous, next reflect.Value) {
	switch out.Kind() {
	case reflect.Struct:
		for i := 0; i < out.NumField(); i++ {
			mergeChanged(out.Field(i), previous.Field(i), next.Field(i))
		}
		return
	case reflect.Map:
		mergeChangedMap(out, previous, next)
		return
	}
	if !reflect.DeepEqual(previous.Interface(), next.Interface()) {
		out.Set(next)
	}
}

// mergeChangedMap は、この変更が実際に触れた項目を写します。
//
// map は 1 つの値ではありません。server.extra_headers は独立した設定の集まりが
// たまたま 1 つのテーブルとして書かれているもので、API 越しに変更されるのと同じか
// それ以上に手で編集されます。PaperTracker のリリースに合わせてワイヤ形式を調整
// することこそが、その存在理由だからです。丸ごと置き換えると、ユーザーがファイルに
// 足したヘッダーを、ブリッジが別のヘッダーを変えたという理由で落とすことになります。
// 葉ごとに併合するのは、まさにそれを防ぐためです。
//
// 変更によって取り除かれたキーは、ここでも取り除きます。それも他と同じく、その
// キーに対する変更だからです。
func mergeChangedMap(out, previous, next reflect.Value) {
	touched := make(map[any]reflect.Value)
	for _, key := range next.MapKeys() {
		was := previous.MapIndex(key)
		now := next.MapIndex(key)
		if !was.IsValid() || !reflect.DeepEqual(was.Interface(), now.Interface()) {
			touched[key.Interface()] = now
		}
	}
	for _, key := range previous.MapKeys() {
		if !next.MapIndex(key).IsValid() {
			// 無効な値は「このキーは無くなった」を表す。
			touched[key.Interface()] = reflect.Value{}
		}
	}
	if len(touched) == 0 {
		return
	}

	// 書き込むのではなく新しく作る。out の map は base の設定が持っているものと
	// 同一であり、base は他人の写し — 最後に判明しているファイルの内容か、保留中の
	// 書き込み — だから。その場で編集すると、こちらを組み立てた副作用として、
	// 向こうのファイル観を変えてしまう。
	merged := reflect.MakeMap(out.Type())
	if !out.IsNil() {
		for _, key := range out.MapKeys() {
			merged.SetMapIndex(key, out.MapIndex(key))
		}
	}
	for key, value := range touched {
		merged.SetMapIndex(reflect.ValueOf(key), value)
	}
	out.Set(merged)
}

// captureUnchanged は、2 つの設定が同じソースを組み立てて動かすかどうかを返します。
// 変更がカメラを中断させなければならないかは、これで決まります。
//
// 数に入るのは、選択されているソース自身の設定だけです。Source ツリー全体を比べると、
// ユーザーが後で切り替えるつもりの MJPEG URL を書き入れただけで動いているカメラが
// 再起動され、そのカメラが再接続している間、変更は丸ごと拒否されることになります。
//
// 後から追加されたソース種別は default に落ちて再起動します。それが安全な答えです。
// 既存のソースの中に増えたフィールドは、構造体の比較が捕まえます。
func captureUnchanged(previous, next config.Config) bool {
	if previous.Source.Type != next.Source.Type ||
		previous.Source.MaxFrameSize != next.Source.MaxFrameSize ||
		previous.Transform != next.Transform {
		return false
	}
	switch next.Source.Type {
	case config.SourceUVC:
		return previous.Source.UVC == next.Source.UVC
	case config.SourceSerial:
		return previous.Source.Serial.Port == next.Source.Serial.Port &&
			previous.Source.Serial.Baud == next.Source.Serial.Baud &&
			slices.Equal(previous.Source.Serial.Header, next.Source.Serial.Header)
	case config.SourceMJPEG:
		return previous.Source.MJPEG == next.Source.MJPEG
	default:
		return false
	}
}

// deferRestartRequired は、プロセスの起動時にしか読まれない設定を next から
// 取り除き、取り除いたものの名前を返します。next は書き換えられます。
//
// これらは保存されますが、動作中のブリッジには適用されません。適用したように
// 振る舞えば、値は変わったのに動きは変わらないという食い違いが、次の起動まで
// 残り続けます。たとえばトレイはメニューを一度だけ、その時点の言語が与えたラベルで
// 組み立てるので、ui.language を動作中に受け入れても画面上の言葉は一つも変わりません。
//
// 以前はここで拒否していました。変えたのは、拒否が正直である代わりに、GUI から
// 表示言語や PaperTracker 連携を変える手段を一つも残さないからです。保存はする、
// ただし今は効かないと言う方が、ユーザーにとって前へ進める答えになります。名前を
// 返すのは、そう言えるようにするためです。
//
// 名前は TOML のキーそのものです。翻訳しません。ユーザーが設定ファイルを開いた
// ときに探す文字列であり、この画面はそこへ案内するものだからです。
func deferRestartRequired(previous config.Config, next *config.Config) []string {
	var deferred []string
	if previous.Server.Listen != next.Server.Listen {
		next.Server.Listen = previous.Server.Listen
		deferred = append(deferred, "server.listen")
	}
	if previous.Log.Level != next.Log.Level {
		next.Log.Level = previous.Log.Level
		deferred = append(deferred, "log.level")
	}
	if previous.Log.Dir != next.Log.Dir {
		next.Log.Dir = previous.Log.Dir
		deferred = append(deferred, "log.dir")
	}
	if previous.PaperTracker != next.PaperTracker {
		next.PaperTracker = previous.PaperTracker
		deferred = append(deferred, "papertracker")
	}
	if previous.UI != next.UI {
		next.UI = previous.UI
		deferred = append(deferred, "ui.language")
	}
	return deferred
}

// Switch は、稼働中のソース種別だけを変更し、他はそのままにします。
//
// 現在の設定を読み、書き換えた写しを適用するまでが 1 つの排他的な操作です。2 つに
// 分けると、その隙間に入った設定変更が取り消されます。Switch はその変更より前に
// 読んだ設定全体をそのまま適用し、保存してしまうからです。
func (b *Bridge) Switch(ctx context.Context, sourceType string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	cfg := b.cfg
	cfg.Source.Type = sourceType
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return err
	}
	// ソース種別は動作中に変えられるものなので、保留になる葉は生まれない。
	_, err := b.applyLocked(ctx, cfg)
	return err
}

// Devices は、今使えるカメラとシリアルポートを列挙します。
//
// 2 つは独立に集め、そのまま独立に報告します。失敗の仕方が独立しているからです —
// ffmpeg の無い機械にもシリアルポートはあります — し、片方のエラーはもう片方の
// リストを差し止める理由になりません。
//
// 列挙の失敗は「何も見つからなかった」とも違いますし、どちらだったかをユーザーに
// 伝えられるのは呼び出し側だけです。ここで飲み込むと、トレイと API は、その機械に
// カメラが 1 台も無いかのように空のリストを見せることになります。
func (b *Bridge) Devices(ctx context.Context) server.Devices {
	var devices server.Devices

	cameras, err := source.ListDevices(ctx, b.Snapshot().Source.UVC.FFmpegPath)
	if err != nil {
		b.log.Warn("could not list capture devices", "error", err)
		devices.CameraError = err.Error()
	}
	devices.Cameras = cameras

	ports, err := source.ListSerialPorts()
	if err != nil {
		b.log.Warn("could not list serial ports", "error", err)
		devices.SerialError = err.Error()
	}
	devices.SerialPorts = ports

	return devices
}

// SetPaused はキャプチャを停止または再開します。一時停止はカメラを解放します。
// これは UVC で意味を持ちます。デバイスは排他的で、ブリッジが握っている間
// Baballonia はそれを開けないからです。
func (b *Bridge) SetPaused(paused bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.paused == paused {
		return nil
	}
	b.paused = paused
	b.publishView()
	b.status.SetPaused(paused)

	if paused {
		b.stopLocked()
		b.log.Info("capture paused")
		return nil
	}
	b.log.Info("capture resumed")
	if err := b.startLocked(); err != nil {
		// 再開は、起動と同じくソースが機能するという主張ではない。どちらも、
		// 後から挿されたカメラを拾えるようドライバに再試行させるだけ。代わりに
		// 再開を失敗させると、ブリッジは一時停止のまま残り、一時停止したブリッジは
		// 別のソースを渡せない。この 2 つが揃うと、設定ファイルを編集する以外に
		// 出口の無い袋小路になる。
		//
		// ここで nil を返しても理由は失われない。起動できなかったものは status
		// tracker に載っており、それはトレイが表示し /healthz が答えるものだから。
		b.log.Error("capture resumed but the source could not be started", "error", err)
	}
	return nil
}

// Paused は、キャプチャが今一時停止中かを返します。Snapshot と同じくロックフリーな
// 写しを読むので、遅い設定変更の最中でもトレイは再描画できます。
func (b *Bridge) Paused() bool {
	return b.view.Load().paused
}

// startLocked は、設定されたドライバを組み立てて起動し、あとは背後で再試行させます。
// 起動と再開が求めるのはそれです。サインイン後に挿されたカメラも拾える必要があります。
// 呼び出し側が mu を保持します。
func (b *Bridge) startLocked() error {
	return b.launchLocked(nil)
}

// verifyStartLocked は、設定されたドライバを起動してフレームが届くまで待ち、届か
// なければ何も動いていない状態でエラーを返します。Apply が必要とするのはこれです。
// ソースが機能することを証明するのはフレームだけであり、カメラに届かないドライバは
// 失敗せず再接続するからです。
//
// 待機は ctx が終われば終わります。ctx は変更を求めたリクエストであり、その背後の
// クライアントが居なくなれば、新しいソースが立ち上がったと伝える相手はもういません。
// そのまま進めば、呼び出し側が何も知らされていない設定を保存することになります。
// このコンテキストが区切るのは待機だけで、ソース自体はブリッジ自身のルートコンテキストの
// 上で生き続けます。
func (b *Bridge) verifyStartLocked(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return b.launchLocked(ctx)
}

// launchLocked は、設定されたドライバを組み立てて起動します。verifyCtx が非 nil なら
// 最初に配信されるフレームを待ち、nil ならドライバが動き出した時点で返ります。
// 呼び出し側が mu を保持します。
func (b *Bridge) launchLocked(verifyCtx context.Context) error {
	if b.root == nil || b.paused || b.root.Err() != nil {
		// 何も動かないが、それでも設定はドライバを生み出せるものでなければならない。
		// 一時停止中に無検査で受け入れると、Resume が起動できない設定を保存する
		// ことになり、しかもそれまで動いていた設定は既に失われている。
		//
		// 構築することと動かすことは違うので、これらの設定はどちらに転んでも未検証。
		// フラグをそのままにすると、置き換えられた側の設定が成したことを、こちらの
		// 手柄にしてしまう。
		b.proven = false
		if _, err := b.newSource(); err != nil {
			return err
		}
		// 何も起動していないが、設定は確かに変わった。そしてトレイと /healthz が
		// ソースを読むのはステータスから。放っておくと、置き換えられた側のソースを、
		// そのソースが最後に抱えていたエラーごと名乗り続ける。一時停止中に交換した
		// カメラが、誰かが再開するまで「古いカメラがまだ失敗している」ように見える。
		b.status.SetSource(b.cfg.Source.Type)
		return nil
	}

	drv, err := b.newSource()
	if err != nil {
		// これを再試行するものは無い。設定そのものが使えないので、再接続すべき
		// ドライバが存在しない。記録しなければトレイは "connecting..." と表示し、
		// /healthz はソースが未接続だとしか言わない。どちらも「試している何か」を
		// 描写している。既定の設定に UVC のデバイス名は入っていないので、これは
		// 新しいユーザーが最初に出会うものになる。
		b.proven = false
		b.status.SetSource(b.cfg.Source.Type)
		b.status.Disconnected(b.cfg.Source.Type, err)
		return err
	}

	// 検証が見るのはドライバではなく hub。ドライバは 1 枚解析した時点でフレームを
	// 告げるが、そこと hub の間には変換段があり、そこで落とされ得る — ピクセル上限を
	// 超えた画像や、デコーダが拒む画像。クライアントに何かが見えると言えるのは、
	// hub まで届いたフレームだけ。
	//
	// それを決めるのは pump であり、答えを待つ者が居ようが居まいが同じ仕事をする。
	// 1 枚デコードできるまで何も配信しない。呼び出し側が答えを必要とするとき、それを
	// 受け取るのがここ。
	published := make(chan error, 1)
	var publishedOnce sync.Once
	maxPixels := b.cfg.CoreTransform().MaxPixels
	firstFrame := func(err error) {
		publishedOnce.Do(func() { published <- err })
	}

	ctx, cancel := context.WithCancel(b.root)
	frames := make(chan core.Frame, frameQueueDepth)
	stopped := make(chan struct{})
	// バッファ付きにしておく。誰も待たなくなった失敗を報告するときに、ドライバが
	// ブロックしないようにするため。
	failed := make(chan error, 1)

	b.cancel = cancel
	b.stopped = stopped
	b.died.Store(false)
	b.status.SetSource(drv.Name())

	transform := b.cfg.CoreTransform()
	log := b.log

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		// フレームのチャネルはドライバが所有し、ドライバが閉じる。pump が、途中に
		// あるものを抜き切ってから終われるようにするため。
		defer close(frames)
		// Run が返るのはキャンセルされたときか、再試行では直らない失敗のときだけ
		// なので、ここでのエラーはこのソースが二度とフレームを出さないことを意味する。
		err := drv.Run(ctx, frames)
		if err != nil {
			// コンテキストが何を言おうと記録する。この 2 つは競合するから。失敗を
			// 返したばかりのドライバと、この行が ctx.Err() を読む前にキャンセルする
			// 一時停止とが重なると、背後に何も無いのに設定が proven に見える状態が
			// 残る。これはまさにそれを排除するために存在する。
			//
			// そもそもここで ctx.Err() を読むのは問い方が違う。ドライバは自身の
			// コンテキストがキャンセルされたとき nil を返す — 3 つとも返すし、
			// 「命じられて止まった」と言えることがそのための実装理由 — なので、
			// 非 nil のエラーはキャンセルの失敗ではなくソースの失敗を意味する。
			b.died.Store(true)
		}
		if err != nil && ctx.Err() == nil {
			log.Error("source stopped", "source", drv.Name(), "error", err)
			failed <- err
		}
	}()

	go func() {
		defer wg.Done()
		pump(latestOnly(frames, log), transform, b.hub, log, maxPixels, firstFrame)
	}()

	go func() {
		wg.Wait()
		close(stopped)
	}()

	if verifyCtx == nil {
		b.proven = true
		b.log.Info("source started", "source", drv.Name())
		return nil
	}

	// ここからフレームまでの経路は、どれもフレームが得られないやり方。
	b.proven = false

	timer := time.NewTimer(startVerifyTimeout)
	defer timer.Stop()
	var frameErr error
	select {
	case frameErr = <-published:
	case err := <-failed:
		b.stopLocked()
		return err
	case <-timer.C:
		b.stopLocked()
		return fmt.Errorf("bridge: %s produced no frame within %s", drv.Name(), startVerifyTimeout)
	case <-verifyCtx.Done():
		// 呼び出し側は待つのをやめた。向こうは失敗を報告するので、その背後で
		// 変更を完了させると、動作中のブリッジと設定ファイルが、誰も知らされて
		// いないソースの上に残ることになる。
		b.stopLocked()
		return fmt.Errorf("bridge: %s was still starting when the request ended: %w", drv.Name(), verifyCtx.Err())
	case <-b.root.Done():
		// 停止中であることは、何かが機能する証拠ではない。ここで成功を報告すると、
		// 誰も検証していない設定が保存され、次回の実行はその上で始まる。
		b.stopLocked()
		return errors.New("bridge: shutting down before the new source produced a frame")
	}

	if err := verifyOutcome(drv.Name(), frameErr, verifyCtx.Err(), b.root.Err()); err != nil {
		b.stopLocked()
		return err
	}

	b.proven = true
	b.log.Info("source started", "source", drv.Name())
	return nil
}

// verifyOutcome は、変更を求めたリクエストとブリッジ自身がまだ健在かを踏まえて、
// 届いたフレームにどれだけの価値があるかを述べます。
//
// 上の select と分けてあるのは、select が優先順位を表現できないからです。最初の
// フレームとリクエストの期限が同時に届くと、どちらの case も準備完了になりどちらが
// 選ばれてもおかしくないので、フレームの case を読んだことは「誰も待つのをやめて
// いない」証拠にはなりません。それを額面どおり受け取ると、失敗を告げられた
// クライアントの背後で変更を適用し、設定ファイルに書いてしまいます。フレームを
// 手にしてから改めて問い直すことで、select がどちらを選んだかに関わらず答えが同じに
// なります。
func verifyOutcome(name string, frameErr, requestErr, shutdownErr error) error {
	if frameErr != nil {
		return fmt.Errorf("bridge: %s produced a frame that is not a usable JPEG: %w", name, frameErr)
	}
	if requestErr != nil {
		return fmt.Errorf("bridge: %s started as the request ended, so the change was not kept: %w", name, requestErr)
	}
	if shutdownErr != nil {
		// ブリッジが止まる直前に実力を示した設定も、結局その上で何も動いていない
		// 設定であり、次の起動は未検証のままそれで立ち上がることになる。
		return errors.New("bridge: shutting down as the new source produced its first frame")
	}
	return nil
}

// stopLocked は動作中のドライバをキャンセルし、終了するまで待ちます。次のドライバが
// 開く前に排他的なデバイスが解放されていることを保証するのは、この待機です。
// 呼び出し側が mu を保持します。
func (b *Bridge) stopLocked() {
	if b.cancel == nil {
		return
	}
	b.cancel()
	<-b.stopped
	b.cancel, b.stopped = nil, nil
	b.status.SetSource("")
}

// latestOnly はフレームを転送し、向こう側が忙しい間は最新の 1 枚だけを待たせます。
//
// ドライバは 1 フレーム分の枠を通してフレームを渡し、その枠が埋まっているとブロック
// します。カメラより遅い変換 — 回転や再エンコード — があれば、それは毎フレーム
// 起こります。ブロックしたドライバとは、ソケットやパイプやシリアルポートを読むのを
// やめたドライバのことです。画像は代わりにそちらへ積み上がり、その 1 枚ごとに
// ストリームは実時間からさらに遅れます。hub の「最新フレーム優先」が効くのは変換の
// 後なので、hub はそれらを目にすることさえありません。
//
// ドライバが生み出せる速さで読み、変換が手を付けられなかった分を捨てることが、その
// 行列を作らせない方法です。ここでフレームを落とすのは意図した答えです。口の動きを
// 追う用途では、持っている価値があるのは最新の画像だけだからです。
func latestOnly(in <-chan core.Frame, log *slog.Logger) <-chan core.Frame {
	out := make(chan core.Frame, 1)
	go func() {
		defer close(out)
		for frame := range in {
			select {
			case out <- frame:
				continue
			default:
			}
			// 枠には変換がまだ取っていないフレームが入っていて、それはこれより古い。
			select {
			case <-out:
				log.Debug("dropping a frame the transform did not keep up with")
			default:
			}
			select {
			case out <- frame:
			default:
			}
		}
	}()
	return out
}

// pump は任意の変換を適用して各フレームを配信し、hub まで到達したフレームごとに
// onPublish を呼びます。
func pump(frames <-chan core.Frame, transform core.Transform, h *hub.Hub, log *slog.Logger, maxPixels int, firstFrame func(error)) {
	// 1 枚デコードできるまで何も配信しない。ここまでの検査はすべて構造を見るもので、
	// フレームごとの検査に許されるのはそこまで。そして SOI の直後に EOI が来る
	// ペイロードは、JPEG の構造を備えていて中身は空。それを送るソースは最後まで
	// 健全に見える — フレーム数は増え、/healthz は緑、トレイには fps — その間
	// トラッカーは使えるものを何も受け取らない。だから 1 枚がそれを証明するまで、
	// この実行は機能しているとは数えない。
	decoded := false
	// 1 枚もデコードできていない間は、フレームを 1 枚ずつではなく間隔を置いて調べる。
	// ここで高くつくのはデコードであり、使えないものを毎秒 30 枚送ってくる上流に
	// 毎秒 30 回のデコードを払うべきではない。
	var nextCheck time.Time
	// 伝えるのは最初の答えだけ。それがソース切替を待つ呼び出し側の求めたもので
	// あり、後のフレームがデコードできる頃には、その呼び出し側は既にソースの失敗を
	// 告げられている。
	reported := false
	report := func(err error) {
		if !reported {
			reported = true
			firstFrame(err)
		}
	}

	for frame := range frames {
		if !transform.IsNoop() {
			data, err := transform.Apply(frame.Data)
			if err != nil {
				log.Warn("dropping a frame the transform could not handle", "error", err)
				continue
			}
			frame.Data = data
		}

		switch {
		case !decoded:
			// デコードはヘッダーの検査も兼ねており、そして先に来なければならない。
			// 読めるヘッダーを持たないフレームは両方に失敗するが、それを言い表す
			// 答えは「使える画像ではない」の方だから。
			now := time.Now()
			if now.Before(nextCheck) {
				continue
			}
			if err := core.DecodableJPEG(frame.Data, maxPixels); err != nil {
				report(err)
				nextCheck = now.Add(undecodableRecheckInterval)
				log.Warn("dropping a frame that is not a usable image", "error", err)
				continue
			}
			report(nil)
			decoded = true

		case transform.IsNoop():
			// そのまま流すフレームはこちら側でデコードされないので、上限は代わりに
			// ヘッダーへ適用する。デコードするのはクライアントであり、65535x65535 を
			// 宣言する数百バイトは、この上限が拒むために存在するその確保を、
			// クライアントに要求する。ヘッダーを読むだけならキャプチャ速度でも
			// 十分安いが、デコードはそうではない。変換を適用する経路では、デコードの
			// 前に既に同じことを確認している。
			if err := core.WithinPixelLimit(frame.Data, maxPixels); err != nil {
				log.Warn("dropping a frame that declares an image over the pixel limit", "error", err)
				continue
			}
		}
		h.Publish(frame)
	}
}

// newSource は、現在の設定が指すドライバを組み立てます。
func (b *Bridge) newSource() (source.Source, error) {
	cfg := b.cfg
	switch cfg.Source.Type {
	case config.SourceUVC:
		return source.NewUVC(source.UVCConfig{
			Device:       cfg.Source.UVC.Device,
			Size:         cfg.Source.UVC.Size,
			Framerate:    cfg.Source.UVC.Framerate,
			FFmpegPath:   cfg.Source.UVC.FFmpegPath,
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, b.status)

	case config.SourceSerial:
		return source.NewSerial(source.SerialConfig{
			Port:         cfg.Source.Serial.Port,
			Baud:         cfg.Source.Serial.Baud,
			Header:       cfg.SerialHeader(),
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, b.status)

	case config.SourceMJPEG:
		return source.NewMJPEGProxy(source.MJPEGConfig{
			URL:          cfg.Source.MJPEG.URL,
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, b.log, b.status)

	default:
		return nil, fmt.Errorf("bridge: unknown source type %q", cfg.Source.Type)
	}
}
