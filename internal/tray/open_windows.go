package tray

import (
	"fmt"
	"runtime"
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
// 問題は起きようがなく、表示状態は SW_SHOWNORMAL として明示的に渡します。
//
// この関数はブロックします。呼び出し側は必ずトレイのイベントループの外で実行して
// ください。openTarget がそうしています。
func openPath(target string) error {
	// スレッドを固定して COM を初期化します。既定のハンドラがインプロセスの COM
	// シェル拡張として実装されている場合、ShellExecuteW はそちらへ処理を委ねるので、
	// 初期化されていないスレッドから呼ぶと開けないことがあります。それでは
	// 「クリックしても何も起きない」という、この修正が消そうとしている状態がそのまま
	// 残ります。
	//
	// COM のアパートメントはスレッドに紐づくので、固定は初期化と同じくらい重要です。
	// goroutine が別のスレッドへ移った先で ShellExecuteW を呼べば、そこは初期化されて
	// いません。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// シェル拡張が期待するのは STA です。
	hr, _, _ := coInitializeEx.Call(0, coinitApartmentThreaded)
	// S_FALSE はこのスレッドで既に初期化済みという意味で、成功です。どちらの場合も
	// 釣り合いを取るために CoUninitialize を呼びます。RPC_E_CHANGED_MODE のときだけは
	// こちらが初期化したのではないので呼びません。
	if hr == sOK || hr == sFalse {
		defer coUninitialize.Call()
	}

	targetPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("open %q: %w", target, err)
	}
	verbPtr, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return fmt.Errorf("open %q: %w", target, err)
	}

	ret, _, _ := shellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(targetPtr)),
		0,
		0,
		swShowNormal,
	)
	if ret > shellExecuteSuccessFloor {
		return nil
	}
	return fmt.Errorf("open %q: %s", target, shellExecuteError(ret))
}

// shellExecuteError は、ShellExecuteW の戻り値を説明に変えます。
//
// GetLastError は見ません。この API は失敗の理由を 32 以下の戻り値で報告するもので、
// あわせて GetLastError を設定することは保証していません。それを読むと、ゼロや無関係な
// 直前のエラーを拾って「操作は正常に終了しました」とログに残ることになります。この
// ログは、開かなかった理由についてユーザーが手にできる唯一の手がかりです。
func shellExecuteError(code uintptr) string {
	switch code {
	case 0:
		return "the system is out of memory or resources"
	case 2:
		return "the file was not found"
	case 3:
		return "the path was not found"
	case 5:
		return "access was denied"
	case 8:
		return "there was not enough memory to finish the operation"
	case 11:
		return "the executable is not a valid application"
	case 26:
		return "a sharing violation occurred"
	case 27:
		return "the file association is incomplete or invalid"
	case 28:
		return "the request timed out waiting for the application to respond (DDE)"
	case 29:
		return "the application failed to complete the request (DDE)"
	case 30:
		return "the application is busy with another request (DDE)"
	case 31:
		return "no application is associated with this file type"
	case 32:
		return "the associated application could not be loaded"
	default:
		return fmt.Sprintf("ShellExecute failed with code %d", code)
	}
}

var (
	shell32       = syscall.NewLazyDLL("shell32.dll")
	shellExecuteW = shell32.NewProc("ShellExecuteW")

	ole32          = syscall.NewLazyDLL("ole32.dll")
	coInitializeEx = ole32.NewProc("CoInitializeEx")
	coUninitialize = ole32.NewProc("CoUninitialize")
)

const (
	// swShowNormal は、開いたウィンドウを通常どおり表示させます。ここを明示するのが
	// 上の 1 つ目の問題への答えそのものです。
	swShowNormal = 1

	// shellExecuteSuccessFloor は、ShellExecute が成功を表すのに超える値です。
	// 32 以下はすべてエラーコードで、この API はそう定義されています。
	shellExecuteSuccessFloor = 32

	coinitApartmentThreaded = 0x2

	sOK    = 0
	sFalse = 1
)
