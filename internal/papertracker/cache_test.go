package papertracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCache(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got != "127.0.0.1:18080" {
		t.Errorf("cache holds %q, want the bridge address", got)
	}
}

// The client prefixes the cached value with http:// itself, so a scheme in the
// file would produce http://http://... and silently fail to connect.
func TestWriteCacheRejectsAScheme(t *testing.T) {
	if err := WriteCache(t.TempDir(), "http://127.0.0.1:18080"); err == nil {
		t.Fatal("expected an address with a scheme to be rejected")
	}
}

func TestWriteCacheRejectsABadDirectory(t *testing.T) {
	if err := WriteCache("", "127.0.0.1:1"); err == nil {
		t.Error("expected an empty directory to be rejected")
	}
	if err := WriteCache(filepath.Join(t.TempDir(), "missing"), "127.0.0.1:1"); err == nil {
		t.Error("expected a missing directory to be rejected")
	}

	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteCache(file, "127.0.0.1:1"); err == nil {
		t.Error("expected a file to be rejected as an install directory")
	}
}

// The backup exists to preserve the camera address the client had. Taking it
// again on the second run would overwrite it with the bridge's own address,
// which is exactly the value the user does not want back.
func TestBackupIsTakenOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	original := "192.168.1.50"
	if err := os.WriteFile(CachePath(dir), []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	backup, err := os.ReadFile(CachePath(dir) + BackupSuffix)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup holds %q, want the camera address %q", backup, original)
	}
}

func TestWriteCacheWithNoExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if _, err := os.Stat(CachePath(dir) + BackupSuffix); !os.IsNotExist(err) {
		t.Error("a backup was created even though there was nothing to preserve")
	}
}

func TestRestoreCache(t *testing.T) {
	dir := t.TempDir()
	original := "192.168.1.50"
	if err := os.WriteFile(CachePath(dir), []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got != original {
		t.Errorf("cache holds %q after restore, want %q", got, original)
	}
	if _, err := os.Stat(CachePath(dir) + BackupSuffix); !os.IsNotExist(err) {
		t.Error("the backup should be removed once restored")
	}
}

func TestRestoreCacheWithoutABackup(t *testing.T) {
	if err := RestoreCache(t.TempDir()); err == nil {
		t.Fatal("expected an error when there is no backup to restore")
	}
}

func TestFindInstallDirReportsNotFound(t *testing.T) {
	// The temp home has no PaperTracker in it, so every candidate misses.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	if _, err := FindInstallDir(); err == nil {
		t.Fatal("expected ErrNotFound with no installation present")
	}
}

func TestFindInstallDirRecognisesAnInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	install := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(install, CacheFileName), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := FindInstallDir()
	if err != nil {
		t.Fatalf("FindInstallDir: %v", err)
	}
	if got != install {
		t.Errorf("FindInstallDir() = %q, want %q", got, install)
	}
}
