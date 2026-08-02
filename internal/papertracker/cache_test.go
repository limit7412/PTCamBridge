package papertracker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// Restoring runs against whichever folder the search returns, and more than one
// PaperTracker can sit on a machine -- an old copy beside a new one. The one
// that matters is the one the bridge wrote to; picking the first that merely
// looks like an install reports "nothing to restore" while the client that was
// really changed stays pointed at a bridge that is no longer running.
func TestFindRestoreDirPrefersTheFolderTheBridgeWroteTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Searched first, and a perfectly ordinary install -- but untouched.
	untouched := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(untouched, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(CachePath(untouched), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Searched later, and the one the bridge actually took over.
	changed := filepath.Join(home, ".local", "share", "PaperTracker")
	if err := os.MkdirAll(changed, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(CachePath(changed), []byte("192.168.1.60"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteCache(changed, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	if got, err := FindInstallDir(); err != nil || got != untouched {
		t.Fatalf("FindInstallDir() = %q, %v -- the fixture does not reproduce the ambiguity", got, err)
	}
	got, err := FindRestoreDir()
	if err != nil {
		t.Fatalf("FindRestoreDir: %v", err)
	}
	if got != changed {
		t.Errorf("FindRestoreDir() = %q, want the folder holding the backup %q", got, changed)
	}
}

// The marker counts as well: a first run against a client with no cache at all
// leaves only that behind, and it is just as much "the bridge was here".
func TestFindRestoreDirFindsAFolderWithOnlyTheMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := filepath.Join(home, ".local", "share", "PaperTracker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	got, err := FindRestoreDir()
	if err != nil {
		t.Fatalf("FindRestoreDir: %v", err)
	}
	if got != dir {
		t.Errorf("FindRestoreDir() = %q, want %q", got, dir)
	}
}

// Nothing to restore has to be distinguishable from a failure: the search runs
// on every start with write_cache off, on machines the bridge never touched.
func TestFindRestoreDirReportsNoBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	install := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(CachePath(install), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := FindRestoreDir(); !errors.Is(err, ErrNoBackup) {
		t.Errorf("FindRestoreDir error = %v, want ErrNoBackup", err)
	}
}

// The cache itself is replaced, not overwritten in place. os.WriteFile empties
// the file before writing it, so a disk that fills up in between leaves the
// client with no address at all -- and WriteCache's caller only logs the
// failure and carries on, so nothing would put it back.
//
// A hard link is what makes the difference visible: it keeps hold of the file
// that was there, so it still reads as the old address if the new one arrived
// under a different name and was renamed into place, and as the new address if
// the old file was truncated and written over.
func TestWriteCacheReplacesTheCacheRatherThanTruncatingIt(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "held-open")
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	held, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read the link: %v", err)
	}
	if string(held) != original {
		t.Errorf("the file that was there now reads %q: it was written over in place, not replaced", held)
	}
	if got, err := ReadCache(dir); err != nil || got != "127.0.0.1:18080" {
		t.Errorf("ReadCache() = %q, %v, want the bridge address", got, err)
	}
}

// Restoring is the same, and worse if it goes wrong: the backup is removed
// straight afterwards, so a half-written original is all the user would have
// left.
func TestRestoreCacheReplacesTheCacheRatherThanTruncatingIt(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	link := filepath.Join(dir, "held-open")
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}

	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}

	held, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read the link: %v", err)
	}
	if string(held) != "127.0.0.1:18080" {
		t.Errorf("the file that was there now reads %q: it was written over in place, not replaced", held)
	}
	if got, err := ReadCache(dir); err != nil || got != original {
		t.Errorf("ReadCache() = %q, %v, want the camera address back", got, err)
	}
}

