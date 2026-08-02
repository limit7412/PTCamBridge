// Package status tracks the health of the active capture source so the
// /healthz and /stats endpoints, and the tray menu, can report it.
package status

import (
	"sync"
	"time"
)

// Snapshot is a consistent read of the tracker.
type Snapshot struct {
	Source         string    `json:"source"`
	Connected      bool      `json:"connected"`
	LastError      string    `json:"last_error,omitempty"`
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
	reconnects     uint64
	connectedSince time.Time
	startedAt      time.Time
	paused         bool
}

// New returns a tracker whose uptime starts now.
func New() *Tracker {
	return &Tracker{startedAt: time.Now()}
}

// SetSource records which driver is active, clearing the previous driver's
// state. Call this when a source is selected or switched.
func (t *Tracker) SetSource(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = name
	t.connected = false
	t.lastError = ""
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
		Reconnects:     t.reconnects,
		ConnectedSince: t.connectedSince,
		StartedAt:      t.startedAt,
		UptimeSeconds:  time.Since(t.startedAt).Seconds(),
		Paused:         t.paused,
	}
}
