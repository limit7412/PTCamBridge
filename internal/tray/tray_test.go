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
func clickSources() (map[string]chan struct{}, map[string]<-chan struct{}) {
	send := map[string]chan struct{}{}
	watch := map[string]<-chan struct{}{}
	for _, choice := range sourceChoices {
		ch := make(chan struct{})
		send[choice.kind] = ch
		watch[choice.kind] = ch
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
	go watchSourceClicks(ctx, discardLogger(), watch, out)

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
	go watchSourceClicks(ctx, discardLogger(), watch, out)

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
		watchSourceClicks(ctx, discardLogger(), watch, out)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not stop when the context was cancelled")
	}
}
