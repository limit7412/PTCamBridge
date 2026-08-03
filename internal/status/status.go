// Package status は、稼働中のキャプチャソースの健全性を追跡し、/healthz と
// /stats、そしてトレイメニューがそれを報告できるようにします。
package status

import (
	"sync"
	"time"
)

// Snapshot は、tracker の一貫した読み取り結果です。
type Snapshot struct {
	Source    string `json:"source"`
	Connected bool   `json:"connected"`
	LastError string `json:"last_error,omitempty"`
	// LastErrorKey は、LastError が「ユーザーが対処できる数少ない失敗」の
	// いずれかであるとき、そのメッセージを指すキーです。トレイがユーザーの
	// 言語で表示するために使います。それ以外は空で、その場合トレイは文言を
	// そのまま表示します。
	//
	// JSON には出しません。/stats はプログラムが読むものであり、キーにするなら
	// 隣にある英語のテキストの方が安定しています。
	LastErrorKey   string    `json:"-"`
	Reconnects     uint64    `json:"reconnects"`
	ConnectedSince time.Time `json:"connected_since"`
	StartedAt      time.Time `json:"started_at"`
	UptimeSeconds  float64   `json:"uptime_seconds"`
	Paused         bool      `json:"paused"`
}

// Tracker は、ソースの接続状態の遷移を記録します。ドライバが報告に使う
// Reporter インターフェースを満たしており、ドライバ側は HTTP やトレイについて
// 何も知らずに済みます。
type Tracker struct {
	mu             sync.RWMutex
	source         string
	connected      bool
	lastError      string
	lastErrorKey   string
	reconnects     uint64
	connectedSince time.Time
	startedAt      time.Time
	paused         bool

	// classify は、画面側が翻訳すべきエラーに対応するメッセージ名を返します。
	// 注入にしているのは、どの失敗がそれに当たるかを知っているのはドライバ側
	// だからです。このパッケージはトレイとサーバの双方が読む葉であり、そちらを
	// 知るべきではありません。
	classify func(error) string
}

// Option は Tracker を設定します。
type Option func(*Tracker)

// WithErrorKeys は、翻訳する価値のある失敗の名前の付け方を tracker に教えます。
// これが無ければすべてのエラーはその文言のまま報告されますが、それはログが
// 運んでいるものと同じです。
func WithErrorKeys(classify func(error) string) Option {
	return func(t *Tracker) { t.classify = classify }
}

// New は、稼働時間の起点を今にした tracker を返します。
func New(opts ...Option) *Tracker {
	t := &Tracker{startedAt: time.Now()}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// SetSource は、どのドライバが稼働中かを記録し、前のドライバの状態を消します。
// ソースを選択・切り替えたときに呼びます。
func (t *Tracker) SetSource(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = name
	t.connected = false
	t.lastError = ""
	t.lastErrorKey = ""
	t.connectedSince = time.Time{}
}

// Connected は、ソースがフレームを届けている状態として記録します。
func (t *Tracker) Connected(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.connected && t.source == source {
		return
	}
	t.source = source
	t.connected = true
	t.lastError = ""
	t.lastErrorKey = ""
	t.connectedSince = time.Now()
}

// Disconnected は、ソースが落ちている状態として記録します。err が nil なら
// 正常に終了したという意味です。
func (t *Tracker) Disconnected(source string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = source
	if t.connected {
		t.reconnects++
	}
	t.connected = false
	t.connectedSince = time.Time{}
	if err != nil {
		t.lastError = err.Error()
		t.lastErrorKey = ""
		if t.classify != nil {
			t.lastErrorKey = t.classify(err)
		}
	}
}

// SetPaused は、ユーザーがトレイメニューからキャプチャを一時停止したことを
// 記録します。これは障害ではなく意図的な停止です。
func (t *Tracker) SetPaused(paused bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = paused
	if paused {
		t.connected = false
		t.connectedSince = time.Time{}
	}
}

// Paused は、キャプチャが一時停止中かどうかを返します。
func (t *Tracker) Paused() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.paused
}

// Snapshot は現在の状態を返します。
func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return Snapshot{
		Source:         t.source,
		Connected:      t.connected,
		LastError:      t.lastError,
		LastErrorKey:   t.lastErrorKey,
		Reconnects:     t.reconnects,
		ConnectedSince: t.connectedSince,
		StartedAt:      t.startedAt,
		UptimeSeconds:  time.Since(t.startedAt).Seconds(),
		Paused:         t.paused,
	}
}
