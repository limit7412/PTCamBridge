package source

import (
	"errors"

	"github.com/limit7412/PTCamBridge/internal/i18n"
)

// ErrorKey names the message for a failure the interface should show in the
// user's language, or returns empty for one it should not.
//
// Only the failures a person can act on without reading the log: no camera
// named, no ffmpeg found, no port that could be a board. Everything else --
// a refused port, a device that vanished mid-capture, a malformed URL -- is
// reported as the driver wrote it, because those messages carry the detail
// that makes them useful and a translated summary would carry less.
//
// It lives here rather than in the tray or the status tracker because this is
// where the errors are defined: a sentinel added next door should be one edit
// away from being translatable, not two packages away.
func ErrorKey(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoDevice):
		return string(i18n.ErrNoCamera)
	case errors.Is(err, ErrNoFFmpeg):
		return string(i18n.ErrNoFFmpeg)
	case errors.Is(err, ErrNoSerialPort):
		return string(i18n.ErrNoPort)
	}
	return ""
}
