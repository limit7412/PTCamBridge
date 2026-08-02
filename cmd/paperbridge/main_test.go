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
	if err := restoreCache(opts); err != nil {
		t.Fatalf("restoreCache: %v", err)
	}

	if _, err := os.Stat(papertracker.CachePath(changed)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the folder the bridge wrote to was not restored: %v", err)
	}
	if got, err := papertracker.ReadCache(untouched); err != nil || got != "192.168.1.50" {
		t.Errorf("the untouched folder = %q, %v, want it left alone", got, err)
	}
}

// The settings can name a folder the bridge never wrote to: the client is
// reinstalled elsewhere while write_cache is on, and install_dir follows it.
// The record stays behind in the old folder, still pointing that copy of the
// client at a bridge that is about to stop, so "no backup here" cannot be the
// end of it.
func TestRestoreFallsBackWhenTheNamedFolderHasNoBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Where the bridge actually wrote, and where it no longer looks.
	old := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(old), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := papertracker.WriteCache(old, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	// Where the settings point now.
	moved := t.TempDir()
	if err := os.WriteFile(papertracker.CachePath(moved), []byte("192.168.1.60"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir, err := restoreWhereverItWas(moved)
	if err != nil {
		t.Fatalf("restoreWhereverItWas: %v", err)
	}
	if dir != old {
		t.Errorf("restored in %q, want the folder holding the backup %q", dir, old)
	}
	if got, err := papertracker.ReadCache(old); err != nil || got != "192.168.1.50" {
		t.Errorf("the folder the bridge wrote to = %q, %v, want the camera address back", got, err)
	}
	if got, err := papertracker.ReadCache(moved); err != nil || got != "192.168.1.60" {
		t.Errorf("the folder the settings name = %q, %v, want it left alone", got, err)
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
	if _, err := restoreWhereverItWas(""); !errors.Is(err, papertracker.ErrNoBackup) {
		t.Fatalf("restoreWhereverItWas error = %v, want ErrNoBackup", err)
	}
	if err := restoreCache(opts); err != nil {
		t.Errorf("restoreCache = %v, want it to report that there is nothing to restore", err)
	}
}
