// Package i18n は、PTCamBridge が使う人に見せるテキストを、提供する全言語分
// 保持します。
//
// ここに入れるものと入れないものの線引きは意図的です。トレイ、-list-devices と
// -restore-cache の出力、そしてユーザーが対処すると想定される少数のエラーは
// 読み手に向けて書かれたものなので、翻訳します。ログは違います。ログは診断の
// 記録であり、不具合報告に貼られ、その文言で検索されます。実行した機械によって
// 言語が変わるログは、後から読む全員にとって価値が下がります。/stats と
// /api/v1/* の応答も同様で、これらはプログラムが読みます。
//
// コンソールへ出るもの全部ではありません。main が最後に ptcambridge: <err> として
// 出す起動時のエラーは英語のままです。ログと同じ性格のものですし、そもそもログが
// まだ立ち上がっていない時点の出力でもあります。
//
// カタログを生成物ではなく map にしているのは、文字列が数十で言語が 2 つだから
// です。そのために golang.org/x/text を持ち込むのは、cgo 無しの実行ファイル 1 つを
// 配るというこのプロジェクトに対して、依存とビルド手順を余計に増やすことになります。
package i18n

import (
	"fmt"
	"os"
	"strings"
)

// Lang は、画面を提供する言語です。
type Lang string

const (
	// English は原文が書かれている言語であり、何かが欠けているときの
	// 落とし先でもあります。
	English  Lang = "en"
	Japanese Lang = "ja"
	// Auto は、OS に設定されている言語に従うという指定です。
	Auto Lang = "auto"
)

// Supported は、選択できる言語の一覧です。Auto を含みます。
func Supported() []Lang { return []Lang{Auto, English, Japanese} }

// ParseLang は設定された言語を読み、Auto はシステムに問い合わせて解決します。
// 空は Auto を意味します。未知の値は、黙って英語に落とすのではなくエラーにします。
// "jp" と書いたユーザーには、何も変わらない理由を悩ませるのではなく、そう伝える
// べきだからです。
func ParseLang(s string) (Lang, error) {
	switch Lang(strings.ToLower(strings.TrimSpace(s))) {
	case "", Auto:
		return Detect(), nil
	case English:
		return English, nil
	case Japanese:
		return Japanese, nil
	}
	return English, fmt.Errorf("i18n: unknown language %q; use auto, en or ja", s)
}

// Detect は OS に設定されている言語を返します。このプログラムがテキストを持たない
// 言語だった場合は英語を返します。
func Detect() Lang { return fromTag(systemLanguage()) }

// fromTag は、BCP 47 または POSIX のロケールを対応言語に写します。見るのは主
// サブタグだけです。ja-JP も ja_JP.UTF-8 もどちらも日本語であり、ここには地域に
// よって変わるものはありません。
func fromTag(tag string) Lang {
	tag = strings.ToLower(strings.TrimSpace(tag))
	for _, cut := range []string{"-", "_", "."} {
		if i := strings.Index(tag, cut); i >= 0 {
			tag = tag[:i]
		}
	}
	if Lang(tag) == Japanese {
		return Japanese
	}
	return English
}

// envLanguage は、C ライブラリと同じ順序で POSIX のロケール変数を読みます。
// Windows 以外ではこれが判定のすべてであり、Windows ではこれが上書き手段です。
func envLanguage() string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// Key はメッセージ 1 件を識別します。英語の文字列ではなくキーで引くのは、英語の
// 文言を直したときに翻訳が黙って孤立しないようにするためです。
type Key string

// Printer は、1 つの言語でメッセージを組み立てます。
//
// パッケージレベルの言語設定ではなく値にしているのは、トレイとコンソールが起動時に
// 受け取ってそれきり変えないからです。グローバルにすると、得るものが無いままテストが
// 順序に依存するようになります。
type Printer struct{ lang Lang }

// NewPrinter は lang 用の printer を返します。対応していない言語では英語を出します。
// 英語はすべてのメッセージが持つことを保証されているからです。
func NewPrinter(lang Lang) Printer {
	if lang != Japanese {
		lang = English
	}
	return Printer{lang: lang}
}

// Lang は、この printer が使う言語です。
func (p Printer) Lang() Lang { return p.lang }

// S は k に対応するメッセージを返します。
//
// 登録の無いキーはキー自身を返します。これは意図的に不格好です。空のメニュー項目に
// なる代わりに画面上ですぐ目に付きますし、そもそもテストがリリースまで到達させません。
func (p Printer) S(k Key) string {
	forms, ok := messages[k]
	if !ok {
		return string(k)
	}
	if text, ok := forms[p.lang]; ok && text != "" {
		return text
	}
	return forms[English]
}

// F は k に対応するメッセージを args で整形します。
func (p Printer) F(k Key, args ...any) string {
	return fmt.Sprintf(p.S(k), args...)
}

// Reported は、機械が出した文言を画面に出すためのものです。key に名前が付いて
// いればユーザーの言語で、付いていなければ text をそのまま返します。
//
// この 2 段構えは status.Snapshot がエラーを運ぶ形そのものです。ユーザーが対処
// できる少数の失敗にはキーが付き、残りはドライバの文言のまま届きます。後者を
// 翻訳しないのは、それがログに載っているものと同じ文字列であり、書き写して
// 検索する対象だからです。
func (p Printer) Reported(key, text string) string {
	if key != "" {
		return p.S(Key(key))
	}
	return text
}
