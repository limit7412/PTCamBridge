package i18n

import (
	"regexp"
	"strings"
	"testing"
)

// A half-translated menu reads as a bug rather than as a language the program
// does not have, so every key carries every language. The fallback in S exists
// for safety, not as somewhere to leave work unfinished.
func TestEveryMessageHasEveryLanguage(t *testing.T) {
	for key, forms := range messages {
		for _, lang := range []Lang{English, Japanese} {
			if strings.TrimSpace(forms[lang]) == "" {
				t.Errorf("%s has no %s text", key, lang)
			}
		}
	}
}

// verbs finds the format placeholders in a message. %% is an escaped percent
// and takes no argument, so it is not one.
var verbs = regexp.MustCompile(`%[-+# 0-9.*]*[a-zA-Z]`)

// A %s that became a %d in translation does not fail to build; it renders as
// "%!d(string=uvc)" in front of the user, in the language they chose.
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
	// A language nobody wrote text for still has to render something.
	p := NewPrinter(Lang("de"))
	if got := p.S(MenuQuit); got != "Quit" {
		t.Errorf("S(MenuQuit) = %q, want the English text", got)
	}
	if p.Lang() != English {
		t.Errorf("Lang() = %q, want English for an unsupported language", p.Lang())
	}
}

// An unknown key is rendered as itself: visible at a glance, rather than an
// empty menu entry that looks like a rendering bug.
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
		// Empty and "auto" both mean "ask the system", which the environment
		// above pins to English for this test.
		{in: "", want: English},
		{in: "auto", want: English},
		// Not silently English: someone who wrote this meant Japanese and has
		// to be told it did not take.
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

// Detection has to honour the environment, which is the only way to try the
// other language without changing a system setting.
func TestDetectReadsTheEnvironment(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "ja_JP.UTF-8")
	if got := Detect(); got != Japanese {
		t.Errorf("Detect() = %q, want Japanese from LANG", got)
	}

	// LC_ALL wins over LANG, the way the C library orders them.
	t.Setenv("LC_ALL", "en_US.UTF-8")
	if got := Detect(); got != English {
		t.Errorf("Detect() = %q, want LC_ALL to win", got)
	}
}
