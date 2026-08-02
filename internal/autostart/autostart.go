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

// Enabled reports whether the autostart entry exists and matches what Enable
// would write for the same configPath.
func Enabled(configPath string) (bool, error) { return enabled(configPath) }

// Enable registers the current executable to start at sign-in.
//
// configPath is the settings file the user named on the command line, and is
// written into the registered command so the next sign-in starts with the same
// settings. Pass an empty string when no path was given, which leaves the
// entry using the default per-user location.
func Enable(configPath string) error { return enable(configPath) }

// Disable removes the autostart entry. Removing an entry that is not there
// succeeds.
func Disable() error { return disable() }

// Supported reports whether this platform has an autostart implementation.
func Supported() bool { return supported }
