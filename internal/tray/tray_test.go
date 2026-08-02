package tray

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// clickSources builds one unblocked click channel per source type, the shape
// systray hands over.
func clickSources(extra ...string) (map[string]chan struct{}, map[string]<-chan struct{}) {
	send := map[string]chan struct{}{}
	watch := map[string]<-chan struct{}{}
	keys := make([]string, 0, len(sourceChoices)+len(extra))
	for _, choice := range sourceChoices {
		keys = append(keys, choice.kind)
	}
	keys = append(keys, extra...)
	for _, key := range keys {
		ch := make(chan struct{})
		send[key] = ch
		watch[key] = ch
	}
	return send, watch
}

// Each entry has its own channel, so nothing about them carries an order on
// its own. Receiving them in one place is what supplies it: with a goroutine
// per entry, two clicks are taken independently and then race to forward, and
// picking UVC and then MJPEG could arrive the other way round -- leaving the
// bridge on the source the user chose first.
func TestSourceClicksArriveInTheOrderTheyWereClicked(t *testing.T) {
	want := []string{config.SourceUVC, config.SourceMJPEG, config.SourceSerial, config.SourceMJPEG}

	send, watch := clickSources()
	out := make(chan string, len(want))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchClicks(ctx, discardLogger(), watch, out)

	// The channels are unblocked, so each send returns only once the watcher
	// has taken it. That is the same handover systray does.
	for _, kind := range want {
		select {
		case send[kind] <- struct{}{}:
		case <-time.After(2 * time.Second):
			t.Fatalf("the watcher was not listening for a %s click", kind)
		}
	}

	for i, expected := range want {
		select {
		case got := <-out:
			if got != expected {
				t.Fatalf("click %d = %q, want %q", i+1, got, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("click %d (%s) never arrived", i+1, expected)
		}
	}
}

// systray sends with a select and a default: a click only lands if a receiver
// is parked on that channel right then. So the watcher has to be back waiting
// straight away, which means it must never block handing a click on.
func TestSourceClickWatcherKeepsListeningWhenNobodyDrains(t *testing.T) {
	send, watch := clickSources()
	// Depth 1, then left full on purpose.
	out := make(chan string, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchClicks(ctx, discardLogger(), watch, out)

	for i := 0; i < 4; i++ {
		select {
		case send[config.SourceUVC] <- struct{}{}:
		case <-time.After(2 * time.Second):
			t.Fatalf("the watcher stopped listening after %d clicks it could not forward", i)
		}
	}
}

func TestSourceClickWatcherStopsWithTheContext(t *testing.T) {
	_, watch := clickSources()
	out := make(chan string, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchClicks(ctx, discardLogger(), watch, out)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not stop when the context was cancelled")
	}
}

// Pause and the source entries share one ordered input, not a case each in the
// event loop. Go picks among ready select cases at random, so a case each
// would let a pause overtake the source click before it -- and since a source
// cannot be changed while paused, that pauses the old source instead of the
// new one the user had just chosen.
func TestPauseAndSourceClicksShareOneOrder(t *testing.T) {
	want := []string{config.SourceMJPEG, actionPause, config.SourceUVC, actionPause}

	send, watch := clickSources(actionPause)
	out := make(chan string, len(want))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchClicks(ctx, discardLogger(), watch, out)

	for _, action := range want {
		select {
		case send[action] <- struct{}{}:
		case <-time.After(2 * time.Second):
			t.Fatalf("the watcher was not listening for a %s click", action)
		}
	}

	for i, expected := range want {
		select {
		case got := <-out:
			if got != expected {
				t.Fatalf("click %d = %q, want %q", i+1, got, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("click %d (%s) never arrived", i+1, expected)
		}
	}
}

// The pause key shares a namespace with the source type names, so it has to
// stay distinct from all of them.
func TestPauseActionDoesNotCollideWithASourceType(t *testing.T) {
	for _, choice := range sourceChoices {
		if choice.kind == actionPause {
			t.Fatalf("actionPause %q is also a source type", actionPause)
		}
	}
}

// A full queue drops the action, and has to say so. A caller tracking what it
// has asked for -- the pause toggle does -- must not count a request that was
// never made, or its next click asks for the state the bridge is already in
// and the button looks broken.
func TestCommandQueueReportsWhetherAnActionWasTaken(t *testing.T) {
	// Nothing runs until the worker is released, so the queue fills up.
	release := make(chan struct{})
	q := newCommandQueue(discardLogger(), 2)
	defer close(release)

	if !q.submit("first", func() { <-release }) {
		t.Fatal("the first action was refused by an empty queue")
	}
	// The worker may or may not have picked the first one up yet, so fill
	// past the depth rather than assuming.
	for i := 0; i < 8; i++ {
		q.submit("filler", func() {})
	}
	if q.submit("overflow", func() {}) {
		t.Error("an action was accepted by a queue that is already full")
	}
}

// The queue exists to put the actions in one order and keep them there.
func TestCommandQueueRunsActionsInOrder(t *testing.T) {
	done := make(chan string, 3)
	q := newCommandQueue(discardLogger(), commandQueueDepth)

	for _, name := range []string{"one", "two", "three"} {
		if !q.submit(name, func() { done <- name }) {
			t.Fatalf("%s was refused", name)
		}
	}
	q.close()

	for i, want := range []string{"one", "two", "three"} {
		select {
		case got := <-done:
			if got != want {
				t.Fatalf("action %d = %q, want %q", i+1, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("action %d (%s) never ran", i+1, want)
		}
	}
}
