//go:build !windows

package i18n

// systemLanguage は POSIX のロケール変数を読みます。Windows 以外はどこでもこれを
// 設定しますし、慣習的にもここを見るものです。
func systemLanguage() string { return envLanguage() }
