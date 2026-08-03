package tray

import (
	"syscall"
	"unsafe"
)

// The tray is the whole UI, and a menu item cannot ask a question. Downloading
// a third party's binary onto the user's machine is not something a single
// click should do silently, so this is the one place PTCamBridge puts a window
// on screen -- through user32 directly, which keeps the build free of cgo and
// of a UI toolkit brought in for one message box.
var (
	user32      = syscall.NewLazyDLL("user32.dll")
	messageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOKCancel      = 0x00000001
	mbIconQuestion  = 0x00000020
	mbSetForeground = 0x00010000
	// The tray icon has no window of its own, so without this the box can open
	// behind whatever the user is looking at and the bridge appears to hang.
	mbTopMost = 0x00040000

	idOK = 1
)

// confirm asks the user to approve an action and reports whether they did.
//
// A failure to show the box counts as "no": treating a question that was never
// asked as agreement is exactly the outcome the box exists to prevent.
func confirm(title, text string) bool {
	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	textPtr, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return false
	}
	ret, _, _ := messageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(mbOKCancel|mbIconQuestion|mbSetForeground|mbTopMost),
	)
	return ret == idOK
}
