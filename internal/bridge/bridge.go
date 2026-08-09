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
	"runtime"
	"slices"
	"strings"
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

	// startup は、このプロセスが実際に使っている、起動時にしか読まれない設定です。
	// 動かしません。動かしてしまうと、何が「まだ効いていない」のかを言う基準が
	// 無くなります。restartDeferred を参照。
	startup config.Config

	// overridden は、起動時に環境変数で上書きされた、起動時にしか読まれない設定の
	// 葉の名前です。SetPersistBase が一度だけ決めます。名前は上書きを重ねた側から
	// 受け取ります。設定値の差から数えると、ファイルと同じ値を指定した上書きが
	// 見えません。
	//
	// mu の外に置いてあります。決まった後は変わらないのに、Apply はソースを検証する
	// 最長 30 秒のあいだ mu を握るので、ロックの下に置くと /ui/state の polling が
	// その間ずっと止まります。しかも handleUIState は状態と統計をロック待ちより先に
	// 読むので、解けた瞬間に 30 秒古い応答がまとめて返り、新しい表示を上書きします。
	overridden atomic.Pointer[[]string]

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

	// modes は、列挙に成功したカメラのモードを、名前 (小文字化したもの) ごとに
	// 憶えたものです。identities は、最後に数えた顔ぶれ — 名前と
	// "@device_pnp_..." のそれぞれから、その 1 台の素性を引きます。listing は
	// 今走っている列挙です。CameraModes を参照。
	//
	// listing の鍵は打たれた名前 (小文字化したもの) です。素性にしないのは、
	// 素性が後から変わるからです — 登録した後に顔ぶれを数え直すと、その鍵では
	// 誰も引けなくなります。同じ 1 台かどうかは findListingLocked が、そのつど
	// 素性に直して見ます。
	modesMu    sync.Mutex
	modes      map[string]modeMemory
	identities map[string][]string
	listing    map[string]*modeLookup
	// listingWG は走っている列挙です。Stop が終わりを待ちます。stopped は、その
	// 待ちが済んだ後です — 以降は新しい列挙を始めません。
	listingWG      sync.WaitGroup
	lookupsStopped bool

	// lifetime は、このアプリケーションが動いている間だけ生きているコンテキスト
	// です (Start が受け取るもの)。列挙はこれの下で走ります — 要求 1 本より長く、
	// プロセスより短く。listModesOnce を参照。
	//
	// root と同じものですが、こちらはロックの外から読めます。mu の下に置くと、
	// 設定変更中の Apply が最長 30 秒それを握るので、モードを訊いた画面がその間
	// 待たされます。
	lifetime atomic.Pointer[context.Context]

	mu sync.Mutex
	// opening は、launchLocked が今まさに開こうとしているカメラの名前です。
	// view を通して公開します。capturing を参照。
	opening string
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

// modeMemory は、1 台のカメラについて憶えたモードと、憶えたときのその個体の
// 素性です。素性は「同じカメラかどうか」を後から言えるようにするためだけにあります。
type modeMemory struct {
	modes    []source.Mode
	identity string
}

// modeLookup は、走っている列挙 1 つと、その答えを待っている側への受け渡しです。
// modes と err は done を閉じる前に書き、閉じた後は読むだけです。
type modeLookup struct {
	done chan struct{}
	// device は、この列挙が訊いている名前です。走っている列挙を探すときは、
	// これを毎回そのときの素性に直して突き合わせます (findListingLocked)。
	// 素性そのものを鍵にすると、登録の後に顔ぶれを数え直しただけで、走っている
	// 列挙を誰も見つけられなくなります。
	device string
	// identity は、この列挙を始めたときにそのカメラが持っていた素性です。
	// rememberModesLocked を参照。
	identity string
	// cancel は、この列挙を諦めさせます。使うのは 2 つの場面だけ — 終了と、
	// 同じカメラをキャプチャのために開くとき。要求 1 本が去っただけでは使いません。
	cancel context.CancelFunc
	// waiting は、この 1 本の答えを待っている呼び出しの数です。modesMu の下で
	// 数えます。
	waiting int
	modes   []source.Mode
	err     error
}

// view は、呼び出し側が読むだけで決して変更しないもののロックフリーな写しです。
type view struct {
	cfg    config.Config
	paused bool
	// opening は、今まさに開こうとしているカメラです。設定そのものと違って、
	// これは検証を通る前から公開します。何を開こうとしているかは、その時点で
	// 確定しているからです。capturing を参照。
	opening string
}

// provenLocked は、現在の設定の背後に動くソースがあるかどうかを返します。起動に
// 成功し、その後自力で止まっていないソースのことです。呼び出し側が mu を保持します。
func (b *Bridge) provenLocked() bool {
	return b.proven && !b.died.Load()
}

// publishView はロックフリーな写しを更新します。呼び出し側が mu を保持します。
func (b *Bridge) publishView() {
	b.view.Store(&view{cfg: b.cfg, paused: b.paused, opening: b.opening})
}

// New は、渡された設定でブリッジを組み立てます。キャプチャを始めるには Start を
// 呼ぶ必要があります。
func New(cfg config.Config, cfgPath string, h *hub.Hub, st *status.Tracker, log *slog.Logger) *Bridge {
	b := &Bridge{hub: h, status: st, log: log, cfgPath: cfgPath, cfg: cfg, persistBase: cfg, startup: cfg}
	b.publishView()
	return b
}

