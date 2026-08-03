package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/limit7412/PTCamBridge/internal/config"
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

	restored, err := restoreEverywhereItWas(moved)
	if err != nil {
		t.Fatalf("restoreEverywhereItWas: %v", err)
	}
	if len(restored) != 1 || restored[0] != old {
		t.Errorf("restored %q, want just the folder holding the backup %q", restored, old)
	}
	if got, err := papertracker.ReadCache(old); err != nil || got != "192.168.1.50" {
		t.Errorf("the folder the bridge wrote to = %q, %v, want the camera address back", got, err)
	}
	if got, err := papertracker.ReadCache(moved); err != nil || got != "192.168.1.60" {
		t.Errorf("the folder the settings name = %q, %v, want it left alone", got, err)
	}
}

// install_dir can be changed while write_cache is on -- the client is moved or
// reinstalled -- and the bridge then leaves a record in both folders. Stopping
// at the first one restored leaves the other client pointing at a bridge that
// is being removed, with nothing left that would ever look again.
func TestRestoreUndoesEveryFolderTheBridgeWroteTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first := filepath.Join(home, "PaperTracker")
	second := filepath.Join(home, ".local", "share", "PaperTracker")
	for dir, address := range map[string]string{first: "192.168.1.50", second: "192.168.1.60"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(papertracker.CachePath(dir), []byte(address), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := papertracker.WriteCache(dir, "127.0.0.1:18080"); err != nil {
			t.Fatalf("WriteCache: %v", err)
		}
	}

	restored, err := restoreEverywhereItWas(second)
	if err != nil {
		t.Fatalf("restoreEverywhereItWas: %v", err)
	}
	if len(restored) != 2 {
		t.Errorf("restored %q, want both folders", restored)
	}
	if got, err := papertracker.ReadCache(first); err != nil || got != "192.168.1.50" {
		t.Errorf("the folder no longer named = %q, %v, want its own camera back", got, err)
	}
	if got, err := papertracker.ReadCache(second); err != nil || got != "192.168.1.60" {
		t.Errorf("the folder the settings name = %q, %v, want its own camera back", got, err)
	}
}

// Undoing what the bridge did to somebody else's application must not depend on
// the whole settings file being valid. Someone uninstalling PTCamBridge with a
// misspelled key or an out-of-range value in it would otherwise be told nothing
// was changed, while the client still points at the bridge they are removing.
func TestRestoreReadsInstallDirFromAnOtherwiseUnusableSettingsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	// Somewhere the search will not look.
	install := filepath.Join(t.TempDir(), "PaperTracker Portable")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(install), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := papertracker.WriteCache(install, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	cfgPath := filepath.Join(t.TempDir(), "ptcambridge.toml")
	settings := "[server]\nlsiten = 'oops'\n\n[papertracker]\ninstall_dir = '" + install + "'\n"
	if err := os.WriteFile(cfgPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("write the settings: %v", err)
	}

	opts := options{configPath: cfgPath}
	if got := configuredInstallDir(opts); got != install {
		t.Fatalf("configuredInstallDir() = %q, want %q even though the file has a bad key", got, install)
	}
	if err := restoreCache(opts); err != nil {
		t.Fatalf("restoreCache: %v", err)
	}
	if got, err := papertracker.ReadCache(install); err != nil || got != "192.168.1.50" {
		t.Errorf("ReadCache() = %q, %v, want the camera address back", got, err)
	}
}

// The folder can be named only by the environment, and a folder the bridge
// writes to is a folder it has to be able to undo. Reading just the file here
// would report "nothing was changed" about the one client that was.
func TestRestoreHonoursTheInstallDirEnvironmentVariable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	// Somewhere the search will not look, named only by the variable.
	install := filepath.Join(t.TempDir(), "PaperTracker Portable")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(install), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := papertracker.WriteCache(install, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	t.Setenv(config.EnvInstallDir, install)

	// No settings file at all, as after an uninstall.
	opts := options{configPath: filepath.Join(t.TempDir(), "gone.toml")}
	if got := configuredInstallDir(opts); got != install {
		t.Fatalf("configuredInstallDir() = %q, want the folder from %s", got, config.EnvInstallDir)
	}
	if err := restoreCache(opts); err != nil {
		t.Fatalf("restoreCache: %v", err)
	}
	if got, err := papertracker.ReadCache(install); err != nil || got != "192.168.1.50" {
		t.Errorf("ReadCache() = %q, %v, want the camera address back", got, err)
	}
}

// Returning to the defaults means deleting the [papertracker] section, which
// takes install_dir with it. If that named a folder the search does not cover,
// nothing else would ever mention it again -- so the bridge keeps its own note
// of where it has written.
func TestRestoreFindsAFolderNoSettingNamesAnyMore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", filepath.Join(home, "config"))

	// A portable copy of the client, somewhere the search will never look.
	portable := filepath.Join(t.TempDir(), "PaperTracker Portable")
	if err := os.MkdirAll(portable, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(portable), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// What a run with write_cache on does.
	if err := papertracker.WriteCache(portable, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if err := rememberWrittenDir(portable); err != nil {
		t.Fatalf("rememberWrittenDir: %v", err)
	}

	// The user then removes the whole [papertracker] section, so nothing names
	// the folder any more.
	restored, err := restoreEverywhereItWas("")
	if err != nil {
		t.Fatalf("restoreEverywhereItWas: %v", err)
	}
	if len(restored) != 1 || restored[0] != portable {
		t.Errorf("restored %q, want the folder the bridge recorded %q", restored, portable)
	}
	if got, err := papertracker.ReadCache(portable); err != nil || got != "192.168.1.50" {
		t.Errorf("ReadCache() = %q, %v, want the camera address back", got, err)
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
	if restored, err := restoreEverywhereItWas(""); err != nil || len(restored) != 0 {
		t.Fatalf("restoreEverywhereItWas() = %q, %v, want nothing to do and no error", restored, err)
	}
	if err := restoreCache(opts); err != nil {
		t.Errorf("restoreCache = %v, want it to report that there is nothing to restore", err)
	}
}
