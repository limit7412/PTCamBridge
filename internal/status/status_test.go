package status

import (
	"errors"
	"testing"
)

// トレイはどの失敗を翻訳してよいかを知る必要があり、error の値が最後に渡るのは
// tracker である。
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

// 接続したらエラーと一緒に理由も消す。さもないとトレイは、既に終わった失敗を
// 説明し続ける。
func TestTrackerClearsTheKeyOnConnect(t *testing.T) {
	tracker := New(WithErrorKeys(func(error) string { return "err.no_camera" }))
	tracker.Disconnected("uvc", errors.New("no camera"))
	tracker.Connected("uvc")

	if got := tracker.Snapshot().LastErrorKey; got != "" {
		t.Errorf("LastErrorKey = %q after connecting, want empty", got)
	}
}

// 分類器の無い tracker も動く。ただ翻訳を求めないだけで、それが既存の呼び出し側
// すべてが期待している挙動。
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

// 一時停止は数えます。今の状態だけを載せると、状態を読む側の 2 回の読みの間で
// 止めて再開まで済ませた操作が見えません。読む間隔は、背景のタブでは分単位まで
// 伸びます。
func TestTrackerCountsThePauses(t *testing.T) {
	tr := New()
	if got := tr.Snapshot().Pauses; got != 0 {
		t.Fatalf("pauses = %d before anything happened, want 0", got)
	}

	tr.SetPaused(true)
	tr.SetPaused(false)
	if got := tr.Snapshot().Pauses; got != 1 {
		t.Errorf("pauses = %d after one pause and resume, want 1", got)
	}

	tr.SetPaused(true)
	tr.SetPaused(false)
	if got := tr.Snapshot().Pauses; got != 2 {
		t.Errorf("pauses = %d after a second pause, want 2", got)
	}

	// 同じことを二度命じても状態は動いていないので、数も動かしません。
	tr.SetPaused(true)
	tr.SetPaused(true)
	if got := tr.Snapshot().Pauses; got != 3 {
		t.Errorf("pauses = %d after being told to pause twice, want 3 — nothing moved the second time", got)
	}
}
