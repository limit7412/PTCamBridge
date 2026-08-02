//go:build !windows

package papertracker

import (
	"os"
	"path/filepath"
)

// candidateDirs covers the Wine and development layouts a non-Windows build
// might see. The client itself only ships for Windows, so this exists to keep
// the package testable and the code path identical across platforms.
func candidateDirs() []string {
	var dirs []string
	home, err := os.UserHomeDir()
	if err != nil {
		return dirs
	}
	return append(dirs,
		filepath.Join(home, "PaperTracker"),
		filepath.Join(home, ".local", "share", "PaperTracker"),
		filepath.Join(home, ".wine", "drive_c", "PaperTracker"),
		filepath.Join(home, ".wine", "drive_c", "Program Files", "PaperTracker"),
	)
}
