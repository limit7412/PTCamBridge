package i18n

import (
	"syscall"
	"unsafe"
)

// systemLanguage は、ユーザーの表示言語を Windows に問い合わせます。
//
// Windows は POSIX のロケール変数を一切設定しないので、通常の機械では環境から
// 読めるものはありません。それでも先に環境変数を見るのは、それを設定するシェルから
// ブリッジを起動する人はそう意図しているからであり、システム設定を変えずにもう一方の
// 言語を試せる唯一の手段でもあるからです。
//
// GetUserDefaultLocaleName ではなく GetUserPreferredUILanguages です。この 2 つは
// 別の設定で、Windows は両者が食い違うことを許します。ロケールは地域形式、つまり
// 数値や日付の慣習であり、UI 言語はメニューが描かれる言語です。表示が日本語で
// 地域が米国という機械は、ロケール側の呼び出しでは英語を返されます。まさにここで
// 間違えてはいけない場合です。
//
// 返るのは優先順のリストで、先頭が Windows 自身が画面を描いている言語です。ここで
// 見るのはそれだけです。
func systemLanguage() string {
	if v := envLanguage(); v != "" {
		return v
	}

	var count, length uint32
	// バッファに nil を渡すと、二重 null 終端のリスト全体の大きさを
	// ワイド文字数で問い合わせることになる。
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
	// UTF16ToString は最初の null で止まる。それがリストの先頭要素の終わり。
	return syscall.UTF16ToString(buf)
}

// muiLanguageName は、数値の言語識別子ではなく "ja-JP" のような名前を要求します。
// 結果を POSIX 側とまったく同じ方法で解釈できるようにするためです。
const muiLanguageName = 0x8

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	getUserPreferredUILanguages = kernel32.NewProc("GetUserPreferredUILanguages")
)
