// Package hub distributes frames from the single active source to every
// connected stream client.
//
// Delivery is latest-frame-wins: each subscriber holds a one-frame slot and a
// frame that arrives while the previous one is still queued replaces it. For
// mouth tracking, a client that has fallen behind wants the current frame, not
// the backlog, and a slow client must never stall the source read loop.
package hub

import (
	"sync"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// fpsAlpha weights the newest interval in the input rate estimate.
const fpsAlpha = 0.1

// fpsGapReset is the arrival gap past which the rate estimate is discarded
// rather than smoothed, so a reconnect does not average across the outage.
const fpsGapReset = 2 * time.Second

// Stats is a snapshot of hub activity, surfaced by the /stats endpoint.
type Stats struct {
	Published     uint64    `json:"published"`
	Dropped       uint64    `json:"dropped"`
	Subscribers   int       `json:"subscribers"`
	InputFPS      float64   `json:"input_fps"`
	LastFrameSize int       `json:"last_frame_size"`
	LastFrameAt   time.Time `json:"last_frame_at"`
}

// Hub is a one-producer, many-consumer frame broadcaster.
type Hub struct {
	mu        sync.Mutex
	subs      map[uint64]chan core.Frame
	nextID    uint64
	seq       uint64
	latest    core.Frame
	hasLatest bool

	published uint64
	dropped   uint64
	fps       float64
	lastAt    time.Time
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{subs: make(map[uint64]chan core.Frame)}
}

// Publish broadcasts a frame. The hub stamps the sequence number and, when the
// caller left it zero, the arrival time. Data must not be modified afterwards:
// every subscriber shares the slice.
func (h *Hub) Publish(f core.Frame) {
	now := time.Now()
	if f.RecvedAt.IsZero() {
		f.RecvedAt = now
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.seq++
	f.Seq = h.seq
	h.latest = f
	h.hasLatest = true
	h.published++

	if !h.lastAt.IsZero() {
		gap := f.RecvedAt.Sub(h.lastAt)
		switch {
		case gap > fpsGapReset || gap <= 0:
			h.fps = 0
		case h.fps == 0:
			h.fps = 1 / gap.Seconds()
		default:
			h.fps = h.fps*(1-fpsAlpha) + (1/gap.Seconds())*fpsAlpha
		}
	}
	h.lastAt = f.RecvedAt

	for _, ch := range h.subs {
		select {
		case ch <- f:
			continue
		default:
		}
		// The slot is occupied by a frame this subscriber has not read yet.
		// Discard it and try once more; if the subscriber grabbed it in the
		// meantime the send succeeds, and if it is truly wedged we drop.
		select {
		case <-ch:
			h.dropped++
		default:
		}
		select {
		case ch <- f:
		default:
			h.dropped++
		}
	}
}

// Subscribe returns a channel of frames and a function that unsubscribes and
// closes it. The cancel function is idempotent and must be called exactly once
// per subscription for the hub to forget the client.
func (h *Hub) Subscribe() (<-chan core.Frame, func()) {
	ch := make(chan core.Frame, 1)

	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.subs[id] = ch
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(ch)
			}
		})
	}
	return ch, cancel
}

// Latest returns the most recently published frame, if any has been published.
func (h *Hub) Latest() (core.Frame, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.latest, h.hasLatest
}

// Subscribers reports the number of connected stream clients.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Stats returns a snapshot of hub counters.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()

	s := Stats{
		Published:   h.published,
		Dropped:     h.dropped,
		Subscribers: len(h.subs),
		InputFPS:    h.fps,
	}
	if h.hasLatest {
		s.LastFrameSize = h.latest.Size()
		s.LastFrameAt = h.latest.RecvedAt
	}
	// The rate estimate is only meaningful while frames keep arriving.
	if !h.lastAt.IsZero() && time.Since(h.lastAt) > fpsGapReset {
		s.InputFPS = 0
	}
	return s
}
