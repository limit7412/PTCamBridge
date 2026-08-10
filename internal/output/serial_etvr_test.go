package output

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
)

// fakePort は、テストが中身を読めて、詰まらせることもできるシリアルポートです。
type fakePort struct {
	mu      sync.Mutex
	written [][]byte
	// block が nil でない間、Write はそれが閉じられるまで返りません。相手が
	// 読まない仮想ポートを表しています。
	block chan struct{}

	closeOnce sync.Once
	closed    chan struct{}
}

func newFakePort() *fakePort {
	return &fakePort{closed: make(chan struct{})}
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	block := p.block
	p.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-p.closed:
			return 0, errors.New("fake port: closed while writing")
		}
	}
	select {
	case <-p.closed:
		return 0, errors.New("fake port: closed")
	default:
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = append(p.written, slices.Clone(b))
	return len(b), nil
}

func (p *fakePort) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

func (p *fakePort) packets() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.written)
}

func (p *fakePort) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// testJPEG は、core の検証を通る最小の JPEG です。中身が意味を持つ場面は無く、
// 通るかどうかだけが問われます。
func testJPEG(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = 0x20
	}
	data[0], data[1] = 0xFF, 0xD8
	data[len(data)-2], data[len(data)-1] = 0xFF, 0xD9
	return data
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// newTestSerial は、開くたびに ports から次のポートを返す出力を組み立てます。
// ports を使い切った後は最後のものを返し続けます。
func newTestSerial(t *testing.T, log *slog.Logger, frames Frames, open func(name string, baud int) (serialPort, error)) *Serial {
	t.Helper()
	if log == nil {
		log = discardLogger()
	}
	s, err := NewSerial(SerialConfig{Port: "COM-test"}, frames, log)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.openPort = open
	return s
}

