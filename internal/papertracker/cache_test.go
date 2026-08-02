package papertracker

import (
	"errors"
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

// "Nothing was ever changed here" has to be distinguishable from "the restore
// failed": the bridge restores on every start with write_cache off, and would
// otherwise log an error each time on a machine it never touched.
func TestRestoreCacheWithoutABackup(t *testing.T) {
	err := RestoreCache(t.TempDir())
	if err == nil {
		t.Fatal("expected an error when there is no backup to restore")
	}
	if !errors.Is(err, ErrNoBackup) {
		t.Errorf("error = %v, want it to wrap ErrNoBackup", err)
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

// The first run has nothing to preserve, but it still has to record that --
// otherwise the second run backs up the bridge's own address and calls it the
// client's, and the pre-bridge state is gone for good.
func TestWriteCacheRecordsThatThereWasNoOriginal(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("first WriteCache: %v", err)
	}
	if _, err := os.Stat(path + NoOriginalSuffix); err != nil {
		t.Fatalf("no marker after the first run: %v", err)
	}
	if _, err := os.Stat(path + BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a backup was taken when there was no original: %v", err)
	}

	// A second run must not now treat the bridge's own address as the client's.
	if err := WriteCache(dir, "127.0.0.1:18081"); err != nil {
		t.Fatalf("second WriteCache: %v", err)
	}
	if _, err := os.Stat(path + BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the second run backed up the bridge's own address: %v", err)
	}

	// Restoring "no cache" means removing the file, not leaving an address.
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		data, _ := os.ReadFile(path)
		t.Errorf("the cache still exists after restoring (%q), want it gone", data)
	}
	if _, err := os.Stat(path + NoOriginalSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("the marker outlived the restore")
	}
}

// The ordinary case still has to work: a real original is preserved and put
// back verbatim.
func TestWriteCachePreservesARealOriginal(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.WriteFile(path, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if _, err := os.Stat(path + NoOriginalSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("the no-original marker was written despite a real original")
	}
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got != "192.168.1.50" {
		t.Errorf("restored %q, want the camera address back", got)
	}
}

// Restoring runs on every start once write_cache is off, and the folder is
// searched for when the settings do not name one. A backup named so generally
// that anything could have written it would be read back on a machine where
// the bridge was never enabled -- replacing the client's cache with a stranger's
// file, and deleting that file on the way out.
func TestRestoreCacheIgnoresABackupTheBridgeDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	cache := CachePath(dir)
	foreign := cache + ".bak"

	if err := os.WriteFile(cache, []byte("192.168.1.50:80"), 0o644); err != nil {
		t.Fatalf("write the cache: %v", err)
	}
	// Somebody else's backup, sitting in the same folder.
	if err := os.WriteFile(foreign, []byte("something else entirely"), 0o644); err != nil {
		t.Fatalf("write the foreign backup: %v", err)
	}

	if err := RestoreCache(dir); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("RestoreCache = %v, want ErrNoBackup: the bridge wrote nothing here", err)
	}

	got, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("read the cache back: %v", err)
	}
	if string(got) != "192.168.1.50:80" {
		t.Errorf("cache = %q, want it untouched", got)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("the foreign backup was removed: %v", err)
	}
}

// A backup only counts once it is complete. Half of one is worse than none:
// backupOnce would see it and decide the pre-bridge address was already safe,
// so the real one would be overwritten and only a truncated copy left to
// restore.
func TestBackupIsNeverVisibleHalfWritten(t *testing.T) {
	dir := t.TempDir()
	cache := CachePath(dir)
	original := "192.168.1.50:80"
	if err := os.WriteFile(cache, []byte(original), 0o644); err != nil {
		t.Fatalf("write the cache: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	// Nothing under a temporary name is left lying about, and the backup that
	// is there holds the whole address.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	want := map[string]bool{CacheFileName: true, CacheFileName + BackupSuffix: true}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}

	backup, err := os.ReadFile(cache + BackupSuffix)
	if err != nil {
		t.Fatalf("read the backup: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup = %q, want the whole original address %q", backup, original)
	}
}
