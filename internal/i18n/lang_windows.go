package i18n

import (
	"syscall"
	"unsafe"
)

// systemLanguage asks Windows what the user's display language is.
//
// Windows sets none of the POSIX locale variables, so there is nothing to read
// from the environment on a normal machine -- but they are honoured first
// anyway, because someone running the bridge from a shell that sets them means
// it, and it is the only way to try the other language without changing a
// system setting.
//
// GetUserDefaultLocaleName rather than GetUserDefaultUILanguage: it hands back
// a name like "ja-JP" that maps onto the same parsing the POSIX side uses,
// instead of a numeric LANGID that would need a table of its own.
func systemLanguage() string {
	if v := envLanguage(); v != "" {
		return v
	}

	// LOCALE_NAME_MAX_LENGTH is 85 wide characters.
	buf := make([]uint16, 85)
	n, _, _ := getUserDefaultLocaleName.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		// Nothing to go on; the caller falls back to English.
		return ""
	}
	// The count includes the terminating null.
	return syscall.UTF16ToString(buf[:n-1])
}

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	getUserDefaultLocaleName = kernel32.NewProc("GetUserDefaultLocaleName")
)
