// Package output は、hub に集まったフレームを、HTTP ストリーム以外の宛先へ
// 送り出します。
//
// 存在理由は、PaperTracker クライアントの有線トラッカー経路が HTTP を話さない
// ことです。あちらが待っているのはシリアルポートに流れてくる ETVR のパケット列で、
// PTCamBridge が配信している MJPEG-over-HTTP とは伝送そのものが違います。
// ここはその差を埋める側で、internal/source/serial_etvr.go — 同じパケットを
// 読む方 — のちょうど鏡像です。
//
// 実機で使うには、仮想シリアルポートのペア (Windows なら com0com など) を用意し、
// 片方を output.serial.port に、もう片方をクライアントに指定します。ペアの用意は
// このアプリケーションの仕事ではありません。OS の機能であり、他人の COM 番号を
// 勝手に作るべきでもないからです。
package output

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// DefaultBaud は、Babble の有線ファームウェアが動作する速度です。読む側
// (source.DefaultSerialBaud) と同じ値で、意味も同じです。
const DefaultBaud = 3000000

// 再接続のバックオフ。読む側のドライバと同じ形にしてあります。ポートが
// 塞がっている・まだ現れていない、という失敗の仕方が同じだからです。
const (
	backoffMax = 15 * time.Second
	// backoffStable は、待ち時間を初期値に戻すために、1 回の接続が生き延びて
	// いなければならない時間です。
	backoffStable = 30 * time.Second
)

// backoffInitial は最初の待ち時間です。var にしているのはテストのためで、
// 確かめたいのは「開けなかったポートを開き直すか」であって、その間に 1 秒
// 座っていることではありません。
var backoffInitial = 1 * time.Second

// writeStallTimeout は、1 回の Write が返らないまま許される時間です。var なのは、
// テストがこれを使い切らずに済むようにするためです。
//
// これが要るのは、書き込みに期限を設ける手段が他に無いからです。
// go.bug.st/serial の Port が持っているのは読み取りのタイムアウトだけで、
// 相手が読まない仮想ポートへの Write は永久に返らないことがあります。そうなると
// このアプリケーションは黙って止まり、終了することすらできません。閉じれば Write は
// 返るので、閉じる仕事を見張り役の goroutine に持たせます。
var writeStallTimeout = 5 * time.Second

// writeStallCheck は、見張り役が止まった書き込みを探しに来る間隔です。
var writeStallCheck = 250 * time.Millisecond

// AutoPort は、読む側が「ポートを探させる」ために受け付ける値です。書く側は
// これを拒否します。理由は NewSerial を参照してください。
const AutoPort = "auto"

// Frames は、この出力が購読する相手です。hub.Hub がこれを満たします。
type Frames interface {
	Subscribe() (<-chan core.Frame, func())
}

// SerialConfig は、シリアル出力の設定です。
type SerialConfig struct {
	// Port は "COM7" のようなポート名です。空や "auto" は受け付けません。
	Port string
	// Baud は 0 なら DefaultBaud になります。
	Baud int
	// Header はパケットの前置きです。空なら core の既定値
	// (0xFF 0xA0 0xFF 0xA1) を使います。
	Header []byte
	// MaxFrameSize は JPEG 1 枚の上限です。0 なら core の既定値を使います。
	MaxFrameSize int
}

// Stats は、シリアル出力の活動のスナップショットで、/stats が返します。
type Stats struct {
	Port string `json:"port"`
	// Open は、いまポートが開いているかどうかです。
	Open bool `json:"open"`
	// Written と Bytes は、実際にポートへ渡したパケットの数とバイト数です。
	Written uint64 `json:"written"`
	Bytes   uint64 `json:"bytes"`
	// Dropped は、書けずに捨てたフレームの数です。今のところ、ワイヤ形式の
	// 長さフィールドに収まらない大きすぎるフレームだけがここに来ます。
	Dropped uint64 `json:"dropped"`
	// Opens は、ポートを開いた回数です。2 以上なら再接続が起きています。
	Opens uint64 `json:"opens"`
	// LastError は、最後にポートを畳んだ理由です。開いている間は空になりません
	// — 前回の理由はそのまま残します。何度も切れているポートについて、いま
	// 開いているという一点だけを見て「問題は無い」と読まれないためです。
	LastError string `json:"last_error,omitempty"`
}