// SetPersistBase は、ファイルが持っているとおりの設定を記録します。保存はこれを
// 土台にします。これが無いと、実効設定がそのまま保存され、一度きりの上書きが、
// 何かが Apply を呼んだ最初の瞬間に恒久的なものになります。
// overridden には、環境変数が実際に指定した葉の名前を渡します
// (config.Config.ApplyEnv が返すもの)。値の差から推測してはいけません。ファイルと
// 同じ値を指定した上書きは差を作りませんが、上書きとしては存在します。
//
// コマンドラインのフラグは渡しません。次の起動には残らないからです。自動起動は
// 実行ファイルと -config しか登録しないので、-log-level debug で一度だけ起動した
// 人が info を保存したら、それは次のサインインで実際に効きます。
func (b *Bridge) SetPersistBase(cfg config.Config, overridden []string) {
	// 控えるのは、起動時にしか読まれない葉だけです。動作中に読み直せる葉の上書きは
	// 保留とは関係がありません。設定画面から変えればその場で効きます。
	var restartOnly []string
	for _, name := range overridden {
		if slices.Contains(restartOnlyLeaves, name) && !slices.Contains(restartOnly, name) {
			restartOnly = append(restartOnly, name)
		}
	}
	b.overridden.Store(&restartOnly)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.persistBase = cfg
}

// Overridden は、起動時に環境変数で上書きされた、起動時にしか読まれない設定の
// 名前を返します。
//
// これらは「次の起動を待っている」ものとは違います。ファイルに何を書いても、
// 次の起動でも同じ上書きが勝つからです。保留として数えると、画面は永遠に起きない
// 変更を毎回知らせることになります。かといって黙るのも違うので、別の名前で返します。
//
// ロックを取りません。画面が毎秒読むものが、ソースの検証を待つ理由はありません。
func (b *Bridge) Overridden() []string {
	names := b.overridden.Load()
	if names == nil {
		return nil
	}
	return slices.Clone(*names)
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
	b.lifetime.Store(&ctx)
	// 起動時は検証しない。まだ挿さっていないカメラは、現れたときに拾えなければ
	// ならず、ドライバを畳んでしまうとそれができなくなる。
	return b.startLocked()
}

// Stop はキャプチャを止め、ドライバがデバイスを解放し終えるまで待ちます。
func (b *Bridge) Stop() {
	b.mu.Lock()
	b.stopLocked()
	b.mu.Unlock()

	// 走っている列挙も止めて、終わるまで待ちます。待たないと、こちらのプロセスが
	// 先に終わり、ffmpeg が残ってカメラを掴んだままになり得ます — Windows の
	// 子プロセスは親と一緒には死にません。カメラを解放しないまま終わることは、
	// このアプリケーションが最もしてはいけないことです。
	b.stopLookups()
}

// stopLookups は、走っている列挙をすべて諦めさせ、終わるまで待ちます。
func (b *Bridge) stopLookups() {
	b.modesMu.Lock()
	// 印を立てるのは待つ前です。待っている間に始まった列挙は、この Wait では
	// 拾えません。拾えないものを止める唯一の方法は、始めさせないことです。
	b.lookupsStopped = true
	for _, call := range b.listing {
		call.cancel()
	}
	b.modesMu.Unlock()
	b.listingWG.Wait()
}

// cancelListing は、名前で指定されたカメラの列挙を諦めさせ、手放すまで待ちます。
//
// キャプチャがそのカメラを開く直前に呼びます。排他的なデバイスなので、両方は
// 開けません。列挙は後からやり直せますが、キャプチャはここで失敗すると設定ごと
// 巻き戻ります — どちらかが譲るなら、譲るのは列挙の側です。
func (b *Bridge) cancelListing(device string) {
	b.modesMu.Lock()
	// 1 本とは限りません。素性が分かる前は、同じ 1 台が名前と "@device_pnp_..."
	// で別々に登録され得ます。1 本だけ止めても、もう 1 本がカメラを掴んだままです。
	running := b.listingsForLocked(device)
	for _, call := range running {
		call.cancel()
	}
	b.modesMu.Unlock()
	for _, call := range running {
		<-call.done
	}
}

// findListingLocked は、そのカメラについて走っている列挙を返します。呼び出し側が
// modesMu を保持します。
//
// 走っている分をそのつど素性に直して見ます。登録のときの素性を鍵にすると、その後
// Devices が顔ぶれを数え直しただけで鍵が変わり、走っている列挙を誰も見つけられ
// なくなります — 2 本目の ffmpeg が同じカメラへ向かい、キャプチャの起動もそれに
// 手放させられなくなります。
func (b *Bridge) findListingLocked(device string) *modeLookup {
	for _, call := range b.listing {
		if b.sameCameraLocked(call.device, device) {
			return call
		}
	}
	return nil
}

