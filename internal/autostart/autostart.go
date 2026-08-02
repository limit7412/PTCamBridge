// Package autostart registers PaperBridge to launch when the user signs in.
//
// On Windows that means an entry under the per-user Run key, which needs no
// elevation. Everything else reports that it is unsupported rather than
// pretending to succeed.
package autostart

import "errors"

// ErrUnsupported is returned on platforms with no autostart integration.
var ErrUnsupported = errors.New("autostart: not supported on this platform")

// EntryName is the value name written under the Run key.
const EntryName = "PaperBridge"

// Enabled reports whether the autostart entry exists and points at this
// executable.
func Enabled() (bool, error) { return enabled() }

// Enable registers the current executable to start at sign-in.
func Enable() error { return enable() }

// Disable removes the autostart entry. Removing an entry that is not there
// succeeds.
func Disable() error { return disable() }

// Supported reports whether this platform has an autostart implementation.
func Supported() bool { return supported }