// Restoring runs on every start with write_cache off, so it has to be
// impossible to do twice. If the record survives a restore -- its removal
// failing on a locked or read-only file -- the next start would put the
// pre-bridge address back over whatever the client cached since, undoing a
// camera the user chose after they stopped using the bridge.
func TestRestoreCacheIsNotAppliedTwice(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.WriteFile(path, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}

	// Stand in for a removal that failed: the record is back under the name a
	// restore in progress uses, which is the state that removal would leave.
	working := path + BackupSuffix + RestoringSuffix
	if err := os.WriteFile(working, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}
	// The client has moved on to another camera since.
	if err := os.WriteFile(path, []byte("192.168.1.77"), 0o644); err != nil {
		t.Fatalf("write the new address: %v", err)
	}

	err := RestoreCache(dir)
	if err == nil {
		t.Fatal("a restore that already happened was applied again")
	}
	if !errors.Is(err, ErrRestoreInterrupted) {
		t.Errorf("error = %v, want ErrRestoreInterrupted", err)
	}
	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got != "192.168.1.77" {
		t.Errorf("cache = %q, want the address the client chose since", got)
	}
}

// A restore that could not write is a restore that has not happened, so the
// record has to go back where it was. Left taken, the next start would find
// contents that do not match the cache and refuse to guess -- a full disk or a
// locked file would turn into something only a person can finish.
func TestRestoreCachePutsTheRecordBackWhenItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path+BackupSuffix, []byte(original), 0o644); err != nil {
		t.Fatalf("write the backup: %v", err)
	}
	// A directory where the cache should be: the rename onto it cannot succeed,
	// which is the closest thing to a full disk that a test can arrange.
	if err := os.MkdirAll(filepath.Join(path, "in-the-way"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := RestoreCache(dir); err == nil {
		t.Fatal("expected the write to fail")
	}

	backup, err := os.ReadFile(path + BackupSuffix)
	if err != nil {
		t.Fatalf("the record was not put back: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup = %q, want the address it held %q", backup, original)
	}
	if _, err := os.Stat(path + BackupSuffix + RestoringSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the claim was left taken: %v", err)
	}

	// And once the way is clear it restores, without anyone renaming anything.
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("clear the way: %v", err)
	}
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if got, _ := ReadCache(dir); got != original {
		t.Errorf("cache = %q, want the camera address back", got)
	}
}

// The ordinary version of that: the restore finished and only the tidying up
// failed, so the client already holds what the record says. Nothing is left to
// do but remove it, quietly -- this runs on every start.
func TestRestoreCacheCleansUpAfterItself(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	working := path + BackupSuffix + RestoringSuffix
	if err := os.WriteFile(working, []byte(original), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}

	if err := RestoreCache(dir); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("RestoreCache = %v, want ErrNoBackup: there is nothing left to put back", err)
	}
	if _, err := os.Stat(working); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the leftover was not cleaned up: %v", err)
	}
	if got, _ := ReadCache(dir); got != original {
		t.Errorf("cache = %q, want it left alone", got)
	}
}

// An interrupted restore is a restore waiting to happen, not litter. Turning
// write_cache back on takes the cache over again, and the address under the
// working name is still the one to hand back -- so it becomes the record once
// more, instead of being replaced by a copy of the bridge's own address.
func TestWriteCacheReclaimsAnInterruptedRestore(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	working := path + BackupSuffix + RestoringSuffix
	if err := os.WriteFile(working, []byte(original), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	backup, err := os.ReadFile(path + BackupSuffix)
	if err != nil {
		t.Fatalf("read the backup: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup = %q, want the client's own address %q", backup, original)
	}
	if _, err := os.Stat(working); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the working name outlived the reclaim: %v", err)
	}

	// And it restores as usual from there.
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	if got, _ := ReadCache(dir); got != original {
		t.Errorf("cache = %q, want the camera address back", got)
	}
}

// The same for a client that had no cache at all: restoring means removing the
// file, and doing that a second time would delete a cache the client wrote
// after the bridge was done with it.
func TestRestoreCacheDoesNotRemoveACacheWrittenAfterTheRestore(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}

	working := path + NoOriginalSuffix + RestoringSuffix
	if err := os.WriteFile(working, nil, 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}
	// The client has since cached a camera of its own.
	if err := os.WriteFile(path, []byte("192.168.1.77"), 0o644); err != nil {
		t.Fatalf("write the new address: %v", err)
	}

	if err := RestoreCache(dir); !errors.Is(err, ErrRestoreInterrupted) {
		t.Fatalf("RestoreCache = %v, want ErrRestoreInterrupted", err)
	}
	if got, err := ReadCache(dir); err != nil || got != "192.168.1.77" {
		t.Errorf("ReadCache() = %q, %v, want the client's own address left alone", got, err)
	}
}

