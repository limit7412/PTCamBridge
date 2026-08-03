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
// GetUserPreferredUILanguages, not GetUserDefaultLocaleName. Those are two
// different settings and Windows lets them disagree: the locale is the
// regional format, the number and date conventions, while the UI language is
// the one menus are drawn in. A machine displaying Japanese with its region
// set to the United States would be handed English by the locale call, which
// is precisely the case this has to get right.
//
// The list is preferred languages in order; the first is the one Windows draws
// its own interface in, and the only one considered here.
func systemLanguage() string {
	if v := envLanguage(); v != "" {
		return v
	}

	var count, length uint32
	// A nil buffer asks for the size, in wide characters, of the whole
	// double-null-terminated list.
	ok, _, _ := getUserPreferredUILanguages.Call(
		muiLanguageName,
		uintptr(unsafe.Pointer(&count)),
		0,
		uintptr(unsafe.Pointer(&length)),
	)
	if ok == 0 || length == 0 {
		return ""
	}

	buf := make([]uint16, length)
	ok, _, _ = getUserPreferredUILanguages.Call(
		muiLanguageName,
		uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&length)),
	)
	if ok == 0 || count == 0 {
		return ""
	}
	// UTF16ToString stops at the first null, which is the end of the first
	// name in the list.
	return syscall.UTF16ToString(buf)
}

// muiLanguageName asks for names such as "ja-JP" rather than numeric language
// identifiers, so the result parses the same way the POSIX side does.
const muiLanguageName = 0x8

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	getUserPreferredUILanguages = kernel32.NewProc("GetUserPreferredUILanguages")
)
