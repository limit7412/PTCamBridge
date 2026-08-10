package hub

import (
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

func frame(b byte) core.Frame {
	return core.Frame{Data: []byte{0xFF, 0xD8, b, 0xFF, 0xD9}}
}

func TestPublishReachesEverySubscriber(t *testing.T) {
	h := New()
	a, cancelA := h.Subscribe()
	b, cancelB := h.Subscribe()
	defer cancelA()
	defer cancelB()

	h.Publish(frame(1))

	for i, ch := range []<-chan core.Frame{a, b} {
		select {
		case f := <-ch:
			if f.Data[2] != 1 {
				t.Errorf("subscriber %d got the wrong frame", i)
			}
			if f.Seq != 1 {
				t.Errorf("subscriber %d got Seq %d, want 1", i, f.Seq)
			}
		default:
			t.Errorf("subscriber %d received nothing", i)
		}
	}
}

// 枠を読んでいない購読者が受け取るべきは、既に待っていた方ではなく最新のフレーム。
func TestPublishReplacesTheQueuedFrame(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	defer cancel()

	h.Publish(frame(1))
	h.Publish(frame(2))
	h.Publish(frame(3))

	f := <-ch
	if f.Data[2] != 3 {
		t.Errorf("got frame %d, want the newest one (3)", f.Data[2])
	}
	select {
	case extra := <-ch:
		t.Errorf("expected a depth of one, also got frame %d", extra.Data[2])
	default:
	}
	if got := h.Stats().Dropped; got != 2 {
		t.Errorf("Dropped = %d, want 2", got)
	}
}

// 詰まった購読者が、他の購読者を足止めしてはいけない。
func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	h := New()
	slow, cancelSlow := h.Subscribe()
	fast, cancelFast := h.Subscribe()
	defer cancelSlow()
	defer cancelFast()

	_ = slow // never read from
	for i := 0; i < 50; i++ {
		h.Publish(frame(byte(i)))
		select {
		case f := <-fast:
			if f.Data[2] != byte(i) {
				t.Fatalf("iteration %d: the fast subscriber fell behind", i)
			}
		default:
			t.Fatalf("iteration %d: the fast subscriber received nothing", i)
		}
	}
}

func TestLatest(t *testing.T) {
	h := New()
	if _, ok := h.Latest(); ok {
		t.Error("a fresh hub should not report a latest frame")
	}
	h.Publish(frame(7))
	f, ok := h.Latest()
	if !ok || f.Data[2] != 7 {
		t.Errorf("Latest() = (%v, %v), want frame 7", f.Data, ok)
	}
}

func TestCancelUnsubscribesAndClosesTheChannel(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	if h.Subscribers() != 1 {
		t.Fatalf("Subscribers() = %d, want 1", h.Subscribers())
	}

	cancel()
	cancel() // must be safe to call twice

	if h.Subscribers() != 0 {
		t.Errorf("Subscribers() = %d after cancel, want 0", h.Subscribers())
	}
	if _, open := <-ch; open {
		t.Error("the channel should be closed after cancel")
	}
	h.Publish(frame(1)) // must not panic on a closed channel
}

func TestStatsTracksRateAndSize(t *testing.T) {
	h := New()
	now := time.Now()
	for i := 0; i < 5; i++ {
		h.Publish(core.Frame{Data: []byte{0xFF, 0xD8, 0x00, 0xFF, 0xD9}, RecvedAt: now.Add(time.Duration(i) * 33 * time.Millisecond)})
	}

	s := h.Stats()
	if s.Published != 5 {
		t.Errorf("Published = %d, want 5", s.Published)
	}
	if s.LastFrameSize != 5 {
		t.Errorf("LastFrameSize = %d, want 5", s.LastFrameSize)
	}
	if s.InputFPS < 20 || s.InputFPS > 45 {
		t.Errorf("InputFPS = %.1f, want roughly 30 for 33ms spacing", s.InputFPS)
	}
}

// リセット幅より長い間隔が空いたということはソースが落ちたということ。その断絶を
// またいでレートを平滑化してはいけない。
func TestStatsResetsRateAfterAGap(t *testing.T) {
	h := New()
	now := time.Now()
	h.Publish(core.Frame{Data: frame(1).Data, RecvedAt: now.Add(-time.Hour)})
	h.Publish(core.Frame{Data: frame(2).Data, RecvedAt: now.Add(-time.Hour).Add(10 * time.Second)})

	if got := h.Stats().InputFPS; got != 0 {
		t.Errorf("InputFPS = %.3f after a long gap, want 0", got)
	}
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	h := New()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			h.Publish(frame(byte(i)))
		}
	}()
	for i := 0; i < 50; i++ {
		ch, cancel := h.Subscribe()
		select {
		case <-ch:
		default:
		}
		cancel()
	}
	<-done
}

// ブリッジ自身の出口が取りこぼした分を、ストリームクライアントの取りこぼしに
// 混ぜてはいけません。混ぜると、HTTP に誰も繋がっていないのに診断画面の「破棄」が
// 増え続け、原因を指すものがどこにもなくなります。シリアル出力の相手が居ないだけで
// 起きる見え方です。
func TestDropsAreCountedSeparatelyForTheBridgesOwnOutlets(t *testing.T) {
	h := New()

	// どちらも読まないので、2 枚目から取りこぼしが始まる。
	external, cancelExternal := h.Subscribe()
	defer cancelExternal()
	internal, cancelInternal := h.SubscribeInternal()
	defer cancelInternal()
	_, _ = external, internal

	for i := 0; i < 4; i++ {
		h.Publish(core.Frame{Data: []byte{0xFF, 0xD8, 0xFF, 0xD9}})
	}

	stats := h.Stats()
	if stats.Dropped == 0 || stats.DroppedInternal == 0 {
		t.Fatalf("Dropped = %d, DroppedInternal = %d, want both sides to have dropped something",
			stats.Dropped, stats.DroppedInternal)
	}
	if stats.Dropped != stats.DroppedInternal {
		t.Errorf("Dropped = %d but DroppedInternal = %d; both subscribers stalled the same way",
			stats.Dropped, stats.DroppedInternal)
	}
}

// 出口だけが詰まっているとき、ストリームクライアント側の破棄は 0 のままでなければ
// なりません。ここが動くと、HTTP 配信に問題があるように見えます。
func TestAStalledOutletDoesNotShowUpAsClientDrops(t *testing.T) {
	h := New()
	internal, cancel := h.SubscribeInternal()
	defer cancel()
	_ = internal

	for i := 0; i < 4; i++ {
		h.Publish(core.Frame{Data: []byte{0xFF, 0xD8, 0xFF, 0xD9}})
	}

	if got := h.Stats().Dropped; got != 0 {
		t.Errorf("Dropped = %d with no stream client subscribed, want 0", got)
	}
	if got := h.Stats().DroppedInternal; got == 0 {
		t.Error("DroppedInternal = 0, want the stalled outlet counted somewhere")
	}
}