// A folder in the middle of a restore still has to be found, or the search
// would report that the bridge never touched the machine.
func TestFindRestoreDirFindsAnInterruptedRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := filepath.Join(home, ".local", "share", "PaperTracker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	working := CachePath(dir) + BackupSuffix + RestoringSuffix
	if err := os.WriteFile(working, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}

	got, err := FindRestoreDir()
	if err != nil {
		t.Fatalf("FindRestoreDir: %v", err)
	}
	if got != dir {
		t.Errorf("FindRestoreDir() = %q, want %q", got, dir)
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

// install_dir can change while write_cache is on, and the bridge then holds a
// record in the folder it used to write to as well as the one it writes to now.
// Restoring has to find both, or the client left behind keeps pointing at a
// bridge that has stopped.
func TestFindRestoreDirsFindsEveryFolderTheBridgeWroteTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first := filepath.Join(home, "PaperTracker")
	second := filepath.Join(home, ".local", "share", "PaperTracker")
	// A third folder that looks like an installation but was never touched.
	untouched := filepath.Join(home, ".wine", "drive_c", "PaperTracker")

	for _, dir := range []string{first, second, untouched} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(CachePath(dir), []byte("192.168.1.50"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, dir := range []string{first, second} {
		if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
			t.Fatalf("WriteCache: %v", err)
		}
	}

	dirs, err := FindRestoreDirs()
	if err != nil {
		t.Fatalf("FindRestoreDirs: %v", err)
	}
	if len(dirs) != 2 || !slices.Contains(dirs, first) || !slices.Contains(dirs, second) {
		t.Errorf("FindRestoreDirs() = %q, want exactly %q and %q", dirs, first, second)
	}
}

// install_dir can name a folder the search knows nothing about -- a portable
// copy of the client anywhere the user likes. Once that setting is cleared,
// and deleting the whole [papertracker] section is how someone returns to the
// defaults, the bridge's own note is the only thing left that says where to
// undo the change.
func TestWrittenDirsRecordsFoldersTheSearchCannotFind(t *testing.T) {
	state := t.TempDir()
	portable := filepath.Join(t.TempDir(), "PaperTracker Portable")

	if dirs, err := WrittenDirs(state); err != nil || len(dirs) != 0 {
		t.Fatalf("WrittenDirs() = %q, %v, want nothing recorded yet", dirs, err)
	}
	if err := RememberWrittenDir(state, portable); err != nil {
		t.Fatalf("RememberWrittenDir: %v", err)
	}
	// Recording it again on the next start must not repeat it.
	if err := RememberWrittenDir(state, portable); err != nil {
		t.Fatalf("RememberWrittenDir again: %v", err)
	}

	dirs, err := WrittenDirs(state)
	if err != nil {
		t.Fatalf("WrittenDirs: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != portable {
		t.Errorf("WrittenDirs() = %q, want exactly %q", dirs, portable)
	}

	// A second folder joins it rather than replacing it.
	other := filepath.Join(t.TempDir(), "PaperTracker")
	if err := RememberWrittenDir(state, other); err != nil {
		t.Fatalf("RememberWrittenDir: %v", err)
	}
	dirs, err = WrittenDirs(state)
	if err != nil {
		t.Fatalf("WrittenDirs: %v", err)
	}
	if !slices.Contains(dirs, portable) || !slices.Contains(dirs, other) {
		t.Errorf("WrittenDirs() = %q, want both folders", dirs)
	}
}

// Nothing recorded and nowhere to record are ordinary answers, not failures:
// this runs on every start.
func TestWrittenDirsIgnoresAnEmptyRequest(t *testing.T) {
	if err := RememberWrittenDir("", "/somewhere"); err != nil {
		t.Errorf("RememberWrittenDir with no state directory = %v", err)
	}
	if err := RememberWrittenDir(t.TempDir(), "   "); err != nil {
		t.Errorf("RememberWrittenDir with no install directory = %v", err)
	}
	if dirs, err := WrittenDirs(""); err != nil || dirs != nil {
		t.Errorf("WrittenDirs(\"\") = %q, %v, want nothing", dirs, err)
	}
}
