// Package console は、GUI サブシステムのプロセスを、それを起動した端末に繋ぎ直します。
//
// PTCamBridge は -H=windowsgui でリンクしており、トレイアプリがコンソールウィンドウを
// 連れ回さないようにしています。ただしそれは標準出力と標準エラー出力の切り離しも
// 意味します。そのままではコマンドラインのフラグ (-list-devices、-version) の出力が
// どこにも出ません。
package console

import (
	"os"
	"syscall"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
)

// attachParentProcess は ATTACH_PARENT_PROCESS で、(DWORD)-1 と定義されています。
var attachParentProcess = ^uintptr(0)

// Attach は、親プロセスのコンソールがあれば標準ストリームをそこに繋ぎます。
// エクスプローラーから起動された場合や既に繋がっている場合は何もしないので、
// 起動時に無条件で呼んで構いません。
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
