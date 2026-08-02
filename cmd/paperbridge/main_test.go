package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/limit7412/PTCamBridge/internal/papertracker"
)

// A wildcard bind is a valid thing to listen on and a useless thing to write
// into the client's address cache.
func TestConnectAddress(t *testing.T) {
	cases := map[string]string{
		"[::]:18080":       "127.0.0.1:18080",
		"0.0.0.0:18080":    "127.0.0.1:18080",
		"127.0.0.1:18080":  "127.0.0.1:18080",
		"192.168.1.5:8080": "192.168.1.5:8080",
		"[::1]:18080":      "[::1]:18080",
		"not-an-address":   "not-an-address",
	}
	for addr, want := range cases {
		if got := connectAddress(addr); got != want {
			t.Errorf("connectAddress(%q) = %q, want %q", addr, got, want)
		}
	}
}

// -restore-cache is for uninstalling, when the settings naming the folder are
// usually already gone, so the search decides which client is put back. With
// two installations on the machine it has to be the one the bridge changed,
// not whichever comes first: restoring the other one reports success against a
// folder nothing happened in and leaves the real cache pointing at a bridge
// that is being removed.
func TestRestoreDirPicksTheFolderTheBridgeWroteTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	untouched := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(untouched, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(untouched), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	changed := filepath.Join(home, ".local", "share", "PaperTracker")
	if err := os.MkdirAll(changed, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := papertracker.WriteCache(changed, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	// No settings file: the situation the flag exists for.
	opts := options{configPath: filepath.Join(t.TempDir(), "gone.toml")}
	got, err := restoreDir(opts)
	if err != nil {
		t.Fatalf("restoreDir: %v", err)
	}
	if got != changed {
		t.Errorf("restoreDir() = %q, want the folder holding the backup %q", got, changed)
	}
}

// Nothing anywhere is an ordinary answer, not a failure, and the command says
// so rather than reporting an error.
func TestRestoreCacheSaysThereIsNothingToUndo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	opts := options{configPath: filepath.Join(t.TempDir(), "gone.toml")}
	if _, err := restoreDir(opts); !errors.Is(err, papertracker.ErrNoBackup) {
		t.Fatalf("restoreDir error = %v, want ErrNoBackup", err)
	}
	if err := restoreCache(opts); err != nil {
		t.Errorf("restoreCache = %v, want it to report that there is nothing to restore", err)
	}
}
