// Package logging は、アプリケーションのロガーと、その書き込み先であるローテー
// ションするファイルを用意します。
//
// ローテーションは依存に頼らず自前で持っています。要件は固定のサイズと世代数だけ
// であり、ここに置いておけば cgo 無しの単一バイナリという構成を保てます。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ローテーションの方針。1 ファイル 10 メガバイト、5 世代を保持。
const (
	DefaultMaxSize    = 10 << 20
	DefaultMaxBackups = 5
	// FileName は、ログディレクトリ内で現在書き込み中のログファイルです。
	FileName = "ptcambridge.log"
)

// Options は Setup を設定します。
type Options struct {
	// Dir はログファイルの置き場所です。空ならコンソールにだけ出します。
	Dir string
	// Level は debug・info・warn・error のいずれかです。
	Level string
	// Console は標準エラー出力にも書きます。トレイアプリとしてではなく
	// 端末から起動したときに役立ちます。
	Console bool
	// MaxSize と MaxBackups はローテーションの方針を上書きします。
	MaxSize    int64
	MaxBackups int
}

// Setup はロガーを組み立てます。返される closer はログファイルを flush して
// 閉じます。コンソール出力だけの場合に呼んでも安全です。
func Setup(opts Options) (*slog.Logger, io.Closer, error) {
	var writers []io.Writer
	var closer io.Closer = nopCloser{}

	if opts.Console {
		writers = append(writers, os.Stderr)
	}
	if opts.Dir != "" {
		rw, err := newRotatingWriter(filepath.Join(opts.Dir, FileName), opts.MaxSize, opts.MaxBackups)
		if err != nil {
			return nil, nil, err
		}
		writers = append(writers, rw)
		closer = rw
	}
	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}

	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: ParseLevel(opts.Level)})
	return slog.New(handler), closer, nil
}

// ParseLevel は、設定されたレベル名を slog のレベルに対応付けます。認識できない
// ものは info にします。
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// rotatingWriter はファイルに追記し、maxSize を超えたら切り替えます。古い世代は
// name.1 から name.N として maxBackups 個まで残します。
type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	file       *os.File
	size       int64
	// closed は意図的な終了を表します。書き込みの受付をやめる理由はこれだけです。
	// ファイルが無いというだけなら、直前のローテーションが開き直せなかったという
	// ことであり、それは回復可能です。
	closed bool
}

func newRotatingWriter(path string, maxSize int64, maxBackups int) (*rotatingWriter, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	if maxBackups <= 0 {
		maxBackups = DefaultMaxBackups
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create the log directory: %w", err)
	}
	w := &rotatingWriter{path: path, maxSize: maxSize, maxBackups: maxBackups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open the log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat the log file: %w", err)
	}
	w.file, w.size = f, info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, os.ErrClosed
	}
	if w.file != nil && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			// ローテーションを失う方が、ログ 1 行を失うよりましだ。
			fmt.Fprintf(os.Stderr, "ptcambridge: log rotation failed: %v\n", err)
		}
	}
	if w.file == nil {
		// ローテーションが古いファイルを閉じ、新しいファイルを開けなかった —
		// ディスクが一杯、あるいは Windows でスキャナがファイルを掴んでいる。
		// ここで開き直すことが、その一瞬のせいで以降ずっとログが詰まるのを
		// 防いでいる。それをしなければ、書き込みは以後失敗し続ける。
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate は既存の世代を 1 つずつ繰り上げ、新しいファイルを開始します。
func (w *rotatingWriter) rotate() error {
	// Close が何を返そうとハンドルは消す。どちらにせよ記述子は失われており、
	// 残しておくと以降の書き込みが Write の開き直し経路に入らず、閉じた
	// ファイルへ向かってしまう。
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return err
	}

	// 最古を捨て、残りの世代を 1 つずつ繰り上げる。
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.maxBackups))
	for i := w.maxBackups - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.path, i)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		_ = os.Rename(from, fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		// いずれにせよ開き直し、ログを動かし続ける。
		_ = w.open()
		return err
	}
	return w.open()
}

// Close は下層のファイルを flush して閉じます。
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
