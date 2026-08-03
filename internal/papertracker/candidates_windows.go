package papertracker

import (
	"os"
	"path/filepath"
)

// candidateDirs lists the folders a PaperTracker install is typically found
// in, most specific first.
func candidateDirs() []string {
	var dirs []string
	for _, base := range []string{
		os.Getenv("LOCALAPPDATA"),
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("APPDATA"),
	} {
		if base == "" {
			continue
		}
		dirs = append(dirs,
			filepath.Join(base, "PaperTracker"),
			filepath.Join(base, "Programs", "PaperTracker"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "PaperTracker"))
	}
	return append(dirs, `C:\PaperTracker`)
}
