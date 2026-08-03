//go:build !windows

package i18n

// systemLanguage reads the POSIX locale variables. Everywhere that is not
// Windows sets them, and they are the conventional place to look.
func systemLanguage() string { return envLanguage() }
