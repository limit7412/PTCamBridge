package i18n

import (
	"regexp"
	"strings"
	"testing"
)

// 半分だけ翻訳されたメニューは、言語が無いのではなくバグに見える。だからすべての
// キーがすべての言語を持つ。S のフォールバックは安全のためにあるのであって、
// やりかけを置いておく場所ではない。
func TestEveryMessageHasEveryLanguage(t *testing.T) {
	for key, forms := range messages {
		for _, lang := range []Lang{English, Japanese} {
			if strings.TrimSpace(forms[lang]) == "" {
				t.Errorf("%s has no %s text", key, lang)
			}
		}
	}
}

// verbs はメッセージ中の書式指定子を探す。%% はエスケープされたパーセントで引数を
// 取らないので、これには数えない。
var verbs = regexp.MustCompile(`%[-+# 0-9.*]*[a-zA-Z]`)

// 翻訳の過程で %s が %d になってもビルドは通る。ユーザーの目の前に、本人が選んだ
// 言語で "%!d(string=uvc)" と表示されるだけ。
func TestTranslationsTakeTheSameArguments(t *testing.T) {
	for key, forms := range messages {
		want := verbs.FindAllString(forms[English], -1)
		for _, lang := range []Lang{Japanese} {
			got := verbs.FindAllString(forms[lang], -1)
			if len(got) != len(want) {
				t.Errorf("%s: %s has %d format verbs, English has %d (%v vs %v)", key, lang, len(got), len(want), got, want)
				continue
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s: %s verb %d is %s, English has %s", key, lang, i, got[i], want[i])
				}
			}
		}
	}
}

func TestPrinterFallsBackToEnglish(t *testing.T) {
	// 誰もテキストを書いていない言語でも、何かは表示されなければならない。
	p := NewPrinter(Lang("de"))
	if got := p.S(MenuQuit); got != "Quit" {
		t.Errorf("S(MenuQuit) = %q, want the English text", got)
	}
	if p.Lang() != English {
		t.Errorf("Lang() = %q, want English for an unsupported language", p.Lang())
	}
}

// 未知のキーはキー自身として表示する。描画のバグに見える空のメニュー項目ではなく、
// 一目で分かる形にする。
func TestPrinterShowsUnknownKeys(t *testing.T) {
	if got := NewPrinter(Japanese).S(Key("menu.nothing")); got != "menu.nothing" {
		t.Errorf("S of an unknown key = %q, want the key itself", got)
	}
}

func TestPrinterUsesTheChosenLanguage(t *testing.T) {
	if got := NewPrinter(Japanese).S(MenuQuit); got != "終了" {
		t.Errorf("Japanese S(MenuQuit) = %q", got)
	}
	if got := NewPrinter(English).S(MenuQuit); got != "Quit" {
		t.Errorf("English S(MenuQuit) = %q", got)
	}
}

func TestParseLang(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "en_US.UTF-8")

	tests := []struct {
		in      string
		want    Lang
		wantErr bool
	}{
		{in: "ja", want: Japanese},
		{in: "JA", want: Japanese},
		{in: " en ", want: English},
		// 空と "auto" はどちらも「システムに従う」を意味する。上の環境変数に
		// より、このテストではそれが英語に固定される。
		{in: "", want: English},
		{in: "auto", want: English},
		// 黙って英語にはしない。こう書いた人は日本語のつもりであり、それが
		// 効いていないことを伝えなければならない。
		{in: "jp", wantErr: true},
		{in: "japanese", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseLang(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLang(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLang(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseLang(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFromTag(t *testing.T) {
	tests := map[string]Lang{
		"ja":          Japanese,
		"ja-JP":       Japanese,
		"ja_JP.UTF-8": Japanese,
		"JA_jp":       Japanese,
		"en-GB":       English,
		"":            English,
		"C":           English,
		"POSIX":       English,
		"de-DE":       English,
		"japanese":    English, // not a tag; only the primary subtag counts
	}
	for tag, want := range tests {
		if got := fromTag(tag); got != want {
			t.Errorf("fromTag(%q) = %q, want %q", tag, got, want)
		}
	}
}

// 判定は環境変数を尊重しなければならない。システム設定を変えずにもう一方の言語を
// 試せる唯一の手段だから。
func TestDetectReadsTheEnvironment(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "ja_JP.UTF-8")
	if got := Detect(); got != Japanese {
		t.Errorf("Detect() = %q, want Japanese from LANG", got)
	}

	// C ライブラリの順序どおり、LC_ALL が LANG に勝つ。
	t.Setenv("LC_ALL", "en_US.UTF-8")
	if got := Detect(); got != English {
		t.Errorf("Detect() = %q, want LC_ALL to win", got)
	}
}
