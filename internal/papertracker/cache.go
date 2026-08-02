// Package papertracker writes the address cache the PaperTracker client falls
// back to when no camera is attached over serial.
//
// The client reads a single line holding a bare address and prefixes it with
// http:// itself, so pointing it at the bridge is a one-line file write.
package papertracker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// RestoringSuffix is appended to whichever of the two records is being put
// back, for as long as that is going on.
//
// Restoring is attempted on every start once write_cache is off, so it has to
// be impossible to do twice. The record is moved under this name before the
// cache is written and removed once it is: a removal that fails at the end can
// then leave litter, but never a restore that runs again and puts the
// pre-bridge address back over an address the client has cached since.
//
// A restore interrupted part way is still recognised under this name and
// finished on the next start, because the move happens before the write and so
// a record found here may never have been applied.
const RestoringSuffix = ".restoring"

// AppliedSuffix marks a record whose address is back in the client's cache and
// which is only waiting to be deleted.
//
// Removing the working file is the last step of a restore, and it is the step
// most likely to fail on its own -- a file held open by a scanner, a folder
// that has gone read-only. What is left then looks exactly like a restore that
// stopped before it wrote anything, and the two call for opposite answers: one
// is litter, the other is the client's own address and the only copy of it.
// Renaming rather than guessing is what keeps them apart.
const AppliedSuffix = ".applied"

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
	// Replaced rather than overwritten in place. os.WriteFile truncates first,
	// so a disk that fills up between the two leaves the client with an empty or
	// half-written address -- and the caller only logs the failure and carries
	// on, so nothing would put it right. A rename either happens or does not.
	if err := writeAtomic(path, []byte(addr)); err != nil {
		return err
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
		switch found, err := exists(existing); {
		case err != nil:
			return err
		case found:
			return nil
		}
	}

	// A file left under a restore's working name is one of two things, and they
	// call for opposite answers -- the same question RestoreCache has to ask.
	//
	// If the restore never finished, the file still holds what the client had
	// before the bridge, and the bridge is taking the cache over again right
	// now: it becomes the record once more, which is what makes an interrupted
	// restore recoverable rather than litter.
	//
	// If the restore did finish and only the tidying up failed, the address in
	// it is already back in the cache -- and the client may have moved on to a
	// camera of its own since. Reclaiming it then would file that stale address
	// as "what the client had", and the next restore would undo the user's own
	// choice. It is litter, and the record to keep is a fresh copy of whatever
	// the cache says now.
	for _, record := range []struct {
		name  string
		erase bool
	}{
		{backup, false},
		{marker, true},
	} {
		// Marked as applied by the restore itself: the address in it is the one
		// the client is holding, or was until the client chose another. Either
		// way it is not the pre-bridge state any more, and the record to keep is
		// a fresh copy of whatever the cache says now.
		dropped, err := dropApplied(record.name)
		if err != nil {
			return err
		}
		if dropped {
			break
		}

		claimed := record.name + RestoringSuffix
		switch found, err := exists(claimed); {
		case err != nil:
			return err
		case !found:
			continue
		}
		// Not marked, so it is a restore that may never have written anything.
		// The client holding exactly what the record says is the one reading
		// that settles it; anything else is treated as unapplied, because the
		// cost of being wrong that way is a stale record rather than the loss
		// of the only copy of the client's own address.
		done, err := restoreLooksDone(path, claimed, record.erase)
		if err != nil {
			return err
		}
		if done {
			if err := os.Remove(claimed); err != nil {
				return fmt.Errorf("papertracker: remove %s: %w", claimed, err)
			}
			break
		}
		if err := os.Rename(claimed, record.name); err != nil {
			return fmt.Errorf("papertracker: rename %s: %w", claimed, err)
		}
		return nil
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

// ErrRestoreInterrupted means a restore stopped part way and the address from
// before the bridge is sitting under its working name. It is deliberately not
// applied on its own: doing that on the next start is how a cache the client
// has since updated gets written over.
var ErrRestoreInterrupted = errors.New("papertracker: a restore was interrupted")

// RestoreCache puts the pre-bridge address back and removes the record of it,
// so a user who stops using PaperBridge can return the client to its own
// camera.
func RestoreCache(installDir string) error {
	path := CachePath(installDir)

	working, erase, err := claimRestore(path)
	if err != nil {
		return err
	}

	if err := applyRestore(path, working, erase); err != nil {
		// Nothing was put back, so the claim goes back with it. Leaving it
		// taken would turn a full disk or a locked file into a restore that
		// only a person can finish: the next start would find a record whose
		// contents do not match the cache and refuse to guess. Put back where
		// it was, it is simply tried again.
		if undo := unclaimRestore(working); undo != nil {
			return errors.Join(err, undo)
		}
		return err
	}

	if err := os.Remove(working); err != nil {
		// The client is already back where it started, so what is left is only
		// the bridge's own file -- but a file under the working name is
		// ambiguous, and the next start would have to guess whether the address
		// in it had been applied. Saying so in the name is what removes the
		// guess. If even that fails there is nothing further to try, and the
		// caller is told either way.
		applied := strings.TrimSuffix(working, RestoringSuffix) + AppliedSuffix
		if renameErr := os.Rename(working, applied); renameErr != nil {
			return errors.Join(
				fmt.Errorf("papertracker: %s was restored but %s could not be removed: %w", path, working, err),
				fmt.Errorf("papertracker: rename %s to %s: %w", working, applied, renameErr),
			)
		}
		return fmt.Errorf("papertracker: %s was restored but %s could not be removed, so it was left as %s: %w",
			path, working, applied, err)
	}
	return nil
}

// dropApplied removes a record that has already been put back, and reports
// whether there was one. Nothing about it needs deciding: the name says the
// address in it is the one the client is holding.
func dropApplied(recordName string) (bool, error) {
	applied := recordName + AppliedSuffix
	switch found, err := exists(applied); {
	case err != nil:
		return false, err
	case !found:
		return false, nil
	}
	if err := os.Remove(applied); err != nil {
		return false, fmt.Errorf("papertracker: remove %s: %w", applied, err)
	}
	return true, nil
}

// applyRestore puts the client back the way the claimed record describes.
func applyRestore(path, working string, erase bool) error {
	if erase {
		// There was no cache before the bridge, so putting that back means
		// removing the file rather than writing an empty one.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("papertracker: remove %s: %w", path, err)
		}
		return nil
	}
	original, err := os.ReadFile(working)
	if err != nil {
		return fmt.Errorf("papertracker: read %s: %w", working, err)
	}
	// Written under a temporary name and renamed, because the record is
	// dropped straight after: a partial write here would lose the address for
	// good, leaving a truncated copy as the only thing to hand back.
	return writeAtomic(path, original)
}

