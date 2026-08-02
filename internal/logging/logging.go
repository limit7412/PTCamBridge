// Package logging sets up the application logger and the rotating file it
// writes to.
//
// Rotation is built in rather than pulled from a dependency: the requirement
// is a fixed size and generation count, and keeping it here preserves the
// cgo-free single-binary build.
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

// Rotation policy: ten megabytes per file, five generations kept.
const (
	DefaultMaxSize    = 10 << 20
	DefaultMaxBackups = 5
	// FileName is the active log file inside the log directory.
	FileName = "paperbridge.log"
)

// Options configures Setup.
type Options struct {
	// Dir receives the log files. An empty Dir logs only to the console.
	Dir string
	// Level is debug, info, warn or error.
	Level string
	// Console also writes to stderr, which is useful when the bridge is
	// started from a terminal rather than as a tray application.
	Console bool
	// MaxSize and MaxBackups override the rotation policy.
	MaxSize    int64
	MaxBackups int
}

// Setup builds the logger. The returned closer flushes and closes the log
// file; it is safe to call even when only console logging is active.
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

// ParseLevel maps a configured level name onto a slog level, falling back to
// info for anything unrecognised.
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

// rotatingWriter appends to a file and rolls it over once it passes maxSize,
// keeping maxBackups older generations as name.1 through name.N.
type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	file       *os.File
	size       int64
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

	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			// Losing rotation is better than losing the log line.
			fmt.Fprintf(os.Stderr, "paperbridge: log rotation failed: %v\n", err)
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate shifts the existing generations up by one and starts a fresh file.
func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil

	// Drop the oldest, then shift each remaining generation up.
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.maxBackups))
	for i := w.maxBackups - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.path, i)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		_ = os.Rename(from, fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		// Reopen regardless so logging keeps working.
		_ = w.open()
		return err
	}
	return w.open()
}

// Close flushes and closes the underlying file.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