// listingsForLocked は、そのカメラについて走っている列挙をすべて返します。
// 呼び出し側が modesMu を保持します。
func (b *Bridge) listingsForLocked(device string) []*modeLookup {
	var found []*modeLookup
	for _, call := range b.listing {
		if b.sameCameraLocked(call.device, device) {
			found = append(found, call)
		}
	}
	return found
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
// 起動時にしか効かない設定も受け入れて保存しますが、この起動の振る舞いは変わりません。
// その名前を返すので、呼び出し側は「保存したが、効くのは次の起動から」と言えます。
// restartDeferred を参照。保存の失敗は config.ErrNotSaved を包んだエラーとして
// 報告します。今変更したものが再起動を越えないことを、呼び出し側が知る必要が
// あるからです。
//
// 落ち着いた設定も一緒に返すのは、名前と設定が同じ瞬間のものでなければならない
// からです。呼び出し側が後から Snapshot を読むと、その隙間に入った別の要求の設定と、
// こちらの要求について数えた名前が並ぶことになります。en で起動して、先の要求が ja、
// 後の要求が en を指定すれば、language=en と pending_restart=["ui.language"] が同時に
// 返り、保留になっていない変更を保留として伝えます。
func (b *Bridge) Apply(ctx context.Context, cfg config.Config) (config.Config, []string, error) {
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return config.Config{}, nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return b.applyLocked(ctx, cfg)
}

// applyLocked は mu を保持済みの Apply です。先に現在の設定を読む必要のある
// 呼び出し側が、読み取り・変更・適用の全体を、間に何も挟ませずに行えるようにする
// ためのものです。cfg の正規化と検証は呼び出し側が済ませています。
func (b *Bridge) applyLocked(ctx context.Context, cfg config.Config) (config.Config, []string, error) {
	// ロック待ちは、別の呼び出し側の検証まるごと分の長さになり得るので、ここへ
	// 辿り着いたリクエストは既に居なくなっているかもしれない。この先で起きることは
	// どれも、そのリクエストの代わりに勝手にやってよいものではない。サーバ設定
	// だけを触る変更は検証に到達しないので、そうしなければ、タイムアウトを告げ
	// られた呼び出し側のために適用され書き出されてしまう。
	if ctx != nil && ctx.Err() != nil {
		return b.cfg, nil, fmt.Errorf("bridge: the request ended before its change was applied: %w", ctx.Err())
	}

	previous := b.cfg
	// 何かを畳む前に読む。ここから先このフラグは「試している設定」を追うことに
	// なるし、置き換えられる側が動いていたかどうかだけが、巻き戻しが成功し得ると
	// 言える根拠だから。
	provenBefore := b.provenLocked()

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
		return b.cfg, nil, errors.New("capture is paused, so a new source cannot be tried: resume first, then change it")
	}

	// 何かを畳む前にエンコーダを組み立てる。使えない boundary のせいで、今動いて
	// いるソースをユーザーから奪うべきではない。
	encoder, err := core.NewMultipartEncoder(cfg.Server.Boundary, cfg.StreamHeaders())
	if err != nil {
		return b.cfg, nil, fmt.Errorf("server.boundary: %w", err)
	}

	if startupOnlyChange(previous, cfg) {
		// 動かしたのは起動時にしか読まれない葉だけ。この起動で触るものは何一つ
		// 無いので、カメラには近づかない。近づくと、壊れたソースを抱えたユーザーが
		// 表示言語もログの行も変えられなくなる。captureAsExpectedLocked は死んだ
		// ドライバを起こし直すためのものだが、ここにはそもそも起こすべき変更が
		// 無いし、検証に失敗すればこの保存ごと巻き戻る。GUI から保存できるように
		// したはずの設定が、カメラが直るまで一つも保存できなくなる。
		b.cfg = cfg
		b.publishView()
	} else if captureUnchanged(previous, cfg) && b.captureAsExpectedLocked() {
		// 動いたのはサーバ側の設定だけなので、カメラには触れない。再起動すれば
		// 何の得も無くストリームが途切れるし、カメラがたまたま再接続中であれば
		// 下の検証が変更を丸ごと拒否してしまう。まさにその状況のための設定である
		// hold_on_source_loss さえも。
		b.cfg = cfg
		b.publishView()
	} else {
		b.stopLocked()
		b.cfg = cfg
		// view はまだ動かさない。この設定は検証を通っておらず、失敗すれば巻き戻る。
		// 先に公開すると、最大 30 秒のあいだ Snapshot が「これから取り消されるかも
		// しれない設定」を返します。それを読んだ管理 API のクライアントは、その値を
		// 土台に次の変更を組み立て、巻き戻ったはずのソースを自分で復活させます。
		// 公開するのは検証を通った後です。ドライバは view ではなく b.cfg を読むので、
		// 起動には差し支えありません。
		if err := b.verifyStartLocked(ctx); err != nil {
			// 要求が途中で終わったのなら、これはソースの失敗ではありません。呼び出し側
			// が去ったか、アプリが終了しているだけで、カメラについては何も分かって
			// いません。ERROR で「ソースを起動できなかった」と書くと、終了のたびに
			// 記録が 2 行残り、後からログを読む人はカメラを疑うことになります。
			// 巻き戻しはどちらでも同じように行います。違うのは何と呼ぶかだけです。
			if requestEnded(err) {
				b.log.Info("the request ended before the new settings could be verified, reverting", "error", err)
			} else {
				b.log.Error("new settings could not start a source, reverting", "error", err)
			}
			b.cfg = previous
			b.publishView()

			// 動作中の設定は元に戻った。書けずに残っている保存はその設定のもので、
			// この要求の成否とは関係が無いので、ここで試せる。試さないと、ファイルが
			// 書けるようになってもカメラが直るまで書けないままになります。保存の
			// やり直しは同じ設定の再送で行うものであり、それがこの経路を通るからです。
			b.flushUnsavedLocked()

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
					return b.cfg, nil, err
				}
				return b.cfg, nil, fmt.Errorf("apply failed (%w) and the previous source could not be restored: %v", err, revertErr)
			}
			return b.cfg, nil, err
		}
		// 検証を通った。ここで初めて外から見える。
		b.publishView()
	}

	if b.stream != nil {
		b.stream.SetStreamOptions(encoder, cfg.Server.HoldOnSourceLoss)
	}

	// saved は、この後ファイルに入る内容です。保存しない場合は要求そのもの。
	saved := cfg
	if b.cfgPath != "" {
		// 変更を重ねる先は、ファイルの最後に判明している内容ではなく、まだ書き
		// 込みを待っているものの方。保存に失敗した後、この 2 つは同じではないし、
		// ファイルを土台にすると先の変更を取り落とす。呼び出し側がエラーを受けて
		// まったく同じ設定を送り直してきた場合も同様で、それは動作中の設定との
		// 差分が無いため、そうしなければ古いファイルを書き直して成功を報告する
		// ことになる。
		base, baseErr := b.saveBaseLocked()
		merged := mergeChanges(base, previous, cfg)
		saved = merged
		err := baseErr
		if err == nil {
			err = config.Save(b.cfgPath, merged)
		}
		if err != nil {
			// 動作中の設定は既に正しいので何も畳まない。呼び出し側には伝える。
			// 変更が一時的なものであると言えるようにするため。保留中の書き込みは
			// 保持しておき、ファイルが再び書けるようになったときに、再試行か次の
			// 変更がそれを書く。
			b.holdUnsavedLocked(base, previous, cfg)
			b.log.Error("settings applied but could not be saved", "path", b.cfgPath, "error", err)
			return b.cfg, restartDeferred(b.startup, saved), fmt.Errorf("%w to %s: %w", config.ErrNotSaved, b.cfgPath, err)
		}
		b.persistBase = merged
		b.unsaved = nil
	}

	// 何が次の起動を待っているかは、保存した内容から数えます。動作中の設定から
	// 数えると、ユーザーが設定ファイルを直接編集した分を見落とします。en で起動した
	// 後に TOML を ja へ書き換え、API からは別の項目だけを変えると、保存の土台は
	// ファイルを読み直すので ja が残るのに、応答は何も待っていないと言うことになります。
	deferred := restartDeferred(b.startup, saved)
	// 起動時に上書きされた葉は、次の起動を待っているのではありません。ファイルに
	// 何を書いても同じ上書きが勝つので、保留として数えると、起きない変更を毎回
	// 知らせることになります。Overridden がそちらを別に伝えます。
	overridden := b.Overridden()
	deferred = slices.DeleteFunc(deferred, func(name string) bool {
		return slices.Contains(overridden, name)
	})
	if len(deferred) > 0 {
		b.log.Info("these settings differ from the ones this process started with, they are only read at startup", "settings", deferred)
	}
	return b.cfg, deferred, nil
}

