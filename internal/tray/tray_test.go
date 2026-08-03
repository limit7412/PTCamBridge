package tray

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
	"github.com/limit7412/PTCamBridge/internal/i18n"
	"github.com/limit7412/PTCamBridge/internal/status"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// clickSources は、ソース種別ごとに非バッファのクリックチャネルを 1 本ずつ作る。
// systray が渡してくるのと同じ形。
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

// 各項目はそれぞれのチャネルを持つので、それら自体は順序を運ばない。順序を与えるのは
// 1 箇所で受け取ること。項目ごとに goroutine を立てると、2 つのクリックは独立に
// 取られてから転送を競い、UVC を選んでから MJPEG を選んだつもりが逆順で届き得る。
// ブリッジはユーザーが最初に選んだソースに残ることになる。
func TestSourceClicksArriveInTheOrderTheyWereClicked(t *testing.T) {
	want := []string{config.SourceUVC, config.SourceMJPEG, config.SourceSerial, config.SourceMJPEG}

	send, watch := clickSources()
	out := make(chan string, len(want))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchClicks(ctx, discardLogger(), watch, out)

	// チャネルは非バッファなので、各送信は監視側が受け取って初めて返る。systray が
	// 行う受け渡しと同じ。
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

// systray は select と default で送る。クリックが届くのは、ちょうどそのチャネルで
// 受信側が待っている場合だけ。だから監視側はすぐ待機へ戻らなければならず、それは
// クリックを渡すときに決してブロックしてはならないということ。
func TestSourceClickWatcherKeepsListeningWhenNobodyDrains(t *testing.T) {
	send, watch := clickSources()
	// 深さ 1 にして、意図的に埋まったままにしておく。
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

// 一時停止とソースの各項目は、イベントループの case を分けるのではなく、順序の
// ある 1 本の入力を共有する。Go は準備完了の select case からランダムに選ぶので、
// case を分けると一時停止がその前のソースクリックを追い越し得る。そして一時停止中は
// ソースを変更できないので、ユーザーが今選んだ新しいソースではなく、古いソースが
// 一時停止される。
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

// 一時停止のキーはソース種別の名前と名前空間を共有するので、そのすべてと区別され
// 続けなければならない。
func TestPauseActionDoesNotCollideWithASourceType(t *testing.T) {
	for _, choice := range sourceChoices {
		if choice.kind == actionPause {
			t.Fatalf("actionPause %q is also a source type", actionPause)
		}
	}
}

// キューが一杯なら操作は捨てられ、そのことを伝えなければならない。自分が何を
// 要求したかを追っている呼び出し側 — 一時停止の切り替えがそう — が、実際には行われ
// なかった要求を数えてはいけない。さもないと次のクリックがブリッジの既にある状態を
// 要求し、ボタンが壊れているように見える。
func TestCommandQueueReportsWhetherAnActionWasTaken(t *testing.T) {
	// ワーカーを解放するまで何も走らないので、キューが埋まる。
	const depth = 2
	running := make(chan struct{})
	release := make(chan struct{})
	q := newCommandQueue(discardLogger(), depth)
	defer close(release)

	if !q.submit("first", func() { close(running); <-release }) {
		t.Fatal("the first action was refused by an empty queue")
	}
	// 下の数え上げが正確になるのは、ワーカーが最初の操作の中に入るのを待つから。
	// それまではキューから取り出しているかどうか分からず、その間に空いた枠が
	// もう 1 つ分の余地を残してしまう。
	<-running

	for i := 0; i < depth; i++ {
		if !q.submit("filler", func() {}) {
			t.Fatalf("filler %d was refused by a queue with room for it", i+1)
		}
	}
	if q.submit("overflow", func() {}) {
		t.Error("an action was accepted by a queue that is already full")
	}
}

// キューは、操作を 1 つの順序に並べ、そのまま保つために存在する。
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

// この確認は、クリックと「第三者のバイナリがユーザーの機械に落ちてくること」の間に
// 立つものなので、誰が、どれくらいの大きさで、どのライセンスかを述べなければならない。
func TestFFmpegPromptNamesWhatIsBeingDownloaded(t *testing.T) {
	build := ffmpegfetch.Build{
		URL:       "https://example.invalid/ffmpeg-lgpl.zip",
		Size:      145349145,
		Publisher: "SomeBuilder/FFmpeg-Builds",
		License:   "LGPL v2.1 or later",
	}

	prompt := ffmpegPrompt(i18n.NewPrinter(i18n.English), build)
	for _, want := range []string{build.Publisher, build.URL, build.License, "145 MB"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not mention %q:\n%s", want, prompt)
		}
	}
}

func TestFFmpegStatusLineFollowsTheDownload(t *testing.T) {
	cases := []struct {
		name  string
		state ffmpegfetch.State
		want  string
	}{
		{"nothing yet", ffmpegfetch.State{}, "Get ffmpeg (for UVC cameras)"},
		{"in flight", ffmpegfetch.State{Downloading: true, Received: 50, Total: 200}, "Downloading ffmpeg... 25%"},
		{"in flight, size unknown", ffmpegfetch.State{Downloading: true}, "Downloading ffmpeg..."},
		{"done", ffmpegfetch.State{Installed: true}, "ffmpeg is installed"},
		{"failed", ffmpegfetch.State{LastError: "digest mismatch"}, "Get ffmpeg (last attempt failed)"},
		// 実行中のダウンロードは、以前のものが失敗していてもそう述べる。この項目が
		// 描写するのは今起きていることだから。
		{"retrying", ffmpegfetch.State{Downloading: true, LastError: "digest mismatch"}, "Downloading ffmpeg..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ffmpegStatusLine(i18n.NewPrinter(i18n.English), tc.state); got != tc.want {
				t.Errorf("ffmpegStatusLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// 状態表示は、ユーザーが一日中読む唯一のテキストなので、その言語で組み立てる。
// ソース名を囲む語も含めて。ソース名自体はプロトコルの識別子なので、そのまま残る。
func TestStatusLineIsTranslated(t *testing.T) {
	jp := i18n.NewPrinter(i18n.Japanese)
	en := i18n.NewPrinter(i18n.English)
	running := status.Snapshot{Source: "uvc", Connected: true}

	if got := statusLine(en, running, false, 29.97, 1); got != "uvc: 30.0 fps, 1 client(s)" {
		t.Errorf("English running line = %q", got)
	}
	if got := statusLine(jp, running, false, 29.97, 1); got != "uvc: 30.0 fps、クライアント 1 件" {
		t.Errorf("Japanese running line = %q", got)
	}
	if got := statusLine(jp, running, true, 0, 0); got != "uvc: 一時停止中" {
		t.Errorf("Japanese paused line = %q", got)
	}
	if got := statusLine(jp, status.Snapshot{}, false, 0, 0); got != "ソース未選択: 接続中..." {
		t.Errorf("Japanese line with no source = %q", got)
	}
}

// tracker が認識した失敗はユーザーの言語で見せる。認識しなかったものはドライバが
// 書いたまま見せる。それらのメッセージは、それを有用にしている詳細を運んでいるし、
// ログにも同じ言葉がある。
func TestStatusLineTranslatesOnlyTheRecognisedFailures(t *testing.T) {
	jp := i18n.NewPrinter(i18n.Japanese)

	known := status.Snapshot{
		Source:       "uvc",
		LastError:    "uvc: no camera configured. Run ...",
		LastErrorKey: string(i18n.ErrNoCamera),
	}
	got := statusLine(jp, known, false, 0, 0)
	if !strings.Contains(got, "カメラが設定されていません") {
		t.Errorf("a recognised failure was not translated: %q", got)
	}
	if strings.Contains(got, "no camera configured") {
		t.Errorf("a recognised failure kept the English text: %q", got)
	}

	unknown := status.Snapshot{Source: "serial", LastError: "serial: read from COM4: access denied"}
	got = statusLine(jp, unknown, false, 0, 0)
	if !strings.Contains(got, "access denied") {
		t.Errorf("an unrecognised failure lost the driver's words: %q", got)
	}
}

// 再接続中の行は、どの言語であってもメニュー項目に収まる短さでなければならない。
func TestStatusLineTruncatesTheReason(t *testing.T) {
	long := status.Snapshot{Source: "serial", LastError: strings.Repeat("x", 200)}
	got := statusLine(i18n.NewPrinter(i18n.Japanese), long, false, 0, 0)
	if strings.Count(got, "x") > 60 {
		t.Errorf("the reason was not truncated: %q", got)
	}
}

// 日本語の理由は 60 バイトを大きく超え、そこで切ると文字の内側に落ちる。トレイに
// 届くのは不正な UTF-8 であり、短くなった文ではなく置換文字として描かれる。
func TestTruncateCutsOnCharacterBoundaries(t *testing.T) {
	long := strings.Repeat("あ", 100)
	got := truncate(long, 60)

	if !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 60 {
		t.Errorf("truncate kept %d runes, want 60", n)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncate = %q, want it to end in an ellipsis", got)
	}
	// 既に十分短いので、どちらの文字体系でもそのまま返る。
	for _, s := range []string{"短い", "short"} {
		if got := truncate(s, 60); got != s {
			t.Errorf("truncate(%q) = %q, want it unchanged", s, got)
		}
	}
}

// トレイに渡されるのは状態表示の全体なので、それ全体が無事でなければならない。
func TestStatusLineStaysValidUTF8(t *testing.T) {
	snapshot := status.Snapshot{
		Source:       "uvc",
		LastError:    "ignored, the key wins",
		LastErrorKey: string(i18n.ErrNoFFmpeg),
	}
	got := statusLine(i18n.NewPrinter(i18n.Japanese), snapshot, false, 0, 0)
	if !utf8.ValidString(got) {
		t.Errorf("status line is not valid UTF-8: %q", got)
	}
}

// 応答しない対象を押し直したときに、実行中のものが積み上がらないこと。
func TestInFlightDropsRepeatsOfTheSameTarget(t *testing.T) {
	f := newInFlight()

	if !f.begin("C:\\logs") {
		t.Fatal("the first request was refused")
	}
	// 相手が固まっている間、押し直しは何も起こしてはならない。
	for i := 0; i < 5; i++ {
		if f.begin("C:\\logs") {
			t.Fatalf("repeat %d was accepted while the target was still opening", i)
		}
	}
	// 別の対象は巻き添えにしない。ログフォルダが固まっていても設定は開ける。
	if !f.begin("C:\\config.toml") {
		t.Error("a different target was refused")
	}

	f.done("C:\\logs")
	if !f.begin("C:\\logs") {
		t.Error("the target stayed locked after it finished")
	}
}

// 失敗して抜けた場合でも解放されること。開けなかった対象が二度と開けなくなるのは、
// 直そうとしているバグそのものに戻る。
func TestInFlightReleasesAfterFailure(t *testing.T) {
	f := newInFlight()
	func() {
		if !f.begin("target") {
			t.Fatal("the first request was refused")
		}
		defer f.done("target")
	}()
	if !f.begin("target") {
		t.Error("the target stayed locked after a failed attempt")
	}
}

// begin と done は別々の goroutine から呼ばれる。前者はイベントループ、後者は
// 開き終えたワーカー。
func TestInFlightIsSafeForConcurrentUse(t *testing.T) {
	f := newInFlight()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f.begin("target") {
				f.done("target")
			}
		}()
	}
	wg.Wait()
	if !f.begin("target") {
		t.Error("the target stayed locked after every attempt finished")
	}
}
