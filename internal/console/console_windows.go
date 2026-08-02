// Package console reattaches a GUI-subsystem process to the terminal that
// launched it.
//
// PaperBridge is linked with -H=windowsgui so the tray application does not
// drag a console window along, but that also detaches stdout and stderr. The
// command line flags (-list-devices, -version) would then print into the void.
package console

import (
	"os"
	"syscall"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
)

// attachParentProcess is ATTACH_PARENT_PROCESS, defined as (DWORD)-1.
var attachParentProcess = ^uintptr(0)

// Attach binds the standard streams to the parent process's console, if there
// is one. It is a no-op when launched from Explorer or already attached, so it
// is safe to call unconditionally at startup.
func Attach() {
	if ret, _, _ := procAttachConsole.Call(attachParentProcess); ret == 0 {
		return
	}
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	os.Stdout, os.Stderr = out, out
}
