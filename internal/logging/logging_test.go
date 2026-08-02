package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"INFO":    slog.LevelInfo,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"chatty":  slog.LevelInfo,
	}
	for name, want := range cases {
		if got := ParseLevel(name); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSetupWritesToTheLogFile(t *testing.T) {
	dir := t.TempDir()
	log, closer, err := Setup(Options{Dir: dir, Level: "info"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	log.Info("hello", "answer", 42)
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "hello") || !strings.Contains(string(data), "answer=42") {
		t.Errorf("the log line is missing from %q", data)
	}
}

func TestSetupHonoursTheLevel(t *testing.T) {
	dir := t.TempDir()
	log, closer, err := Setup(Options{Dir: dir, Level: "warn"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	log.Info("quiet")
	log.Warn("loud")
	closer.Close()

	data, _ := os.ReadFile(filepath.Join(dir, FileName))
	if strings.Contains(string(data), "quiet") {
		t.Error("an info line was written at warn level")
	}
	if !strings.Contains(string(data), "loud") {
		t.Error("the warn line is missing")
	}
}

// Rotation keeps the log bounded on a bridge that runs for days.
func TestRotationKeepsBoundedGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	w, err := newRotatingWriter(path, 256, 3)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	line := append(bytes.Repeat([]byte("x"), 100), '\n')
	for i := 0; i < 40; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	// The active file plus at most maxBackups generations.
	if len(entries) > 4 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("kept %d files (%v), want at most 4", len(entries), names)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the active log file is missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("the first generation is missing: %v", err)
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Error("a generation past the limit was kept")
	}
}

// Restarting must append rather than truncate, or a crash loop would erase the
// evidence of the previous run.
func TestRotatingWriterAppendsOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	first, err := newRotatingWriter(path, 1<<20, 3)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	first.Write([]byte("run one\n"))
	first.Close()

	second, err := newRotatingWriter(path, 1<<20, 3)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	second.Write([]byte("run two\n"))
	second.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "run one") || !strings.Contains(string(data), "run two") {
		t.Errorf("both runs should be present, got %q", data)
	}
}

func TestSetupWithoutADirectoryLogsToTheConsoleOnly(t *testing.T) {
	log, closer, err := Setup(Options{Level: "info"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	log.Info("no file needed")
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestWriteAfterCloseDoesNotPanic(t *testing.T) {
	w, err := newRotatingWriter(filepath.Join(t.TempDir(), FileName), 1<<20, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	w.Close()
	if _, err := w.Write([]byte("late\n")); err == nil {
		t.Error("writing to a closed writer should report an error")
	}
	if err := w.Close(); err != nil {
		t.Errorf("closing twice should be safe, got %v", err)
	}
}
