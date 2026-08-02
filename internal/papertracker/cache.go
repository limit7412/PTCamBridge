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
const BackupSuffix = ".bak"

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

// backupOnce copies path to path+BackupSuffix unless a backup already exists.
func backupOnce(path string) error {
	backup := path + BackupSuffix
	if _, err := os.Stat(backup); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("papertracker: stat %s: %w", backup, err)
	}

	original, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// Nothing to preserve; the client had no cached address.
		return nil
	} else if err != nil {
		return fmt.Errorf("papertracker: read %s: %w", path, err)
	}
	if err := os.WriteFile(backup, original, 0o644); err != nil {
		return fmt.Errorf("papertracker: write %s: %w", backup, err)
	}
	return nil
}

// RestoreCache puts the pre-bridge address back and removes the backup, so a
// user who stops using PaperBridge can return the client to its own camera.
func RestoreCache(installDir string) error {
	path := CachePath(installDir)
	backup := path + BackupSuffix

	original, err := os.ReadFile(backup)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("papertracker: no backup at %s", backup)
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
