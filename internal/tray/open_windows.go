package tray

import (
	"fmt"
	"syscall"
	"unsafe"
)

// openPath は、ファイル・フォルダ・URL をシェルに開かせます。
//
// ShellExecuteW を直接呼びます。以前は rundll32 に url.dll,FileProtocolHandler を
// 渡す子プロセスを起動していましたが、それには 2 つの問題がありました。
//
// 1 つ目は、こちらが子プロセスに渡す STARTUPINFO です。コンソールの一瞬の表示を
// 抑えるために HideWindow を立てていましたが、これは STARTF_USESHOWWINDOW と
// SW_HIDE を意味し、シェル経由で起動されるアプリケーションまでそれを受け継ぎます。
// メモ帳やエクスプローラーは起動したうえで、隠れたまま現れません。クリックしても
// 何も起きないように見え、しかもエラーは何も出ません (#18)。
//
// 2 つ目は引用符です。FileProtocolHandler はコマンドラインの残りをそのまま URL と
// して受け取るので、Go が空白を含むパスに付ける引用符が中身の一部になります。
// ユーザー名に空白がある機械では設定ファイルのパスが必ずそうなります。
//
// ShellExecuteW にはどちらもありません。コマンドラインを組み立てないので引用符の
// 問題は起きようがなく、表示状態は SW_SHOWNORMAL として明示的に渡します。子プロセスを
// 挟まないので、隠すべきコンソールもありません。user32 を直接呼ぶ confirm と同じ理由で、
// cgo も UI ツールキットも持ち込まずに済みます。
func openPath(target string) error {
	targetPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("open %q: %w", target, err)
	}
	verbPtr, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return fmt.Errorf("open %q: %w", target, err)
	}

	ret, _, callErr := shellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(targetPtr)),
		0,
		0,
		swShowNormal,
	)
	// 成功したかどうかは 32 を超える値かどうかで決まります。この API は成功時にも
	// GetLastError を設定するので、callErr はその境界を下回ったときにだけ見ます。
	if ret <= shellExecuteSuccessFloor {
		return fmt.Errorf("open %q: ShellExecute returned %d: %w", target, ret, callErr)
	}
	return nil
}

var (
	shell32       = syscall.NewLazyDLL("shell32.dll")
	shellExecuteW = shell32.NewProc("ShellExecuteW")
)

const (
	// swShowNormal は、開いたウィンドウを通常どおり表示させます。ここを明示するのが
	// 上の 1 つ目の問題への答えそのものです。
	swShowNormal = 1

	// shellExecuteSuccessFloor は、ShellExecute が成功を表すのに超える値です。
	// 32 以下はすべてエラーコードで、この API はそう定義されています。
	shellExecuteSuccessFloor = 32
)
