// Package source は、カメラからフレームを引いてくる命令的な外殻のドライバ群を
// 収めます。ffmpeg の子プロセス越しの UVC デバイス、シリアルポート越しの有線
// Babble ボード、あるいは既存の MJPEG-over-HTTP ストリームです。
//
// どのドライバも自力で再接続します。ドライバの Run が返るのは、コンテキストが
// キャンセルされたときか、設定が使い物にならないときだけです。そのため、抜き差し
// されたカメラはブリッジを再起動せずに復帰します。
package source

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// 再接続のバックオフ。FR-5 に従う。
const (
	backoffInitial = 1 * time.Second
	backoffMax     = 15 * time.Second
	// backoffStable は、待ち時間を「回復した」とみなして初期値に戻すために、
	// 試行が生き延びていなければならない時間です。
	backoffStable = 30 * time.Second
)

// Source は、コンテキストがキャンセルされるまでフレームを生み出します。
type Source interface {
	// Run は out へフレームを流します。再接続は自身で面倒を見ます。返るのは
	// キャンセルされたときか、再試行では直らない設定の誤りのときだけです。
	Run(ctx context.Context, out chan<- core.Frame) error
	// Name は、ログと状態エンドポイントに出るドライバ名です。
	Name() string
}

// Reporter は、ドライバから接続状態の遷移を受け取ります。ブリッジがこれを実装して
// /healthz と /stats を支えており、ドライバはそのどちらも知りません。
type Reporter interface {
	Connected(source string)
	Disconnected(source string, err error)
}

// NopReporter は状態の遷移を捨てます。
type NopReporter struct{}

func (NopReporter) Connected(string)           {}
func (NopReporter) Disconnected(string, error) {}

// splitFunc は、溜まったバイトバッファから完結したフレームを切り出し、まだ消費
// できなかったバイト列を返します。
type splitFunc func(buf []byte, maxSize int) (frames [][]byte, rest []byte)

// frameAssembler は読み取りを溜め込み、その中で完結したフレームを返します。
// ドライバは自分の作業バッファに読み込んでここへ渡します。読み取りをまたいだ
// 未完成フレームの持ち越しは assembler が受け持ちます。
type frameAssembler struct {
	buf     []byte
	maxSize int
	split   splitFunc
}

func newFrameAssembler(split splitFunc, maxSize int) *frameAssembler {
	if maxSize <= 0 {
		maxSize = core.DefaultMaxFrameSize
	}
	return &frameAssembler{maxSize: maxSize, split: split}
}

// feed は chunk を追加し、それによって完結したフレームを返します。返したフレームの
// 所有権は呼び出し側にあります。内部バッファはその場で詰め直しますが、rest が同じ
// 配列の後方を指しているだけなので安全です。
func (a *frameAssembler) feed(chunk []byte) [][]byte {
	a.buf = append(a.buf, chunk...)
	frames, rest := a.split(a.buf, a.maxSize)
	a.buf = append(a.buf[:0], rest...)
	return frames
}

// reset は作りかけのフレームを捨てます。再接続の後に使います。
func (a *frameAssembler) reset() { a.buf = a.buf[:0] }

// send は、キャンセルを尊重しつつフレームをパイプラインへ渡します。受け側の
// チャネルは浅く、その先の hub は決してブロックしないので、ここで待つのは変換の
// 段階だけです。
func send(ctx context.Context, out chan<- core.Frame, data []byte) error {
	select {
	case out <- core.Frame{Data: data, RecvedAt: time.Now()}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runWithBackoff は、ctx がキャンセルされるまで attempt を繰り返し実行します。
// 失敗の間隔は 1 秒、2 秒、4 秒…と延び、上限は 15 秒です。backoffStable の間
// 持ちこたえた試行は待ち時間を初期値に戻すので、たまの切断のせいで長時間動いて
// いるソースが最大の待ち時間に張り付いたままになることはありません。
func runWithBackoff(ctx context.Context, log *slog.Logger, name string, reporter Reporter, attempt func(context.Context) error) error {
	if reporter == nil {
		reporter = NopReporter{}
	}
	delay := backoffInitial
	for {
		started := time.Now()
		err := attempt(ctx)
		if ctx.Err() != nil {
			return nil
		}
		reporter.Disconnected(name, err)

		var fatal *FatalError
		if errors.As(err, &fatal) {
			log.Error("source cannot start", "source", name, "error", err)
			return err
		}
		if time.Since(started) >= backoffStable {
			delay = backoffInitial
		}
		if err != nil {
			log.Warn("source disconnected, retrying", "source", name, "error", err, "retry_in", delay)
		} else {
			log.Info("source ended, retrying", "source", name, "retry_in", delay)
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

// FatalError は、解釈できない設定値のように、再試行では直らない失敗を表します。
// ドライバは再試行ループを止めるためにこれを返します。
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }

func fatalf(err error) error { return &FatalError{Err: err} }
