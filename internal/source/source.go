// Package source holds the imperative shell drivers that pull frames from a
// camera: a UVC device through an ffmpeg child process, a wired Babble board
// over a serial port, or an existing MJPEG-over-HTTP stream.
//
// Every driver reconnects on its own. A driver's Run only returns when the
// context is cancelled or the configuration is unusable, so a camera that is
// unplugged and plugged back in recovers without restarting the bridge.
package source

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// Reconnect backoff, per FR-5.
const (
	backoffInitial = 1 * time.Second
	backoffMax     = 15 * time.Second
	// backoffStable is how long an attempt must survive before the delay is
	// considered recovered and reset to the initial value.
	backoffStable = 30 * time.Second
)

// Source produces frames until its context is cancelled.
type Source interface {
	// Run streams frames into out. It handles its own reconnection and only
	// returns on cancellation or on a configuration error that retrying
	// cannot fix.
	Run(ctx context.Context, out chan<- core.Frame) error
	// Name is the driver name shown in logs and on the status endpoints.
	Name() string
}

// Reporter receives connection state transitions from a driver. The bridge
// implements it to back /healthz and /stats; drivers stay unaware of both.
type Reporter interface {
	Connected(source string)
	Disconnected(source string, err error)
}

// NopReporter discards state transitions.
type NopReporter struct{}

func (NopReporter) Connected(string)           {}
func (NopReporter) Disconnected(string, error) {}

// splitFunc carves whole frames out of an accumulated byte buffer, returning
// the bytes it could not consume yet.
type splitFunc func(buf []byte, maxSize int) (frames [][]byte, rest []byte)

// frameAssembler accumulates reads and hands back the complete frames in them.
// Drivers read into a scratch buffer of their own and feed it here; the
// assembler owns the partial-frame carry-over across reads.
type frameAssembler struct {
	buf     []byte
	maxSize int
	split   splitFunc
}

func newFrameAssembler(split splitFunc, maxSize int) *frameAssembler {
	if maxSize <= 0 {
		maxSize = core.DefaultMaxFrameSize
	}
	return &frameAssembler{maxSize: maxSize, split: split}
}

// feed appends chunk and returns any frames that are now complete. The
// returned frames are owned by the caller; the internal buffer is compacted in
// place, which is safe because rest aliases a later offset of the same array.
func (a *frameAssembler) feed(chunk []byte) [][]byte {
	a.buf = append(a.buf, chunk...)
	frames, rest := a.split(a.buf, a.maxSize)
	a.buf = append(a.buf[:0], rest...)
	return frames
}

// reset drops any partial frame, for use after a reconnect.
func (a *frameAssembler) reset() { a.buf = a.buf[:0] }

// pending is how many bytes are held waiting to become a frame. A driver
// reporting what arrived cannot get that from its own read count: a read can
// carry a frame and the beginning of the next one together, and those trailing
// bytes belong to the time after that frame, not before it.
func (a *frameAssembler) pending() int { return len(a.buf) }

// send hands a frame to the pipeline, honouring cancellation. The receiving
// channel is shallow and the hub past it never blocks, so this waits only for
// the transform step.
func send(ctx context.Context, out chan<- core.Frame, data []byte) error {
	select {
	case out <- core.Frame{Data: data, RecvedAt: time.Now()}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runWithBackoff repeatedly runs attempt until ctx is cancelled, waiting
// 1s, 2s, 4s ... up to 15s between failures. An attempt that stayed up for
// backoffStable resets the delay, so an occasional dropout does not leave a
// long-running source stuck at the maximum wait.
func runWithBackoff(ctx context.Context, log *slog.Logger, name string, reporter Reporter, attempt func(context.Context) error) error {
	if reporter == nil {
		reporter = NopReporter{}
	}
	delay := backoffInitial
	for {
		started := time.Now()
		err := attempt(ctx)
		if ctx.Err() != nil {
			return nil
		}
		reporter.Disconnected(name, err)

		var fatal *FatalError
		if errors.As(err, &fatal) {
			log.Error("source cannot start", "source", name, "error", err)
			return err
		}
		if time.Since(started) >= backoffStable {
			delay = backoffInitial
		}
		if err != nil {
			log.Warn("source disconnected, retrying", "source", name, "error", err, "retry_in", delay)
		} else {
			log.Info("source ended, retrying", "source", name, "retry_in", delay)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if delay *= 2; delay > backoffMax {
			delay = backoffMax
		}
	}
}

// FatalError marks a failure that retrying cannot fix, such as an
// unparsable configuration value. Drivers return it to stop the retry loop.
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }

func fatalf(err error) error { return &FatalError{Err: err} }