// unclaimRestore puts a claimed record back under the name that means "waiting
// to be restored".
func unclaimRestore(working string) error {
	record := strings.TrimSuffix(working, RestoringSuffix)
	if err := os.Rename(working, record); err != nil {
		return fmt.Errorf("papertracker: put %s back to %s: %w", working, record, err)
	}
	return nil
}

// claimRestore takes charge of the pre-bridge record and reports how to apply
// it: the file to read it from, and whether it means "there was no cache".
//
// Moving it aside first is what stops the same restore happening twice. A
// record already under the working name is never applied for that reason: the
// most likely way to get one is a restore that put the address back and then
// could not delete its own file, and applying it again on the next start would
// undo an address the client had cached in the meantime -- a camera the user
// chose after they stopped using the bridge.
//
// That case is recognised by the client already holding what the record says,
// and cleaned up silently. Anything else means the restore stopped before it
// finished, which is reported rather than guessed at: the address is still on
// disk and the user can decide, where writing it over a cache that has moved on
// cannot be undone.
func claimRestore(path string) (working string, erase bool, err error) {
	// The marker wins: it says the client had no cache at all, and a backup
	// cannot exist alongside it.
	for _, record := range []struct {
		name  string
		erase bool
	}{
		{path + NoOriginalSuffix, true},
		{path + BackupSuffix, false},
	} {
		// A record that says it was already applied is only litter, and this is
		// the pass that clears it.
		switch dropped, err := dropApplied(record.name); {
		case err != nil:
			return "", false, err
		case dropped:
			return "", false, fmt.Errorf("%w at %s", ErrNoBackup, record.name)
		}

		claimed := record.name + RestoringSuffix
		switch found, err := exists(claimed); {
		case err != nil:
			return "", false, err
		case found:
			return "", false, tidyClaimed(path, claimed, record)
		}

		switch found, err := exists(record.name); {
		case err != nil:
			return "", false, err
		case !found:
			continue
		}
		if err := os.Rename(record.name, claimed); err != nil {
			return "", false, fmt.Errorf("papertracker: rename %s: %w", record.name, err)
		}
		return claimed, record.erase, nil
	}
	return "", false, fmt.Errorf("%w at %s", ErrNoBackup, path+BackupSuffix)
}

// tidyClaimed deals with a record left under its working name, and always
// returns an error saying what happened: either there is nothing left to
// restore, or the leftover needs a person.
func tidyClaimed(path, claimed string, record struct {
	name  string
	erase bool
}) error {
	done, err := restoreLooksDone(path, claimed, record.erase)
	if err != nil {
		return err
	}
	if !done {
		return fmt.Errorf("%w: %s holds what the client had before PaperBridge; rename it to %s to have it put back",
			ErrRestoreInterrupted, claimed, record.name)
	}
	if err := os.Remove(claimed); err != nil {
		return fmt.Errorf("papertracker: remove %s: %w", claimed, err)
	}
	return fmt.Errorf("%w at %s", ErrNoBackup, record.name)
}

