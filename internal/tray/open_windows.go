package tray

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// openPath は、ファイル・フォルダ・URL をシェルに開かせます。
//
// シェルの API を直接呼びます。以前は rundll32 に url.dll,FileProtocolHandler を
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
// シェルに直接頼めばどちらもありません。コマンドラインを組み立てないので引用符の
// 問題は起きようがなく、表示状態は SW_SHOWNORMAL として明示的に渡します。
//
// ShellExecuteW ではなく ShellExecuteExW を呼ぶのは、SEE_MASK_NOASYNC を渡せるのが
// こちらだけだからです。理由は下の呼び出し部分に書きました。
//
// この関数はブロックします。呼び出し側は必ずトレイのイベントループの外で実行して
// ください。openTarget がそうしています。
func openPath(target string) error {
	// スレッドを固定して COM を初期化します。既定のハンドラがインプロセスの COM
	// シェル拡張として実装されている場合、シェルはそちらへ処理を委ねるので、
	// 初期化されていないスレッドから呼ぶと開けないことがあります。それでは
	// 「クリックしても何も起きない」という、この修正が消そうとしている状態がそのまま
	// 残ります。
	//
	// COM のアパートメントはスレッドに紐づくので、固定は初期化と同じくらい重要です。
	// goroutine が別のスレッドへ移った先で呼べば、そこは初期化されていません。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// シェル拡張が期待するのは STA です。OLE1 の DDE を切るのは、ここで欲しいものが
	// 何も無いのに、応答しないハンドラを待つ経路だけが増えるからです。
	hr, _, _ := coInitializeEx.Call(0, coinitApartmentThreaded|coinitDisableOLE1DDE)
	switch hr {
	case sOK, sFalse:
		// S_FALSE はこのスレッドで既に初期化済みという意味で、成功です。どちらの
		// 場合も釣り合いを取るために CoUninitialize を呼びます。
		defer coUninitialize.Call()
	case rpcEChangedMode:
		// このスレッドは既に MTA です。COM 自体は使えるので進みますが、こちらが
		// 初期化したのではないので CoUninitialize は呼びません。
		//
		// 呼び出しごとに新しい goroutine が新しいスレッドを固定するので、この分岐に
		// 来るのは、このプロセスの誰かがそのスレッドを MTA にして戻さなかった場合
		// だけです。今のところ CoInitializeEx を呼ぶのはここしかなく、ここは必ず
		// 釣り合いを取ります。それでも進むのは、MTA から開ける相手の方がはるかに
		// 多く、ここで諦めればクリックは確実に何も起こさないからです。
	default:
		// COM がまったく初期化されていない状態です。このまま呼べば、上に書いた
		// 「クリックしても何も起きない」に戻る可能性があります。ここで失敗させれば、
		// 少なくとも理由がログに残ります。
		return fmt.Errorf("open %q: CoInitializeEx failed with 0x%08x", target, uint32(hr))
	}

	targetPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("open %q: %w", target, err)
	}
	verbPtr, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return fmt.Errorf("open %q: %w", target, err)
	}

	// SEE_MASK_NOASYNC は、起動が終わるまで戻ってこさせます。
	//
	// 既定では、DDE や COM で非同期に起動される相手に対してシェルは要求を渡した時点で
	// 戻り、残りは呼び出し元スレッドのメッセージループの上で進みます。ここにはその
	// ループが無く、しかもこのスレッドは戻った直後に CoUninitialize して消えます。
	// つまり起動が終わる前に STA ごと足場が無くなり、対象は開かないままになり得ます。
	// Windows がこのフラグについて「メッセージループを持たない、あるいはまもなく
	// 終了するスレッドから呼ぶ場合は指定しなければならない」と書いているのは、
	// まさにこの状況のことです。
	sei := shellExecuteInfoW{
		fMask:  seeMaskNoAsync,
		lpVerb: verbPtr,
		lpFile: targetPtr,
		nShow:  swShowNormal,
	}
	sei.cbSize = uint32(unsafe.Sizeof(sei))

	ret, _, _ := shellExecuteExW.Call(uintptr(unsafe.Pointer(&sei)))
	runtime.KeepAlive(verbPtr)
	runtime.KeepAlive(targetPtr)
	if ret != 0 {
		return nil
	}
	return fmt.Errorf("open %q: %s", target, shellExecuteError(sei.hInstApp))
}

// shellExecuteInfoW は SHELLEXECUTEINFOW です。フィールドの順番と型が Windows 側の
// 定義とずれると、シェルは別の場所を読みます。並べ替えないでください。
type shellExecuteInfoW struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       uintptr
}

// shellExecuteError は、失敗した呼び出しが残した hInstApp を説明に変えます。
//
// GetLastError は見ません。この API は失敗の理由を 32 以下の SE_ERR_* として
// hInstApp に置くもので、あわせて GetLastError を設定することは保証していません。
// それを読むと、ゼロや無関係な直前のエラーを拾って「操作は正常に終了しました」と
// ログに残ることになります。このログは、開かなかった理由についてユーザーが手にできる
// 唯一の手がかりです。
//
// 訳し分けるのは、これらが正反対の対処を要するからです。関連付けが無いなら拡張子の
// 設定の話、DDE のタイムアウトなら開く先のアプリが応答していない話、アクセス拒否なら
// 権限の話で、数字のままログに出すとその 1 つ手前で止まります。
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
	shell32         = syscall.NewLazyDLL("shell32.dll")
	shellExecuteExW = shell32.NewProc("ShellExecuteExW")

	ole32          = syscall.NewLazyDLL("ole32.dll")
	coInitializeEx = ole32.NewProc("CoInitializeEx")
	coUninitialize = ole32.NewProc("CoUninitialize")
)

const (
	// swShowNormal は、開いたウィンドウを通常どおり表示させます。ここを明示するのが
	// 上の 1 つ目の問題への答えそのものです。
	swShowNormal = 1

	// seeMaskNoAsync は、起動が終わるまで戻ってこさせます。呼び出し部分を参照。
	seeMaskNoAsync = 0x00000100

	coinitApartmentThreaded = 0x2
	coinitDisableOLE1DDE    = 0x4

	sOK             = 0
	sFalse          = 1
	rpcEChangedMode = 0x80010106
)
