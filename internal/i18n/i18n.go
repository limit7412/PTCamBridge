// Package i18n holds the text PTCamBridge shows to the person using it, in
// every language it is offered in.
//
// What is in here and what is not is a deliberate line. The tray, the console
// output and the handful of errors a user is expected to act on are written
// for a reader, so they are translated. The log is not: it is a diagnostic
// record, it gets pasted into bug reports and searched for by its wording, and
// a log that changes language with the machine it ran on is worth less to
// everyone who has to read it afterwards. Neither are the /stats and
// /api/v1/* responses, which are read by programs.
//
// The catalogue is a map rather than a generated bundle because there are a
// few dozen strings and two languages. Bringing in golang.org/x/text for that
// would add a dependency and a build step to a project whose distribution
// story is one cgo-free executable.
package i18n

import (
	"fmt"
	"os"
	"strings"
)

// Lang is a language the interface is offered in.
type Lang string

const (
	// English is the language the source text is written in, and the one
	// anything missing falls back to.
	English  Lang = "en"
	Japanese Lang = "ja"
	// Auto asks for the language the operating system is set to.
	Auto Lang = "auto"
)

// Supported lists the languages that can be chosen, Auto included.
func Supported() []Lang { return []Lang{Auto, English, Japanese} }

// ParseLang reads a configured language, resolving Auto against the system.
// An empty value means Auto. An unknown one is an error rather than a silent
// fall back to English: a user who wrote "jp" should be told, not left
// wondering why nothing changed.
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

// Detect returns the language the operating system is set to, or English when
// it is anything this program does not have text for.
func Detect() Lang { return fromTag(systemLanguage()) }

// fromTag maps a BCP 47 or POSIX locale onto a supported language. Only the
// primary subtag is considered: ja-JP and ja_JP.UTF-8 are both Japanese, and
// there is nothing here that varies by region.
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

// envLanguage reads the POSIX locale variables in the order the C library
// does. It is the whole of detection off Windows, and the override on it.
func envLanguage() string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// Key identifies one message. Keys rather than English strings as the lookup,
// so that editing the English wording cannot silently orphan a translation.
type Key string

// Printer renders messages in one language.
//
// A value rather than a package-level language, because the tray and the
// console are handed one at startup and never change it, and a global would
// make the tests order-dependent for no gain.
type Printer struct{ lang Lang }

// NewPrinter returns a printer for lang. An unsupported language prints
// English, which is what every message is guaranteed to have.
func NewPrinter(lang Lang) Printer {
	if lang != Japanese {
		lang = English
	}
	return Printer{lang: lang}
}

// Lang is the language this printer renders in.
func (p Printer) Lang() Lang { return p.lang }

// S returns the message for k.
//
// A key with no entry returns the key itself. That is deliberately ugly: it
// shows up immediately in the interface instead of rendering as an empty menu
// item, and a test already refuses to let one reach a release.
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

// F formats the message for k with args.
func (p Printer) F(k Key, args ...any) string {
	return fmt.Sprintf(p.S(k), args...)
}