// serialPort は、この出力が使う serial.Port の部分です。
type serialPort interface {
	Write(p []byte) (int, error)
	Close() error
}

// Serial は、hub のフレームを ETVR のパケットとしてシリアルポートへ書きます。
type Serial struct {
	cfg    SerialConfig
	parser core.ETVRParser
	frames Frames
	log    *slog.Logger

	// openPort は serial.Open で、テストでは差し替えます。書き込みが詰まったとき
	// 何が起きるかは、詰まるポートを与えられなければ確かめられません。
	openPort func(name string, baud int) (serialPort, error)

	// writeStartedAt は、いま走っている Write が始まった時刻 (UnixNano) で、
	// 何も走っていなければ 0 です。見張り役がこれを読んで、返らなくなった
	// 書き込みを見つけます。session を書いているのは常に 1 本の goroutine だけ
	// ですが、見張り役が別の goroutine なので atomic です。
	//
	// 接続をまたいで持ち越されることはありません。0 に戻すのは Write が返った
	// 直後で、それは失敗して返ったときも通ります。だから接続の始めに改めて
	// 消す必要はありません。
	writeStartedAt atomic.Int64

	mu    sync.Mutex
	stats Stats
}

// NewSerial は出力を組み立て、ポート名とパケットヘッダーを先に検証します。
//
// ポートの自動探索は受け付けません。読む側にはあります (source.AutoPort) が、
// 読むことと書くことでは間違えたときの代償が違います。知らないポートを開いて
// 黙って聞いているだけなら、こちらが取り違えたことに気づく機会は残りますが、
// そこへ毎秒何メガバイトも JPEG を流し込めば、相手が何であれ壊しにかかります。
// 実際に踏んだ例として、読む側の自動探索は VR ヘッドセットのポートを掴んで
// いました。書く側で同じことを起こすわけにはいきません。
func NewSerial(cfg SerialConfig, frames Frames, log *slog.Logger) (*Serial, error) {
	if frames == nil {
		return nil, errors.New("output: a frame source is required")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg.Port = strings.TrimSpace(cfg.Port)
	switch {
	case cfg.Port == "":
		return nil, errors.New("output: output.serial.port is empty; name the serial port to write to, for example COM7")
	case strings.EqualFold(cfg.Port, AutoPort):
		return nil, fmt.Errorf("output: output.serial.port cannot be %q; name the port explicitly, because writing a video stream into a port that turns out to belong to another device is not something that can be taken back", AutoPort)
	}
	if cfg.Baud <= 0 {
		cfg.Baud = DefaultBaud
	}
	parser, err := core.NewETVRParser(cfg.Header, cfg.MaxFrameSize)
	if err != nil {
		return nil, fmt.Errorf("output: %w", err)
	}
	return &Serial{
		cfg:    cfg,
		parser: parser,
		frames: frames,
		log:    log,
		stats:  Stats{Port: cfg.Port},
		openPort: func(name string, baud int) (serialPort, error) {
			return serial.Open(name, &serial.Mode{BaudRate: baud})
		},
	}, nil
}

// writeInFlight は、いま Write が走っているかどうかを返します。テストのために
// あります。書き込みが詰まったときの振る舞いは、書き込みが本当に詰まってから
// でないと確かめられず、それを外から知る手段が他にありません。
func (s *Serial) writeInFlight() bool { return s.writeStartedAt.Load() != 0 }

// Stats は、この出力のカウンタのスナップショットを返します。
func (s *Serial) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Run は、ctx がキャンセルされるまでフレームを書き続け、切れたら開き直します。
//
// 購読はループの外で 1 回だけ行います。接続していない間もフレームは届き続けますが、
// hub の枠は 1 フレーム分で、届いた分は古いものを置き換えていくだけです。つまり
// 開き直した瞬間に書き始めるのは、そのとき最新のフレームです。試行のたびに購読を
// し直すと、その代わりに hub の購読者数が上下し、それを見て配信の有無を決めている
// ものに嘘を伝えることになります。
func (s *Serial) Run(ctx context.Context) error {
	ch, cancel := s.frames.Subscribe()
	defer cancel()

	delay := backoffInitial
	for {
		started := time.Now()
		err := s.session(ctx, ch)
		if ctx.Err() != nil {
			return nil
		}
		s.noteClosed(err)
		if err != nil {
			s.log.Warn("the serial output stopped, retrying",
				"port", s.cfg.Port, "error", err, "retry_in", delay)
		}
		if time.Since(started) >= backoffStable {
			delay = backoffInitial
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if delay *= 2; delay > backoffMax {
			delay = backoffMax
		}
	}
}

// session はポートを開き、書けなくなるかコンテキストが終わるまで書き続けます。
func (s *Serial) session(ctx context.Context, ch <-chan core.Frame) error {
	port, err := s.openPort(s.cfg.Port, s.cfg.Baud)
	if err != nil {
		return fmt.Errorf("output: open %s at %d baud: %w", s.cfg.Port, s.cfg.Baud, err)
	}
	closer := &onceCloser{port: port}
	defer closer.Close()

	// 見張り役。ctx が終わるか、1 回の書き込みが返らなくなったらポートを閉じます。
	// 閉じることが、詰まった Write を返させる唯一の手段です (writeStallTimeout)。
	done := make(chan struct{})
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		ticker := time.NewTicker(writeStallCheck)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				closer.Close()
				return
			case <-ticker.C:
				at := s.writeStartedAt.Load()
				if at != 0 && time.Since(time.Unix(0, at)) > writeStallTimeout {
					s.log.Warn("the serial output is not draining, closing the port",
						"port", s.cfg.Port, "blocked_for", writeStallTimeout)
					closer.Close()
					return
				}
			}
		}
	}()
	// 待つのは、次の試行が始まる前にこの見張り役を確実に降ろすためです。降ろさずに
	// おくと、次の接続が開いた後も前の見張り役が最大 writeStallCheck の間残ります。
	// 閉じる相手は前のポートなので実害はありませんが、止まった書き込みを何本の
	// goroutine が見ているのかを、ここ以外の誰にも答えられなくなります。
	defer func() {
		close(done)
		<-watching
	}()

	s.noteOpened()
	s.log.Info("serial output opened", "port", s.cfg.Port, "baud", s.cfg.Baud)

	// 大きすぎるフレームは 1 セッションにつき 1 回だけ warn します。原因が
	// 解像度や品質の設定である以上、次のフレームも、その次も同じ大きさで来ます。
	// 毎フレーム言えば、同じ 1 つの助言でログが埋まります。
	warnedTooLarge := false

	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-ch:
			if !ok {
				return errors.New("output: the frame hub closed this subscription")
			}
			packet, err := s.parser.EncodePacket(frame.Data)
			if err != nil {
				// このフレーム 1 枚の問題であって、ポートの問題ではありません。
				// 閉じて開き直しても同じ大きさのフレームが来るだけなので、
				// 数えて次へ進みます。
				s.noteDropped()
				if !warnedTooLarge {
					warnedTooLarge = true
					s.log.Warn("the frame does not fit in an ETVR packet, so it is being dropped; lower source.uvc.size, or set transform.reencode_quality to shrink the JPEG",
						"port", s.cfg.Port, "frame_size", frame.Size(), "error", err)
				}
				continue
			}

			s.writeStartedAt.Store(time.Now().UnixNano())
			n, err := port.Write(packet)
			s.writeStartedAt.Store(0)
			switch {
			case err != nil:
				// 見張り役がポートを閉じたのなら、Write はそのせいで失敗します。
				// キャンセルによるものなら、上のループがそう扱います。
				return fmt.Errorf("output: write to %s: %w", s.cfg.Port, err)
			case n != len(packet):
				return fmt.Errorf("output: wrote %d of %d bytes to %s", n, len(packet), s.cfg.Port)
			}
			s.noteWritten(n)
		}
	}
}

// noteOpened から noteClosed までが、Stats を動かす唯一の場所です。
func (s *Serial) noteOpened() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Open = true
	s.stats.Opens++
}

// noteClosed は、ポートが閉じた理由を記録します。理由が無い (キャンセル) 場合でも
// 以前の理由は消しません。LastError を参照してください。
func (s *Serial) noteClosed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Open = false
	if err != nil {
		s.stats.LastError = err.Error()
	}
}

func (s *Serial) noteWritten(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Written++
	s.stats.Bytes += uint64(n)
}

func (s *Serial) noteDropped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Dropped++
}

// onceCloser は、ポートを一度だけ閉じます。閉じるのは session と見張り役の両方で、
// どちらが先かは決まっていません。
type onceCloser struct {
	once sync.Once
	port serialPort
}

func (c *onceCloser) Close() {
	c.once.Do(func() { _ = c.port.Close() })
}
