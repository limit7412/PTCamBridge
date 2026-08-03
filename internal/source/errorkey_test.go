package source

import (
	"errors"
	"fmt"
	"testing"

	"github.com/limit7412/PTCamBridge/internal/i18n"
)

// Only the failures a person can act on without the log get a translation.
// Everything else keeps the driver's own words, which carry the detail.
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
		// Wrapped on the way up through the drivers, which is how these
		// actually arrive at the tracker.
		{name: "wrapped", err: fmt.Errorf("starting the source: %w", ErrNoFFmpeg), want: string(i18n.ErrNoFFmpeg)},
		{name: "fatal wrapper", err: fatalf(ErrNoDevice), want: string(i18n.ErrNoCamera)},
		// The detail in these is the point of them.
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

// Every key it hands out has to exist, or the tray shows the key itself.
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