func waitFor(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// 書き出したバイト列は、読む側のドライバがそのまま解析できなければ意味がありません。
// 形式の写しを 2 つ持つのではなく、実際に読ませて確かめます。
func TestTheSerialOutputWritesPacketsThatTheParserReadsBack(t *testing.T) {
	frames := hub.New()
	port := newFakePort()
	s := newTestSerial(t, nil, frames, func(string, int) (serialPort, error) { return port, nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()

	waitFor(t, 2*time.Second, "the port to open", func() bool { return s.Stats().Open })

	jpeg := testJPEG(64)
	frames.Publish(core.Frame{Data: jpeg})
	waitFor(t, 2*time.Second, "the frame to reach the port", func() bool { return len(port.packets()) == 1 })

	parser, err := core.NewETVRParser(nil, 0)
	if err != nil {
		t.Fatalf("NewETVRParser: %v", err)
	}
	got, rest, _ := parser.Parse(port.packets()[0])
	if len(got) != 1 {
		t.Fatalf("the parser found %d frames in the packet, want 1", len(got))
	}
	if !bytes.Equal(got[0], jpeg) {
		t.Errorf("the parser read back %d bytes, want the %d that were published", len(got[0]), len(jpeg))
	}
	if len(rest) != 0 {
		t.Errorf("the packet left %d unread bytes behind, want none", len(rest))
	}

	cancel()
	<-done
}

// 書く先を推測させてはいけません。当てが外れたポートに映像を流し込むことは、
// 読み違えと違って取り返しがつきません。
func TestTheSerialOutputRefusesAPortItWouldHaveToGuess(t *testing.T) {
	for _, port := range []string{"", "   ", "auto", "AUTO"} {
		_, err := NewSerial(SerialConfig{Port: port}, hub.New(), discardLogger())
		if err == nil {
			t.Errorf("NewSerial with port %q was accepted, want it refused", port)
		}
	}
	if _, err := NewSerial(SerialConfig{Port: "COM7"}, hub.New(), discardLogger()); err != nil {
		t.Errorf("NewSerial with a named port: %v", err)
	}
}

// ワイヤ形式の長さは 16 ビットなので、大きすぎるフレームは載りません。それは
// そのフレーム 1 枚の話であって、ポートの話ではありません。閉じて開き直しても
// 次に来るのは同じ大きさのフレームなので、捨てて先へ進みます。
func TestTheSerialOutputDropsAnOversizedFrameWithoutClosingThePort(t *testing.T) {
	frames := hub.New()
	port := newFakePort()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	s, err := NewSerial(SerialConfig{Port: "COM-test", MaxFrameSize: 1 << 20}, frames, log)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	opens := 0
	s.openPort = func(string, int) (serialPort, error) {
		opens++
		return port, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()
	waitFor(t, 2*time.Second, "the port to open", func() bool { return s.Stats().Open })

	frames.Publish(core.Frame{Data: testJPEG(0x10000 + 1)})
	waitFor(t, 2*time.Second, "the oversized frame to be counted", func() bool { return s.Stats().Dropped == 1 })

	if port.isClosed() {
		t.Fatal("the port was closed over one frame that did not fit")
	}
	frames.Publish(core.Frame{Data: testJPEG(64)})
	waitFor(t, 2*time.Second, "the next frame to be written", func() bool { return len(port.packets()) == 1 })

	if opens != 1 {
		t.Errorf("the port was opened %d times, want 1: the oversized frame should not have caused a reconnect", opens)
	}
	if !strings.Contains(logged.String(), "does not fit in an ETVR packet") {
		t.Errorf("nothing in the log says why the frame was dropped:\n%s", logged.String())
	}

	cancel()
	<-done
}

// 相手が読まない仮想ポートへの Write は返りません。それを返させる手段は閉じる
// ことだけなので、閉じることを誰かがしなければ、アプリケーションはそこで止まります。
func TestTheSerialOutputClosesAPortThatStopsDraining(t *testing.T) {
	shortenStall(t, 100*time.Millisecond, 10*time.Millisecond)
	shortenBackoff(t, 10*time.Millisecond)

	frames := hub.New()
	stuck := newFakePort()
	stuck.block = make(chan struct{})
	fresh := newFakePort()

	var mu sync.Mutex
	opened := 0
	s := newTestSerial(t, nil, frames, func(string, int) (serialPort, error) {
		mu.Lock()
		defer mu.Unlock()
		opened++
		if opened == 1 {
			return stuck, nil
		}
		return fresh, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()

	waitFor(t, 2*time.Second, "the first port to open", func() bool { return s.Stats().Open })
	frames.Publish(core.Frame{Data: testJPEG(64)})

	waitFor(t, 2*time.Second, "the stuck port to be closed", func() bool { return stuck.isClosed() })
	waitFor(t, 2*time.Second, "the port to be opened again", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return opened >= 2
	})
	if got := s.Stats().LastError; got == "" {
		t.Error("Stats does not say why the port was closed")
	}

	// 開き直した先では、また書けなければなりません。
	frames.Publish(core.Frame{Data: testJPEG(64)})
	waitFor(t, 2*time.Second, "the new port to receive a frame", func() bool { return len(fresh.packets()) > 0 })

	cancel()
	<-done
}

// 終了は、書き込みが詰まっていても通らなければなりません。通らなければ、この
// アプリケーションはユーザーが終了を選んでも降りられません。
func TestTheSerialOutputStopsWhileAWriteIsStuck(t *testing.T) {
	// 見張り役が止まった書き込みに気づく前にキャンセルが届くよう、停滞の判定は
	// 長いままにしておきます。ここで確かめたいのはキャンセルの経路です。
	shortenStall(t, time.Hour, 10*time.Millisecond)

	frames := hub.New()
	port := newFakePort()
	port.block = make(chan struct{})
	s := newTestSerial(t, nil, frames, func(string, int) (serialPort, error) { return port, nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()

	waitFor(t, 2*time.Second, "the port to open", func() bool { return s.Stats().Open })
	frames.Publish(core.Frame{Data: testJPEG(64)})
	// 書き込みが本当に始まって詰まるまで待ちます。始まる前にキャンセルすると、
	// 詰まった書き込みを解く経路は通りません。
	waitFor(t, 2*time.Second, "the write to block", func() bool { return s.writeInFlight() })

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return while a write was stuck")
	}
}

// 開けないポートは、諦める理由ではありません。ケーブルはまだ挿されていないかも
// しれませんし、仮想ポートのペアがまだ作られていないかもしれません。
func TestTheSerialOutputKeepsOpeningAPortThatIsNotThereYet(t *testing.T) {
	shortenBackoff(t, 10*time.Millisecond)

	frames := hub.New()
	port := newFakePort()
	var mu sync.Mutex
	attempts := 0
	s := newTestSerial(t, nil, frames, func(string, int) (serialPort, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return nil, errors.New("the port is not there")
		}
		return port, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()

	waitFor(t, 3*time.Second, "the port to open on a later attempt", func() bool { return s.Stats().Open })
	if got := s.Stats().Opens; got != 1 {
		t.Errorf("Stats.Opens = %d, want 1: the failed attempts should not count as opens", got)
	}

	cancel()
	<-done
}

// 購読はループの外で 1 回だけ行います。試行のたびにやり直すと、hub の購読者数が
// 上下し、それを配信の有無として読むものに嘘を伝えます。
func TestTheSerialOutputHoldsOneSubscriptionAcrossReconnects(t *testing.T) {
	shortenStall(t, 50*time.Millisecond, 10*time.Millisecond)
	shortenBackoff(t, 10*time.Millisecond)

	frames := hub.New()
	var mu sync.Mutex
	opened := 0
	s := newTestSerial(t, nil, frames, func(string, int) (serialPort, error) {
		mu.Lock()
		defer mu.Unlock()
		opened++
		port := newFakePort()
		port.block = make(chan struct{})
		return port, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()

	waitFor(t, 2*time.Second, "the port to open", func() bool { return s.Stats().Open })
	// 詰まらせては開き直させる。フレームを流し続けないと停滞の判定は始まりません。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frames.Publish(core.Frame{Data: testJPEG(64)})
		mu.Lock()
		enough := opened >= 3
		mu.Unlock()
		if enough {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	got := opened
	mu.Unlock()
	if got < 3 {
		t.Fatalf("the port was opened %d times, want at least 3 reconnects to have happened", got)
	}
	if subs := frames.InternalSubscribers(); subs != 1 {
		t.Errorf("the hub has %d internal subscribers after %d reconnects, want 1", subs, got)
	}
	// この出口はクライアントではありません。トレイと診断画面が見せる数に
	// 混ざると、誰も繋いでいないのに常に 1 が出ます。
	if subs := frames.Subscribers(); subs != 0 {
		t.Errorf("the hub counts %d stream clients while only the serial output is subscribed, want 0", subs)
	}

	cancel()
	<-done
	if subs := frames.InternalSubscribers(); subs != 0 {
		t.Errorf("the hub still has %d internal subscribers after Run returned, want 0", subs)
	}
}

func shortenStall(t *testing.T, timeout, check time.Duration) {
	t.Helper()
	oldTimeout, oldCheck := writeStallTimeout, writeStallCheck
	writeStallTimeout, writeStallCheck = timeout, check
	t.Cleanup(func() { writeStallTimeout, writeStallCheck = oldTimeout, oldCheck })
}

func shortenBackoff(t *testing.T, d time.Duration) {
	t.Helper()
	old := backoffInitial
	backoffInitial = d
	t.Cleanup(func() { backoffInitial = old })
}