// restoreLooksDone reports whether the client is already in the state the
// record describes, which is what tells a restore that only failed to tidy up
// from one that never finished.
func restoreLooksDone(path, claimed string, erase bool) (bool, error) {
	if erase {
		// "No cache before the bridge" is restored by there being no cache.
		found, err := exists(path)
		return !found, err
	}
	want, err := os.ReadFile(claimed)
	if err != nil {
		return false, fmt.Errorf("papertracker: read %s: %w", claimed, err)
	}
	got, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("papertracker: read %s: %w", path, err)
	}
	return bytes.Equal(got, want), nil
}

// exists reports whether path is there, treating anything other than "not
// found" as a reason to stop rather than a no.
func exists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("papertracker: stat %s: %w", path, err)
	}
	return false, nil
}

// recordNames lists every file that stands for a pre-bridge state, including
// the names a restore in progress uses.
func recordNames(path string) []string {
	return []string{
		path + BackupSuffix,
		path + BackupSuffix + RestoringSuffix,
		path + BackupSuffix + AppliedSuffix,
		path + NoOriginalSuffix,
		path + NoOriginalSuffix + RestoringSuffix,
		path + NoOriginalSuffix + AppliedSuffix,
	}
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

// FindRestoreDir looks for the installation the bridge actually wrote to.
//
// Restoring needs a different answer from writing. FindInstallDir returns the
// first folder that looks like PaperTracker at all, and with more than one on
// the machine -- an old copy beside a new one, or a portable build in Downloads
// -- that is quite likely not the one whose cache the bridge replaced. Restoring
// there finds no backup, reports that there is nothing to undo, and leaves the
// client that was really changed pointing at a bridge which is no longer
// running.
//
// Only the bridge's own files count as a match, for the same reason the backup
// is named after it: the search runs on machines the bridge may never have
// touched.
func FindRestoreDir() (string, error) {
	dirs, err := FindRestoreDirs()
	if err != nil {
		return "", err
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("%w in any of the usual PaperTracker folders", ErrNoBackup)
	}
	return dirs[0], nil
}

// FindRestoreDirs lists every folder the bridge left a record in.
//
// There can be more than one. install_dir is allowed to change while
// write_cache is on -- the client is reinstalled or moved -- and the folder
// left behind still holds a client pointed at the bridge, with a record beside
// it saying what it used to be. Restoring only the first would leave that one
// as it is, and nothing later would look again.
func FindRestoreDirs() ([]string, error) {
	var dirs []string
	for _, dir := range candidateDirs() {
		if dir == "" || slices.Contains(dirs, dir) {
			continue
		}
		if hasBackup(dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// WrittenDirsFile is the bridge's own note of which folders it has pointed at
// itself. It lives with the bridge's settings, not with the client.
//
// The search below only knows the usual places a client is installed, and
// install_dir can name somewhere else entirely -- a portable copy in a folder
// of the user's choosing. Once that setting goes away, and deleting the whole
// [papertracker] section is how someone returns to the defaults, nothing would
// ever name that folder again: the record would sit beside the client, and the
// client would keep pointing at a bridge that has stopped.
const WrittenDirsFile = "written-dirs.txt"

// RememberWrittenDir notes installDir as somewhere to look when restoring.
// Recording the same folder twice does nothing.
func RememberWrittenDir(stateDir, installDir string) error {
	installDir = strings.TrimSpace(installDir)
	if strings.TrimSpace(stateDir) == "" || installDir == "" {
		return nil
	}
	// Recorded as an absolute path. install_dir may be relative, and the folder
	// it names then depends on where the bridge was started from -- a manual
	// run from the client's own folder and the next sign-in are two different
	// places. What is recorded has to be the folder that was actually written
	// to, not a phrase that means something else later.
	installDir, err := filepath.Abs(installDir)
	if err != nil {
		return fmt.Errorf("papertracker: resolve %s: %w", installDir, err)
	}
	known, err := WrittenDirs(stateDir)
	if err != nil {
		return err
	}
	if slices.Contains(known, installDir) {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("papertracker: create %s: %w", stateDir, err)
	}
	known = append(known, installDir)
	return writeAtomic(filepath.Join(stateDir, WrittenDirsFile), []byte(strings.Join(known, "\n")+"\n"))
}

// WrittenDirs lists the folders recorded by RememberWrittenDir. A missing file
// is an ordinary answer: it means the bridge has never written a cache.
func WrittenDirs(stateDir string) ([]string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, nil
	}
	path := filepath.Join(stateDir, WrittenDirsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("papertracker: read %s: %w", path, err)
	}
	var dirs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !slices.Contains(dirs, line) {
			dirs = append(dirs, line)
		}
	}
	return dirs, nil
}

// hasBackup reports whether the bridge recorded a pre-bridge state in dir,
// which is either a backup of the client's address or the marker saying it had
// none. A restore left half done counts: it still has to be finished.
func hasBackup(dir string) bool {
	path := CachePath(dir)
	for _, name := range recordNames(path) {
		if _, err := os.Stat(name); err == nil {
			return true
		}
	}
	return false
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
