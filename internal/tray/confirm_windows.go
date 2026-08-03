package tray

import (
	"syscall"
	"unsafe"
)

// UI はトレイがすべてで、メニュー項目は問いを立てられません。第三者のバイナリを
// ユーザーの機械へダウンロードすることは、1 回のクリックで黙って行われてよいこと
// ではないので、ここが PTCamBridge が画面にウィンドウを出す唯一の場所です。user32 を
// 直接使うことで、ビルドは cgo からも、メッセージボックス 1 つのために持ち込む UI
// ツールキットからも自由なままでいられます。
var (
	user32      = syscall.NewLazyDLL("user32.dll")
	messageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOKCancel      = 0x00000001
	mbIconQuestion  = 0x00000020
	mbSetForeground = 0x00010000
	// トレイアイコンは自身のウィンドウを持たないので、これが無いとボックスが
	// ユーザーの見ているものの背後に開き、ブリッジが固まったように見える。
	mbTopMost = 0x00040000

	idOK = 1
)

// confirm は、操作の承認をユーザーに求め、承認されたかどうかを返します。
//
// ボックスを表示できなかった場合は「いいえ」として扱います。一度も尋ねられなかった
// 問いを同意とみなすことは、まさにこのボックスが防ぐために存在する結末だからです。
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
