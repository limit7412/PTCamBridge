// Package papertracker writes the address cache the PaperTracker client falls
// back to when no camera is attached over serial.
//
// The client reads a single line holding a bare address and prefixes it with
// http:// itself, so pointing it at the bridge is a one-line file write.
package papertracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CacheFileName is the client's address cache, which sits beside its
// executable.
const CacheFileName = "wifi_cache.txt"

// BackupSuffix is appended to preserve the address the client had before the
// bridge took over.
//
// The name says PaperBridge in it on purpose. Restoring runs on every start
// once write_cache is off, and the folder is searched for when the settings do
// not name one, so a plain ".bak" would be read back on a machine where the
// bridge had never been enabled -- overwriting whatever the client had with a
// file somebody else left there, and deleting that file afterwards. Only a
// name nothing else would choose can stand for "the bridge put this here".
const BackupSuffix = ".paperbridge-backup"

// NoOriginalSuffix marks that the client had no cached address at all when the
// bridge first wrote one.
//
// Without it the first run leaves no record, so the second run finds the
// bridge's own address sitting there and preserves that as "the original".
// Restoring would then hand the user back the bridge instead of the state they
// started from, and there would be no way to get to "no cache" again.
const NoOriginalSuffix = ".paperbridge-backup.none"

// ErrNotFound means no PaperTracker installation was located.
var ErrNotFound = errors.New("papertracker: no installation directory found")

// CachePath is the cache file inside an installation directory.
func CachePath(installDir string) string {
	return filepath.Join(installDir, CacheFileName)
}

// WriteCache points the client's cache at addr, which must be a bare
// host:port such as "127.0.0.1:18080".
//
// The original file is copied to wifi_cache.txt.bak the first time, and only
// the first time: a backup taken on every run would quickly hold the bridge's
// own address instead of the camera's, which is the value the user would want
// back.
func WriteCache(installDir, addr string) error {
	if strings.TrimSpace(installDir) == "" {
		return errors.New("papertracker: install directory is empty")
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("papertracker: address is empty")
	}
	if strings.Contains(addr, "://") {
		return fmt.Errorf("papertracker: address %q must be a bare host:port, the client adds the scheme", addr)
	}
	info, err := os.Stat(installDir)
	if err != nil {
		return fmt.Errorf("papertracker: install directory %q: %w", installDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("papertracker: install directory %q is not a directory", installDir)
	}

	path := CachePath(installDir)
	if err := backupOnce(path); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(addr), 0o644); err != nil {
		return fmt.Errorf("papertracker: write %s: %w", path, err)
	}
	return nil
}

// backupOnce records the pre-bridge state, once. That is either the original
// file copied to path+BackupSuffix, or the marker saying there was no original.
func backupOnce(path string) error {
	backup := path + BackupSuffix
	marker := path + NoOriginalSuffix

	// Either file means the pre-bridge state is already on disk, and a second
	// pass would only overwrite it with the bridge's own address.
	for _, existing := range []string{backup, marker} {
		if _, err := os.Stat(existing); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("papertracker: stat %s: %w", existing, err)
		}
	}

	original, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// The client had no cached address. Recording that is what makes the
		// state restorable at all.
		if err := writeAtomic(marker, nil); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("papertracker: read %s: %w", path, err)
	}
	// Written to a temporary name and renamed, so the backup only ever exists
	// complete. A half-written one is worse than none: the check above would
	// see it and decide the pre-bridge state was already safe, and restoring
	// would hand back a truncated address.
	if err := writeAtomic(backup, original); err != nil {
		return err
	}
	return nil
}

// writeAtomic writes data to path via a temporary file in the same directory,
// so a reader never sees a partial file under that name.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("papertracker: create a temporary file for %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("papertracker: write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("papertracker: close %s: %w", tmp.Name(), err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("papertracker: chmod %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("papertracker: rename onto %s: %w", path, err)
	}
	return nil
}

// ErrNoBackup means there is nothing to put back: the bridge never wrote this
// cache, or it has already been restored. Callers that restore on every start
// use it to tell that apart from a restore that failed.
var ErrNoBackup = errors.New("papertracker: no backup to restore")

// RestoreCache puts the pre-bridge address back and removes the backup, so a
// user who stops using PaperBridge can return the client to its own camera.
func RestoreCache(installDir string) error {
	path := CachePath(installDir)
	backup := path + BackupSuffix
	marker := path + NoOriginalSuffix

	if _, err := os.Stat(marker); err == nil {
		// There was no cache before the bridge, so putting that back means
		// removing the file rather than writing an empty one.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("papertracker: remove %s: %w", path, err)
		}
		if err := os.Remove(marker); err != nil {
			return fmt.Errorf("papertracker: remove %s: %w", marker, err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("papertracker: stat %s: %w", marker, err)
	}

	original, err := os.ReadFile(backup)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w at %s", ErrNoBackup, backup)
	} else if err != nil {
		return fmt.Errorf("papertracker: read %s: %w", backup, err)
	}
	if err := os.WriteFile(path, original, 0o644); err != nil {
		return fmt.Errorf("papertracker: write %s: %w", path, err)
	}
	if err := os.Remove(backup); err != nil {
		return fmt.Errorf("papertracker: remove %s: %w", backup, err)
	}
	return nil
}

// ReadCache returns the address currently cached by the client.
func ReadCache(installDir string) (string, error) {
	data, err := os.ReadFile(CachePath(installDir))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// FindInstallDir looks for a PaperTracker installation in the usual places.
// It is best effort: the user can always set papertracker.install_dir instead.
func FindInstallDir() (string, error) {
	for _, dir := range candidateDirs() {
		if dir == "" {
			continue
		}
		if looksLikeInstall(dir) {
			return dir, nil
		}
	}
	return "", ErrNotFound
}

// looksLikeInstall reports whether dir holds something recognisably
// PaperTracker: either the client executable or an address cache it wrote.
func looksLikeInstall(dir string) bool {
	for _, marker := range []string{"PaperTracker.exe", "paperTracker.exe", CacheFileName} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}
