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
	Published     uint64    `json:"published"`
	Dropped       uint64    `json:"dropped"`
	Subscribers   int       `json:"subscribers"`
	InputFPS      float64   `json:"input_fps"`
	LastFrameSize int       `json:"last_frame_size"`
	LastFrameAt   time.Time `json:"last_frame_at"`
}

// Hub は、生産者 1 に対し消費者が多数のフレーム配信器です。
type Hub struct {
	mu        sync.Mutex
	subs      map[uint64]chan core.Frame
	nextID    uint64
	seq       uint64
	latest    core.Frame
	hasLatest bool

	published uint64
	dropped   uint64
	fps       float64
	lastAt    time.Time
}

// New は空の hub を返します。
func New() *Hub {
	return &Hub{subs: make(map[uint64]chan core.Frame)}
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

	for _, ch := range h.subs {
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
			h.dropped++
		default:
		}
		select {
		case ch <- f:
		default:
			h.dropped++
		}
	}
}

// Subscribe は、フレームのチャネルと、購読を解除してそれを閉じる関数を返します。
// 解除関数は冪等ですが、hub がそのクライアントを忘れるためには購読ごとに必ず
// 一度は呼ぶ必要があります。
func (h *Hub) Subscribe() (<-chan core.Frame, func()) {
	ch := make(chan core.Frame, 1)

	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.subs[id] = ch
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

// Subscribers は、接続中のストリームクライアント数を返します。
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Stats は hub のカウンタのスナップショットを返します。
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()

	s := Stats{
		Published:   h.published,
		Dropped:     h.dropped,
		Subscribers: len(h.subs),
		InputFPS:    h.fps,
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
