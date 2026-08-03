//go:build !windows

package console

// Attach is a no-op away from Windows, where the standard streams are already
// connected to whatever launched the process.
func Attach() {}