// flushUnsavedLocked は、書けずに残っている保存をもう一度試します。
//
// 適用そのものが失敗した経路から呼びます。書けなかった保存は、それを生んだ前の変更の
// ものであって、今の要求の成否とは関係がありません。
//
// 失敗しても報告しません。呼び出し側へ返すべきなのはカメラの話で、この要求について
// 関係があるのはそちらです。書けなければ保留のまま残るので、次の保存が拾います。
func (b *Bridge) flushUnsavedLocked() {
	if b.cfgPath == "" || b.unsaved == nil {
		return
	}
	base, err := b.saveBaseLocked()
	if err != nil {
		b.log.Warn("the settings that could not be saved earlier are still waiting", "path", b.cfgPath, "error", err)
		return
	}
	if err := config.Save(b.cfgPath, base); err != nil {
		b.log.Warn("the settings that could not be saved earlier are still waiting", "path", b.cfgPath, "error", err)
		return
	}
	b.persistBase = base
	b.unsaved = nil
	b.log.Info("the settings that could not be saved earlier are now on disk", "path", b.cfgPath)
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

// restartDeferred は、プロセスの起動時にしか読まれない設定のうち、このプロセスが
// 実際に使っているものと食い違っているものの名前を返します。設定そのものには手を
// 触れません。
//
// これらは保存され、設定としても受け入れられますが、動作中の振る舞いは変わりません。
// listen 済みのソケットは動きませんし、ログのハンドラは組み立て済みですし、
// PaperTracker の接続先を書くのは起動時の一度きりですし、トレイはメニューを一度だけ、
// その時点の言語が与えたラベルで組み立てています。名前を返すのは、呼び出し側が
// 「保存はされたが、効くのは次の起動から」と言えるようにするためです。
//
// 以前はここで拒否していました。変えたのは、拒否が正直である代わりに、GUI から
// 表示言語や PaperTracker 連携を変える手段を一つも残さないからです。保存はする、
// ただし今は効かないと言う方が、ユーザーにとって前へ進める答えになります。
//
// 値を古いまま据え置くことも試しましたが、そちらは別の穴を作ります。動作中の設定と
// ファイルの中身が食い違ったまま、その差を覗く手段がどこにも無くなるからです。
// 保存した言語を確かめることも、気が変わって元に戻すこともできません。取り消しの
// 要求は「動作中の値」と同じなので変更として現れず、ファイルに入ったままの値が
// 書き直されるだけになります。だから設定は素直に受け入れ、効いていないという事実を
// 名前で伝えます。これらの葉を動作中に読む場所はどこにもないので、受け入れて困る
// ものもありません。
//
// 比べる相手は直前の設定ではなく、起動時の設定です。直前と比べると、答えが 2 通りに
// 裏返ります。en で起動して ja を保存した後、無関係な項目だけを保存すると、直前も次も
// ja なので何も保留になっていないことになりますが、画面はまだ en です。逆に再起動前に
// en へ取り消すと差が出るので ui.language を保留として返しますが、その値はもう動いて
// います。「効いていない設定の名前」は、動いているものと比べたときにだけ正しくなります。
//
// 起動時の設定はこのプロセスの間ずっと変わりません。だから比較の基準になれます。
//
// 名前は TOML のキーそのものです。翻訳しません。ユーザーが設定ファイルを開いた
// ときに探す文字列だからです。
// startupOnlyChange は、この要求が起動時にしか読まれない葉だけを動かしたかどうかを
// 返します。
//
// 何も動いていない場合は false です。同じ設定をそのまま適用することには意味があり —
// 死んだドライバをもう一度起こす手段がそれです — 素通りさせてはいけません。
//
// 比べる相手は動作中の設定です。restartDeferred が起動時の設定と比べるのとは別の
// 問いだからです。あちらは「何がまだ効いていないか」、こちらは「この要求は動作中の
// 何かに触るか」を訊いています。
func startupOnlyChange(previous, next config.Config) bool {
	if len(restartDeferred(previous, next)) == 0 {
		return false
	}
	// 起動時にしか読まれない葉を previous のものに戻して、残りが同じなら、動いたのは
	// それらだけ。ExtraHeaders と Serial.Header があるので == では比べられません。
	trimmed := next
	trimmed.Server.Listen = previous.Server.Listen
	trimmed.Log = previous.Log
	trimmed.PaperTracker = previous.PaperTracker
	trimmed.UI = previous.UI
	return reflect.DeepEqual(trimmed, previous)
}

// restartOnlyLeaves は、起動時にしか読まれない設定の葉です。restartDeferred が
// 挙げうる名前と、ちょうど同じ集合でなければなりません
// (TestRestartOnlyLeavesMatchWhatCanBeDeferred が見ています)。
//
// 別に持っているのは、上書きの側には差が無いからです。上書きされた葉は
// ApplyEnv と applyFlags が名前で答えるので、そのうちどれが起動時専用かを
// 選ぶには、名前だけで答えられる集合が要ります。
var restartOnlyLeaves = []string{
	"server.listen",
	"log.level",
	"log.dir",
	"papertracker.install_dir",
	"papertracker.write_cache",
	"ui.language",
}

// 名前は葉ごとです。まとめると、上書きされた葉を差し引くときに、隣の正当な保留まで
// 一緒に消えます。papertracker.install_dir を環境変数で上書きしている機械で
// write_cache だけを保存すると、次の起動では確かに write_cache が変わるのに、
// 「papertracker」という 1 つの名前しかなければ、それも上書き済みとして黙ります。
func restartDeferred(startup, next config.Config) []string {
	var deferred []string
	if startup.Server.Listen != next.Server.Listen {
		deferred = append(deferred, "server.listen")
	}
	if startup.Log.Level != next.Log.Level {
		deferred = append(deferred, "log.level")
	}
	if startup.Log.Dir != next.Log.Dir {
		deferred = append(deferred, "log.dir")
	}
	if startup.PaperTracker.InstallDir != next.PaperTracker.InstallDir {
		deferred = append(deferred, "papertracker.install_dir")
	}
	if startup.PaperTracker.WriteCache != next.PaperTracker.WriteCache {
		deferred = append(deferred, "papertracker.write_cache")
	}
	if startup.UI != next.UI {
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
	_, _, err := b.applyLocked(ctx, cfg)
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

	cameras, err := listDevices(ctx, b.Snapshot().Source.UVC.FFmpegPath)
	if err != nil {
		b.log.Warn("could not list capture devices", "error", err)
		devices.CameraError = err.Error()
	} else {
		// 列挙できたときだけ照らし合わせる。失敗した一覧は「1 台も無い」とは
		// 違うので、それを顔ぶれの変化として読むと、ffmpeg が一度でも転んだ
		// 拍子に憶えを捨てることになる。
		b.forgetModesIfCamerasChanged(cameras)
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

// listModes は差し替えられるようにしてあります。本物は Windows でしか答えず
// (source.ListModes を参照)、テストは Windows で走らないので、これが無いと
// CameraModes の憶えと諦めの筋を 1 本も踏めません。
var listModes = source.ListModes

// listDevices も同じ理由で差し替えられるようにしてあります。
var listDevices = source.ListDevices

// exclusiveCameraAccess は、キャプチャ中のカメラをもう一度開けないかどうかです。
//
// Windows だけです。列挙が実際にデバイスを開くのは DirectShow だけで、他の
// プラットフォームの ListModes は何も開かずに空を返します (相当する仕組みが
// 無いことを、エラーではなく無言で表しています。source.ListModes を参照)。
// そこで掴んでいることを理由に断ると、無言のはずの機能が、Windows でしか意味の
// 無い — トレイも一時停止も無い環境の — エラーになります。
//
// var なのはテストのためです。テストは Windows で走りません。
var exclusiveCameraAccess = runtime.GOOS == "windows"

// CameraModes は、1 台のカメラが申告するモードを返します。
//
// 列挙はカメラを開きます。UVC デバイスは排他的なので、ブリッジが今キャプチャして
// いるカメラを訊かれると、その列挙は開けずに失敗します。しかも設定画面でいちばん
// よく訊かれるのは、まさにその「今のカメラ」です。
//
// そこで、うまくいった列挙の答えをカメラ名ごとに憶えておき、開けなかったときは
// それを返します。モードはカメラの持ち物で、こちらの都合では変わらないので、
// 一度得た答えは後からでも正しいままです。キャプチャを止める — 一時停止でも、
// 別のソースへの切り替えでも — とカメラは解放されるので、そこで一度訊けば以降は
// 憶えたもので答えられます。
//
// 憶えが無く、かつ訊かれたのが今キャプチャしているカメラだった場合は、開きに
// 行かずにその場で失敗させます。開けないと分かっているものを開きに行っても、
// 列挙が自前の 15 秒を使い切ってから同じ答えに辿り着くだけです。しかも失敗は
// 憶えないので、画面がカメラ名に触れるたびにそれを払うことになります。
func (b *Bridge) CameraModes(ctx context.Context, device string) ([]source.Mode, error) {
	if exclusiveCameraAccess && b.capturing(device) {
		if modes, ok := b.recallModes(device); ok {
			return modes, nil
		}
		return nil, fmt.Errorf("uvc: PTCamBridge is capturing %s right now and a UVC camera cannot be opened twice, so pause capture from the tray to look at its modes", device)
	}

	modes, err := b.listModesOnce(ctx, device)
	if err == nil {
		return modes, nil
	}
	// 掴んでいないはずのカメラでも、名前の比較は完全ではありません。設定が
	// フレンドリ名を持ち、画面が "@device_pnp_..." で訊けば (あるいはその逆なら)、
	// 同じ 1 台でも別物に見えます。憶えがあるなら、気付けなくても答えは出せます。
	if modes, ok := b.recallModes(device); ok {
		b.log.Debug("could not list the camera modes; answering with what it said earlier", "device", device, "error", err)
		return modes, nil
	}
	return nil, err
}

// listModesOnce は、1 台のカメラについて同時に 1 つだけ列挙を走らせます。
// 同じカメラを訊いている他の呼び出しは、その 1 つの答えを待って共有します。
//
// 画面の側にも同じ抑止がありますが、あちらが知っているのはそのページの中だけです。
// 設定画面を 2 つのタブで開く、あるいは管理 API を並べて叩くと、同じカメラへ
// ffmpeg が 2 本向かいます。排他的なデバイスなので、その 2 本は互いを失敗させ得ます。
// 開くのはこちらなので、抑止もこちらに要ります。
func (b *Bridge) listModesOnce(ctx context.Context, device string) ([]source.Mode, error) {
	b.modesMu.Lock()
	if b.lookupsStopped {
		b.modesMu.Unlock()
		return nil, errors.New("uvc: PTCamBridge is shutting down")
	}
	if call := b.findListingLocked(device); call != nil {
		call.waiting++
		b.modesMu.Unlock()
		return awaitModes(ctx, call)
	}
	// 掴んでいるかどうかを、登録と同じロックの下でもう一度見ます。呼び出し側の
	// 判定はロックの外なので、その後・ここへ来る前に、キャプチャがそのカメラを
	// 予約していることがあります (launchLocked は予約してから cancelListing で
	// modesMu を取ります)。ここで見なければ、その予約をすり抜けた列挙が 1 本
	// 登録され、起動と取り合います。
	if exclusiveCameraAccess && b.openingLocked(device) {
		b.modesMu.Unlock()
		return nil, fmt.Errorf("uvc: PTCamBridge is opening %s right now", device)
	}
	// 列挙は、始めた要求のものではありません。始めたタブが閉じただけで止めると、
	// 同じ答えを待っている他の要求まで巻き添えになります。だから要求の期限からは
	// 切り離し、代わりにこのアプリケーションの生存期間に結びます。終了時に
	// 止まらないと、ffmpeg が残ってカメラを掴んだままになり得ます。
	lifetime := context.Background()
	if lt := b.lifetime.Load(); lt != nil {
		lifetime = *lt
	}
	runCtx, cancel := context.WithCancel(lifetime)

	key := strings.ToLower(device)
	call := &modeLookup{done: make(chan struct{}), device: device, cancel: cancel, identity: b.identityOfLocked(device)}
	if b.listing == nil {
		b.listing = make(map[string]*modeLookup)
	}
	b.listing[key] = call
	// 数えるのは登録と同じロックの下です。解いた後に足すと、その隙間に入った
	// Stop が「走っているものは無い」と見て待ち終え、その後で列挙が始まります。
	b.listingWG.Add(1)
	b.modesMu.Unlock()

	ffmpegPath := b.Snapshot().Source.UVC.FFmpegPath
	go func() {
		defer b.listingWG.Done()
		defer cancel()
		modes, err := listModes(runCtx, ffmpegPath, device)
		b.modesMu.Lock()
		delete(b.listing, key)
		if err == nil {
			changed := b.cameraChangedLocked(device, call.identity)
			// 憶えるのはここです。呼び出し側で憶えると、この列挙を始めたときの
			// 素性が分からなくなります。
			b.rememberModesLocked(device, modes, call.identity)
			if changed {
				// 憶えないだけでは足りません。待っている要求へ成功として返せば、
				// 画面は名前が変わっていないのでそれを受け取り、今そこにいる
				// カメラが持っていない候補を並べ、それ以外を保存できなくします。
				modes, err = nil, fmt.Errorf("uvc: %s changed while its modes were being listed", device)
			}
		}
		b.modesMu.Unlock()
		call.modes, call.err = modes, err
		close(call.done)
	}()

	return awaitModes(ctx, call)
}

// awaitModes は、走っている列挙の答えを待ちます。
//
// 写しを渡すのは、憶えと同じ理由 — 待っていた全員が同じ 1 本を書き換え合わない
// ためです。呼び出し側の要求が先に終わればそちらを返します。列挙は止めません。
func awaitModes(ctx context.Context, call *modeLookup) ([]source.Mode, error) {
	select {
	case <-call.done:
		return slices.Clone(call.modes), call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// capturing は、名前で指定されたカメラを今このブリッジが握っている — あるいは
// まさに開こうとしている — かどうかを返します。一時停止中はカメラを解放している
// ので、握っていないと答えます。
func (b *Bridge) capturing(device string) bool {
	if device == "" {
		return false
	}
	v := b.view.Load()
	// DirectShow のフレンドリ名は大文字小文字を区別しません。
	if v.opening != "" && strings.EqualFold(v.opening, device) {
		// 開いている最中。設定の側はまだ動いていないことがあります — 検証を
		// 通っていない設定を公開しないので (applyLocked を参照)、その窓は最長で
		// startVerifyTimeout です。それを「空いている」と読ませると、その 30 秒の
		// あいだに来た問い合わせがカメラを開きに行き、起動と取り合います。
		return true
	}
	if v.paused || v.cfg.Source.Type != config.SourceUVC {
		return false
	}
	return strings.EqualFold(v.cfg.Source.UVC.Device, device)
}

// sameCameraLocked は、2 つの名前が同じ 1 台を指しているかを返します。
// 呼び出し側が modesMu を保持します。
//
// 同じ 1 台をフレンドリ名でも "@device_pnp_..." でも指せるので、文字列を突き
// 合わせるだけだと、別々の名前で同じカメラを 2 回開きます。画面が代替名で調べて
// いる最中に Apply がフレンドリ名で起動する、という形が実際に起こります。素性は
// 顔ぶれを数えたときに両方から引けるようにしてあります
// (forgetModesIfCamerasChanged を参照)。
//
// フレンドリ名が重複しているときは、その名前は**どの個体でもあり得ます**。
// どれか 1 つに決めると、決めなかった側を「別のカメラ」と答えることになり、
// 動いているカメラへ列挙を向けます。だから候補が 1 つでも重なれば同じ 1 台と
// 見ます — 排他の判定で迷ったときは、衝突する側へ倒します。
//
// 一覧に無い名前は、打たれたまま小文字で突き合わせます。知らないものを勝手に
// 束ねることはできません。
func (b *Bridge) sameCameraLocked(a, c string) bool {
	la, lc := strings.ToLower(a), strings.ToLower(c)
	if la == lc {
		return true
	}
	mine, theirs := b.identities[la], b.identities[lc]
	for _, identity := range mine {
		if slices.Contains(theirs, identity) {
			return true
		}
	}
	return false
}

// identityOfLocked は、その名前が指す個体を 1 つに決められるならそれを返します。
// 決められなければ空を返します — 一覧に無い名前と、フレンドリ名が重複していて
// どの個体か言えない名前です。呼び出し側が modesMu を保持します。
func (b *Bridge) identityOfLocked(device string) string {
	if found := b.identities[strings.ToLower(device)]; len(found) == 1 {
		return found[0]
	}
	return ""
}

// openingLocked は、今まさに開こうとしているカメラかどうかを、素性まで見て
// 答えます。呼び出し側が modesMu を保持します。
//
// capturing は名前を突き合わせるだけなので、設定がフレンドリ名を持ち、画面が
// "@device_pnp_..." で訊いた (あるいはその逆の) ときにすり抜けます。ここは
// 列挙を登録する直前 — すり抜けたものが実際にカメラを開く場所 — なので、
// 素性で見ます。
func (b *Bridge) openingLocked(device string) bool {
	if device == "" {
		return false
	}
	v := b.view.Load()
	if v.opening != "" && b.sameCameraLocked(v.opening, device) {
		return true
	}
	// 既に開き終えたカメラも同じです。capturing はここでも名前しか見ないので、
	// 動いているカメラを代替名で訊かれると「空いている」と答えます。
	if v.paused || v.cfg.Source.Type != config.SourceUVC || v.cfg.Source.UVC.Device == "" {
		return false
	}
	return b.sameCameraLocked(v.cfg.Source.UVC.Device, device)
}

// rememberModesLocked / recallModes は、列挙に成功した答えを憶え、思い出します。
//
// mu ではなく専用のロックの下に置いてあります。設定変更中の Apply は mu をソースの
// 検証のあいだ — 最長で startVerifyTimeout — 握るので、mu の下に置くと、設定画面が
// カメラ名を打っただけで 30 秒待たされます。
//
// started は、その列挙を始めたときにそのカメラが持っていた素性です。戻ってきた
// 時点の素性と違えば、憶えません。調べている 15 秒の間に同じ名前の別機種へ
// 差し替わったということなので、その答えは今そこにあるカメラのものではありません。
// 今の素性を貼ると、後の照合も素通りして、二度と捨てられなくなります。
func (b *Bridge) rememberModesLocked(device string, modes []source.Mode, started string) {
	key := strings.ToLower(device)
	if b.identityOfLocked(device) != started {
		b.log.Debug("the camera it was told about changed while its modes were being listed, not remembering them", "device", device)
		return
	}
	if b.modes == nil {
		b.modes = make(map[string]modeMemory)
	}
	// 素性が空のままのことはあります (一覧より先に訊かれた場合)。次にそのカメラが
	// 一覧に現れたときに埋まります。forgetModesIfCamerasChanged を参照。
	b.modes[key] = modeMemory{modes: slices.Clone(modes), identity: started}
}

// cameraChangedLocked は、その列挙を始めてから、その名前が別の個体を指すように
// なったかを返します。呼び出し側が modesMu を保持します。
//
// 始めた時点で素性を知らなかった (started が空) 場合は「変わった」とは言いません。
// 後から分かったことは変化ではなく、そこで得たモードは、たった今そのカメラが
// 申告したものです。憶えはしません — 同じ 1 台だったと確かめられないので —
// が、訊いた人には返します。
func (b *Bridge) cameraChangedLocked(device, started string) bool {
	return started != "" && b.identityOfLocked(device) != started
}

// forgetModesIfCamerasChanged は、素性の変わったカメラの憶えだけを捨てます。
//
// モードが変わらないのは同じ 1 台についてだけです。憶えの鍵は名前ですが、名前は
// 個体を指しません — "USB Camera" は次に挿した別機種にも付きます。抜き挿しで
// 入れ替わったカメラに、前の機種のモードを勧め続けることになります。
//
// 個体の識別子だけで憶える方法は取っていません。CameraModes が受け取るのは画面が
// 打った名前だけで、そこから個体を引くにはデバイス一覧が要り、一覧は ffmpeg を
// 1 回起動します。顔ぶれは既に — デバイス一覧を読むこの経路で — 手元にあるので、
// 憶えるときにそこから素性を添えます。
//
// 捨てるのは素性が変わったものだけです。顔ぶれ全体で一致を見ると、無関係な
// カメラを 1 台挿しただけで全部消えます。そのとき今キャプチャしているカメラは
// もう調べ直せないので、正しかった答えを二度と出せなくなります。
func (b *Bridge) forgetModesIfCamerasChanged(cameras []source.Device) {
	// 名前でも "@device_pnp_..." でも引けるようにします。画面はどちらでも
	// 訊けるので、憶えの鍵もどちらにもなり得ます。
	//
	// フレンドリ名が重複しているときは、その名前に**両方**を並べます。どれか 1 つ
	// に決めると、決めなかった側を別のカメラと答えることになります。
	identities := make(map[string][]string, len(cameras)*2)
	add := func(name, identity string) {
		key := strings.ToLower(name)
		if !slices.Contains(identities[key], identity) {
			identities[key] = append(identities[key], identity)
		}
	}
	for _, c := range cameras {
		identity := c.Name + "\x00" + c.Alternative
		add(c.Name, identity)
		if c.Alternative != "" {
			add(c.Alternative, identity)
		}
	}

	b.modesMu.Lock()
	defer b.modesMu.Unlock()
	b.identities = identities
	for key, entry := range b.modes {
		now, listed := b.identityOfLocked(key), len(identities[key]) > 0
		switch {
		case entry.identity == "":
			// 素性を知らずに憶えたもの。一覧より先にモードを訊いた場合です。
			// 今の素性を貼ることはできません — 訊いてから数えるまでの間に
			// 同じ名前の別機種へ差し替わっていても、こちらにはそれを言う材料が
			// 無いからです。貼ってしまうと以後の照合も素通りして、他機種の
			// モードを勧め続けます。分からないものは捨てます。
			//
			// 失うものは、ほぼありません。設定画面は最初の一覧を待ってから
			// モードを訊くので (ui_settings.html の devicesListed を参照)、
			// この経路に落ちるのは API を直に叩いた場合だけです。掴んでいない
			// カメラなら訊き直せます。
			b.log.Debug("forgetting modes learned before the cameras were counted", "device", key)
			delete(b.modes, key)
		case !listed:
			// 今は見えないカメラ。見えないことは入れ替わったことではないので、
			// 憶えたままにします。戻ってきたものが別の個体なら、そのとき素性で
			// 分かります。
		case now != entry.identity:
			b.log.Debug("a camera was replaced, forgetting the modes the old one reported", "device", key)
			delete(b.modes, key)
		}
	}
}

// recallModes は、そのカメラについて憶えているモードを返します。
//
// 打たれた名前だけでなく、同じ 1 台を指す別名でも引きます。憶えるのは訊かれた
// 名前ですが、訊く名前は場面ごとに変わります — 一時停止中にフレンドリ名で憶えた
// ものを、キャプチャ中に "@device_pnp_..." で訊かれる、という形が実際に起こります。
// そこで引けないと、掴んでいて調べ直せないカメラについて、答えを持っているのに
// エラーを返すことになります。
func (b *Bridge) recallModes(device string) ([]source.Mode, bool) {
	b.modesMu.Lock()
	defer b.modesMu.Unlock()
	if entry, ok := b.modes[strings.ToLower(device)]; ok {
		return slices.Clone(entry.modes), true
	}
	for key, entry := range b.modes {
		if b.sameCameraLocked(key, device) {
			return slices.Clone(entry.modes), true
		}
	}
	return nil, false
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

	// 開こうとしているカメラを列挙が掴んでいるなら、先に手放させます。排他的な
	// デバイスなので、両方は開けません。ここで衝突すると、モードを見てから保存
	// した人 — つまりこの機能を使った人 — の設定だけが巻き戻ります。
	//
	// 手放させるだけでは足りません。その後に来た問い合わせが、また開きに行きます。
	// 開いている間は、こちらのものだと言い切ります。
	if b.cfg.Source.Type == config.SourceUVC {
		// 先に予約してから手放させます。逆にすると、手放した直後・開く直前に来た
		// 問い合わせが、また同じカメラを開きに行きます。
		b.opening = b.cfg.Source.UVC.Device
		b.publishView()
		defer func() {
			b.opening = ""
			b.publishView()
		}()
		b.cancelListing(b.cfg.Source.UVC.Device)
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
		//
		// 原因を包むのは、これが「中断であって失敗ではない」と呼び出し側が判断
		// できるようにするため。トレイからの切替は Start と同じコンテキストを渡すので、
		// 終了時には verifyCtx.Done() とこの case が同時に準備完了になり、select は
		// どちらを選んでもおかしくない。片方だけが context.Canceled を運んでいると、
		// 正常な終了が半分の確率で ERROR として記録される。
		b.stopLocked()
		return fmt.Errorf("bridge: shutting down before %s produced a frame: %w", drv.Name(), b.root.Err())
	}

	if err := verifyOutcome(drv.Name(), frameErr, verifyCtx.Err(), b.root.Err()); err != nil {
		b.stopLocked()
		return err
	}

	b.proven = true
	b.log.Info("source started", "source", drv.Name())
	return nil
}

// requestEnded は、この失敗が「待っていた相手が居なくなった」ことによるものかどうかを
// 返します。
//
// コンテキストの終わり方は 2 つあり、どちらもカメラについては何も語りません。
// キャンセルは呼び出し側が去ったかアプリが終了した場合、期限切れは呼び出し側が
// 待つ時間を先に決めていた場合です。前者だけを見ていると、期限を付けた
// クライアント — HTTP のタイムアウトはその形 — がソースの失敗として記録されます。
func requestEnded(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
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
		//
		// 上の case と同じ理由で原因を包む。終了の経路がどれも同じ分類になって
		// いなければ、呼び出し側は「中断」と「本当の失敗」を見分けられない。
		return fmt.Errorf("bridge: shutting down as %s produced its first frame: %w", name, shutdownErr)
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
