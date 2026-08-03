package source

import (
	"errors"
	"fmt"
	"testing"

	"github.com/limit7412/PTCamBridge/internal/i18n"
)

// 翻訳するのは、ログ無しで対処できる失敗だけ。それ以外はドライバ自身の文言を
// 残す。詳細を運んでいるのはそちら。
func TestErrorKey(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nothing wrong", err: nil, want: ""},
		{name: "no camera named", err: ErrNoDevice, want: string(i18n.ErrNoCamera)},
		{name: "no ffmpeg", err: ErrNoFFmpeg, want: string(i18n.ErrNoFFmpeg)},
		{name: "no board-like port", err: ErrNoSerialPort, want: string(i18n.ErrNoPort)},
		// ドライバを上がってくる途中で包まれる。これらが実際に tracker へ届く
		// ときの形。
		{name: "wrapped", err: fmt.Errorf("starting the source: %w", ErrNoFFmpeg), want: string(i18n.ErrNoFFmpeg)},
		{name: "fatal wrapper", err: fatalf(ErrNoDevice), want: string(i18n.ErrNoCamera)},
		// これらの要点は、その中にある詳細。
		{name: "a port that refused", err: errors.New("serial: read from COM4: access denied"), want: ""},
		{name: "a camera that vanished", err: errors.New("uvc: camera produced no frame for 10s"), want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorKey(tc.err); got != tc.want {
				t.Errorf("ErrorKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// 返すキーはすべて実在しなければならない。さもないとトレイがキーそのものを表示する。
func TestErrorKeysAreRealMessages(t *testing.T) {
	p := i18n.NewPrinter(i18n.Japanese)
	for _, err := range []error{ErrNoDevice, ErrNoFFmpeg, ErrNoSerialPort} {
		key := ErrorKey(err)
		if key == "" {
			t.Fatalf("%v has no key", err)
		}
		if got := p.S(i18n.Key(key)); got == key {
			t.Errorf("%q renders as itself, so there is no message for it", key)
		}
	}
}
