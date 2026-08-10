// Package hub は、唯一のアクティブなソースから届いたフレームを、接続中の
// すべてのストリームクライアントへ配ります。
//
// 配信は「最新フレーム優先」です。購読者はそれぞれ 1 フレーム分の枠を持ち、
// 前のフレームがまだ枠にあるうちに次が届いた場合は置き換えます。口の動きを
// 追う用途では、遅れたクライアントが欲しいのは現在のフレームであって溜まった
// 分ではなく、また遅いクライアントがソースの読み取りループを止めてしまっては
// なりません。
package hub

import (
	"sync"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// fpsAlpha は、入力レートの推定において最新の間隔に与える重みです。
const fpsAlpha = 0.1

// fpsGapReset は、これを超える到着間隔が空いたらレート推定を平滑化ではなく
// 破棄する閾値です。再接続の前後が平均されて途切れがならされるのを防ぎます。
const fpsGapReset = 2 * time.Second

// Stats は hub の活動のスナップショットで、/stats エンドポイントが返します。
type Stats struct {
	Published uint64 `json:"published"`
	// Dropped は、ストリームクライアントが受け取れなかったフレーム数です。
	// ブリッジ自身の出口の取りこぼしは DroppedInternal に分けてあります。理由は
	// subscriber を参照してください。
	Dropped uint64 `json:"dropped"`
	// DroppedInternal は、ブリッジ自身の出口 (SubscribeInternal) が受け取れなかった
	// フレーム数です。シリアル出力が繋がっていない間や、書き込みが詰まっている間に
	// 増えます。HTTP 配信は何も失っていないので、そちらの数と混ぜてはいけません。
	DroppedInternal uint64    `json:"dropped_internal"`
	Subscribers     int       `json:"subscribers"`
	InputFPS        float64   `json:"input_fps"`
	LastFrameSize   int       `json:"last_frame_size"`
	LastFrameAt     time.Time `json:"last_frame_at"`
}

// subscriber は、1 人の購読者への枠と、それが誰であるかです。
//
// internal を分けているのは、この数を読む人が知りたいことが「HTTP のストリームに
// 何人繋がっているか」だからです。ブリッジ自身の出口 (internal/output) も同じ
// 仕組みでフレームを受け取りますが、それはクライアントではありません。混ぜると、
// シリアル出力を有効にした人のトレイと診断画面は、誰も繋いでいないのに常に 1 を
// 表示します。「クライアント数が 0 のままなら HTTP 経路は使われていない」という
// 切り分けは、まさにそこで壊れます。
type subscriber struct {
	ch       chan core.Frame
	internal bool
}

// Hub は、生産者 1 に対し消費者が多数のフレーム配信器です。
type Hub struct {
	mu        sync.Mutex
	subs      map[uint64]subscriber
	nextID    uint64
	seq       uint64
	latest    core.Frame
	hasLatest bool

	published uint64
	dropped   uint64
	// droppedInternal は、ブリッジ自身の出口が取りこぼした分です。分けて数えるのは、
	// 混ぜると「HTTP のクライアントは 1 つも繋がっていないのに破棄が増え続ける」と
	// いう、原因の分からない見え方になるからです。シリアル出力の相手が居ないだけで
	// 起きます。
	droppedInternal uint64
	fps             float64
	lastAt          time.Time
}

// New は空の hub を返します。
func New() *Hub {
	return &Hub{subs: make(map[uint64]subscriber)}
}

// Publish はフレームを配信します。連番は hub が振り、到着時刻も呼び出し側が
// ゼロ値のままにしていれば hub が入れます。Data はこれ以降変更してはいけません。
// スライスはすべての購読者で共有されます。
func (h *Hub) Publish(f core.Frame) {
	now := time.Now()
	if f.RecvedAt.IsZero() {
		f.RecvedAt = now
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.seq++
	f.Seq = h.seq
	h.latest = f
	h.hasLatest = true
	h.published++

	if !h.lastAt.IsZero() {
		gap := f.RecvedAt.Sub(h.lastAt)
		switch {
		case gap > fpsGapReset || gap <= 0:
			h.fps = 0
		case h.fps == 0:
			h.fps = 1 / gap.Seconds()
		default:
			h.fps = h.fps*(1-fpsAlpha) + (1/gap.Seconds())*fpsAlpha
		}
	}
	h.lastAt = f.RecvedAt

	for _, sub := range h.subs {
		ch := sub.ch
		select {
		case ch <- f:
			continue
		default:
		}
		// この購読者がまだ読んでいないフレームが枠を占めている。捨ててもう一度
		// 試す。その間に購読者が取っていれば送信は成功し、本当に詰まっていれば
		// 落とすことになる。
		select {
		case <-ch:
			h.noteDropLocked(sub.internal)
		default:
		}
		select {
		case ch <- f:
		default:
			h.noteDropLocked(sub.internal)
		}
	}
}

// noteDropLocked は、取りこぼしを相手に応じた側へ数えます。mu は保持済みです。
func (h *Hub) noteDropLocked(internal bool) {
	if internal {
		h.droppedInternal++
		return
	}
	h.dropped++
}

// Subscribe は、フレームのチャネルと、購読を解除してそれを閉じる関数を返します。
// 解除関数は冪等ですが、hub がそのクライアントを忘れるためには購読ごとに必ず
// 一度は呼ぶ必要があります。
//
// これで購読したものは Subscribers に数えられます。外から繋いできたストリーム
// クライアントのためのものです。
func (h *Hub) Subscribe() (<-chan core.Frame, func()) {
	return h.subscribe(false)
}

// SubscribeInternal は Subscribe と同じですが、Subscribers には数えません。
// ブリッジ自身の出口のためのものです。理由は subscriber を参照してください。
func (h *Hub) SubscribeInternal() (<-chan core.Frame, func()) {
	return h.subscribe(true)
}

func (h *Hub) subscribe(internal bool) (<-chan core.Frame, func()) {
	ch := make(chan core.Frame, 1)

	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.subs[id] = subscriber{ch: ch, internal: internal}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(ch)
			}
		})
	}
	return ch, cancel
}

// Latest は、直近に配信されたフレームを返します (何も配信されていなければ
// 2 つ目の戻り値が false になります)。
func (h *Hub) Latest() (core.Frame, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.latest, h.hasLatest
}

// Subscribers は、接続中のストリームクライアント数を返します。ブリッジ自身の
// 出口 (SubscribeInternal) は入りません。
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.countLocked(false)
}

// InternalSubscribers は、ブリッジ自身の出口がいくつ購読しているかを返します。
func (h *Hub) InternalSubscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.countLocked(true)
}

// countLocked は、内部かどうかで数えます。呼び出し側が mu を保持します。
func (h *Hub) countLocked(internal bool) int {
	n := 0
	for _, sub := range h.subs {
		if sub.internal == internal {
			n++
		}
	}
	return n
}

// Stats は hub のカウンタのスナップショットを返します。
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()

	s := Stats{
		Published:       h.published,
		Dropped:         h.dropped,
		DroppedInternal: h.droppedInternal,
		Subscribers:     h.countLocked(false),
		InputFPS:        h.fps,
	}
	if h.hasLatest {
		s.LastFrameSize = h.latest.Size()
		s.LastFrameAt = h.latest.RecvedAt
	}
	// レート推定に意味があるのは、フレームが届き続けている間だけ。
	if !h.lastAt.IsZero() && time.Since(h.lastAt) > fpsGapReset {
		s.InputFPS = 0
	}
	return s
}
