package status

import (
	"errors"
	"testing"
)

// The tray needs to know which failures it may translate, and the tracker is
// where the error value is last seen.
func TestTrackerNamesRecognisedFailures(t *testing.T) {
	known := errors.New("no camera")
	tracker := New(WithErrorKeys(func(err error) string {
		if errors.Is(err, known) {
			return "err.no_camera"
		}
		return ""
	}))

	tracker.Disconnected("uvc", known)
	if got := tracker.Snapshot().LastErrorKey; got != "err.no_camera" {
		t.Errorf("LastErrorKey = %q, want the classified key", got)
	}

	tracker.Disconnected("uvc", errors.New("something nobody translated"))
	if got := tracker.Snapshot().LastErrorKey; got != "" {
		t.Errorf("LastErrorKey = %q, want empty for an unrecognised failure", got)
	}
}

// Connecting clears the reason along with the error, or the tray keeps
// explaining a failure that is over.
func TestTrackerClearsTheKeyOnConnect(t *testing.T) {
	tracker := New(WithErrorKeys(func(error) string { return "err.no_camera" }))
	tracker.Disconnected("uvc", errors.New("no camera"))
	tracker.Connected("uvc")

	if got := tracker.Snapshot().LastErrorKey; got != "" {
		t.Errorf("LastErrorKey = %q after connecting, want empty", got)
	}
}

// A tracker with no classifier still works; it just never asks for a
// translation, which is what every existing caller expects.
func TestTrackerWithoutAClassifier(t *testing.T) {
	tracker := New()
	tracker.Disconnected("uvc", errors.New("no camera"))
	snapshot := tracker.Snapshot()
	if snapshot.LastError == "" {
		t.Error("the error text was lost")
	}
	if snapshot.LastErrorKey != "" {
		t.Errorf("LastErrorKey = %q, want empty with no classifier", snapshot.LastErrorKey)
	}
}
