package tray

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps the shell helper from flashing a console window.
const createNoWindow = 0x08000000

// openPath hands a file, folder or URL to the shell. rundll32 with the
// protocol handler covers all three, unlike explorer.exe which is fine for
// paths but awkward for URLs.
func openPath(target string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd.Start()
}
