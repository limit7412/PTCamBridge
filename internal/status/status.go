// Package status tracks the health of the active capture source so the
// /healthz and /stats endpoints, and the tray menu, can report it.
package status

import (
	"sync"
	"time"
)

// Snapshot is a consistent read of the tracker.
type Snapshot struct {
	Source    string `json:"source"`
	Connected bool   `json:"connected"`
	LastError string `json:"last_error,omitempty"`
	// LastErrorKey names the message for LastError when it is one of the few
	// failures a user can act on, so the tray can show it in their language.
	// Empty for everything else, which the tray then shows as it is.
	//
	// Not in the JSON: /stats is read by programs, and the English text beside
	// it is the stable thing to key on.
	LastErrorKey   string    `json:"-"`
	Reconnects     uint64    `json:"reconnects"`
	ConnectedSince time.Time `json:"connected_since"`
	StartedAt      time.Time `json:"started_at"`
	UptimeSeconds  float64   `json:"uptime_seconds"`
	Paused         bool      `json:"paused"`
}

// Tracker records source connection transitions. It satisfies the Reporter
// interface the drivers report through, without those drivers needing to know
// anything about HTTP or the tray.
type Tracker struct {
	mu             sync.RWMutex
	source         string
	connected      bool
	lastError      string
	lastErrorKey   string
	reconnects     uint64
	connectedSince time.Time
	startedAt      time.Time
	paused         bool

	// classify names the message for an error the interface should translate.
	// Injected because knowing which failures those are belongs to the drivers,
	// and this package is a leaf that the tray and the server both read.
	classify func(error) string
}

// Option configures a Tracker.
type Option func(*Tracker)

// WithErrorKeys teaches the tracker to name the failures worth translating.
// Without it every error is reported as its own text, which is what the log
// carries anyway.
func WithErrorKeys(classify func(error) string) Option {
	return func(t *Tracker) { t.classify = classify }
}

// New returns a tracker whose uptime starts now.
func New(opts ...Option) *Tracker {
	t := &Tracker{startedAt: time.Now()}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// SetSource records which driver is active, clearing the previous driver's
// state. Call this when a source is selected or switched.
func (t *Tracker) SetSource(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = name
	t.connected = false
	t.lastError = ""
	t.lastErrorKey = ""
	t.connectedSince = time.Time{}
}

// Connected marks the source as delivering frames.
func (t *Tracker) Connected(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.connected && t.source == source {
		return
	}
	t.source = source
	t.connected = true
	t.lastError = ""
	t.lastErrorKey = ""
	t.connectedSince = time.Now()
}

// Disconnected marks the source as down. A nil error means it ended cleanly.
func (t *Tracker) Disconnected(source string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = source
	if t.connected {
		t.reconnects++
	}
	t.connected = false
	t.connectedSince = time.Time{}
	if err != nil {
		t.lastError = err.Error()
		t.lastErrorKey = ""
		if t.classify != nil {
			t.lastErrorKey = t.classify(err)
		}
	}
}

// SetPaused records that the user paused capture from the tray menu, which is
// deliberate downtime rather than a fault.
func (t *Tracker) SetPaused(paused bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = paused
	if paused {
		t.connected = false
		t.connectedSince = time.Time{}
	}
}

// Paused reports whether capture is paused.
func (t *Tracker) Paused() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.paused
}

// Snapshot returns the current state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return Snapshot{
		Source:         t.source,
		Connected:      t.connected,
		LastError:      t.lastError,
		LastErrorKey:   t.lastErrorKey,
		Reconnects:     t.reconnects,
		ConnectedSince: t.connectedSince,
		StartedAt:      t.startedAt,
		UptimeSeconds:  time.Since(t.startedAt).Seconds(),
		Paused:         t.paused,
	}
}
