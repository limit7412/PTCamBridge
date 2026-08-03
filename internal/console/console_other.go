//go:build !windows

package console

// Attach は Windows 以外では何もしません。標準ストリームは、プロセスを起動した
// ものに最初から繋がっています。
func Attach() {}
