package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/status"
)

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mjpegUpstream は WiFi カメラの代役。
func mjpegUpstream(t *testing.T, jpg []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for {
			fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpg))
			if _, err := w.Write(jpg); err != nil {
				return
			}
			fmt.Fprint(w, "\r\n")
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// waitForFrame は hub にフレームが届くまで待ち、届かなければテストを失敗させる。
func waitForFrame(t *testing.T, h *hub.Hub, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f, ok := h.Latest(); ok {
			return f.Data
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no frame reached the hub within %s", timeout)
	return nil
}

func mjpegConfig(url string) config.Config {
	cfg := config.Default()
	cfg.Source.Type = config.SourceMJPEG
	cfg.Source.MJPEG.URL = url
	cfg.Normalise()
	return cfg
}

// パイプライン全体。上流のカメラ、ドライバ、変換段、hub。
func TestBridgeDeliversFramesToTheHub(t *testing.T) {
	jpg := testJPEG(t, 32, 32)
	upstream := mjpegUpstream(t, jpg)

	frames := hub.New()
	tracker := status.New()
	b := New(mjpegConfig(upstream.URL), "", frames, tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	got := waitForFrame(t, frames, 5*time.Second)
	if !bytes.Equal(got, jpg) {
		t.Error("the frame on the hub does not match what the camera sent")
	}
	if snapshot := tracker.Snapshot(); !snapshot.Connected || snapshot.Source != "mjpeg" {
		t.Errorf("status = %+v, want a connected mjpeg source", snapshot)
	}
}

// 何もしない変換は、再エンコードではなく元のバイトをそのまま流さなければならない。
func TestBridgeForwardsUntransformedFramesUntouched(t *testing.T) {
	jpg := testJPEG(t, 32, 32)
	upstream := mjpegUpstream(t, jpg)

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	if got := waitForFrame(t, frames, 5*time.Second); !bytes.Equal(got, jpg) {
		t.Errorf("the frame was altered: %d bytes in, %d bytes out", len(jpg), len(got))
	}
}

func TestBridgeAppliesTheConfiguredTransform(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 64, 32))

	cfg := mjpegConfig(upstream.URL)
	cfg.Transform.Rotate = 90

	frames := hub.New()
	b := New(cfg, "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	got := waitForFrame(t, frames, 5*time.Second)
	image, err := jpeg.DecodeConfig(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("decode the published frame: %v", err)
	}
	if image.Width != 32 || image.Height != 64 {
		t.Errorf("published frame is %dx%d, want 32x64 after a quarter turn", image.Width, image.Height)
	}
}

// 繋がっていないソースへの切替は失敗し、動いている方をそのまま走らせ続ける。
// 何もフレームを出していないのに成功を報告したりはしない。
func TestBridgeSwitchRejectsAnUnavailableSource(t *testing.T) {
	shortenVerify(t, 1500*time.Millisecond)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	if err := b.Switch(ctx, config.SourceSerial); err == nil {
		t.Fatal("expected switching to a serial port that is not there to fail")
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the working mjpeg source kept", got)
	}

	before := frames.Stats().Published
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames.Stats().Published > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the working source stopped producing frames after a rejected switch")
}

// 実際に動くソースへの切替は受け入れられ、フレームは届き続ける。
func TestBridgeSwitchToAWorkingSource(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	if err := b.Switch(ctx, config.SourceMJPEG); err != nil {
		t.Fatalf("Switch to a working source: %v", err)
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want mjpeg", got)
	}
}

func TestBridgeRejectsAnUnknownSourceType(t *testing.T) {
	b := New(config.Default(), "", hub.New(), status.New(), discardLogger())
	if err := b.Switch(context.Background(), "hologram"); err == nil {
		t.Fatal("expected an unknown source type to be rejected")
	}
}

// 一時停止はカメラを解放しなければならない。UVC のアクセスは排他的なので、
// ハンドルを握ったままの一時停止したブリッジは、依然として Baballonia を締め出す。
func TestBridgePauseStopsAndResumeRestarts(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	tracker := status.New()
	b := New(mjpegConfig(upstream.URL), "", frames, tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !b.Paused() || !tracker.Snapshot().Paused {
		t.Error("the bridge does not report itself as paused")
	}

	before := frames.Stats().Published
	time.Sleep(100 * time.Millisecond)
	if after := frames.Stats().Published; after != before {
		t.Errorf("%d frames were published while paused", after-before)
	}

	if err := b.SetPaused(false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames.Stats().Published > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no frames were published after resuming")
}

// ソースを起動できない設定が、ブリッジを何も動いていない状態にしてはいけない。
// 動いていた元のソースを戻す。
func TestBridgeApplyRevertsWhenTheNewSourceCannotStart(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	broken := b.Snapshot()
	broken.Source.Type = config.SourceUVC
	broken.Source.UVC.Device = "" // no device: the driver refuses to build

	if _, err := b.Apply(ctx, broken); err == nil {
		t.Fatal("expected Apply to fail for a source that cannot start")
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the working mjpeg source to be restored", got)
	}

	before := frames.Stats().Published
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames.Stats().Published > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the restored source is not producing frames")
}

// ドライバを組み立てられることが示すのは、設定が解釈できることだけ。実際に動き
// 出してから失敗するドライバ — ディスクに ffmpeg が無い場合など — が代償を払わせる
// べきは、少し前まで動いていたソースではなく設定変更の方。
func TestBridgeApplyRevertsWhenTheNewSourceFailsAsynchronously(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	// デバイス名は設定されているのでドライバは組み立てられる。無いのは ffmpeg で、
	// UVC がそれを知るのは Run の中。
	broken := b.Snapshot()
	broken.Source.Type = config.SourceUVC
	broken.Source.UVC.Device = "camera"
	broken.Source.UVC.FFmpegPath = filepath.Join(t.TempDir(), "no-such-ffmpeg")

	if _, err := b.Apply(ctx, broken); err == nil {
		t.Fatal("expected Apply to fail for a driver that cannot run")
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the working mjpeg source to be restored", got)
	}

	before := frames.Stats().Published
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames.Stats().Published > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the restored source is not producing frames")
}

// 書き込みを失うことと変更を失うことは違うが、それでも呼び出し側には伝えなければ
// ならない。今設定したものは再起動を越えない。
func TestBridgeApplyReportsASaveFailure(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	// 設定ファイルがあるべき場所にディレクトリを置くと、実行の他の部分は普通の
	// まま rename だけが失敗する。
	path := filepath.Join(t.TempDir(), config.FileName)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Transform.Rotate = 180

	_, err := b.Apply(ctx, updated)
	if !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("Apply error = %v, want one wrapping config.ErrNotSaved", err)
	}
	// 失敗したのは書き込みだけなので、変更自体は有効なまま。
	if got := b.Snapshot().Transform.Rotate; got != 180 {
		t.Errorf("rotate = %d, want the change to still be active", got)
	}
}

// 動作中のプロセスが採用できない設定は拒否する。受け入れてファイルに書き、その
// ファイルが動いているものと食い違う、という形にはしない。
func TestBridgeApplyRejectsSettingsThatNeedARestart(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	original := b.Snapshot().Server.Listen
	moved := b.Snapshot()
	moved.Server.Listen = "127.0.0.1:19999"

	deferred, err := b.Apply(ctx, moved)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 動作中のブリッジは古いアドレスのまま。すでに bind してあるものは動かせない。
	if got := b.Snapshot().Server.Listen; got != original {
		t.Errorf("listen = %q, want it left at %q", got, original)
	}
	// そして黙って無視してはいけない。呼び出し側が「保存はされたが今は効いて
	// いない」と言えなければ、値が変わったのに動きが変わらない理由が誰にも
	// 分からなくなる。
	if !slices.Contains(deferred, "server.listen") {
		t.Errorf("deferred = %v, want it to name server.listen", deferred)
	}
}

// streamConfiguratorFunc は関数を StreamConfigurator に適合させる。
type streamConfiguratorFunc func(core.MultipartEncoder, bool)

func (f streamConfiguratorFunc) SetStreamOptions(enc core.MultipartEncoder, hold bool) {
	f(enc, hold)
}

// boundary と追加ヘッダーは、今の PaperTracker クライアントが解析するものに合わせる
// ために存在する。だからその変更はサーバ自身まで届かなければならない。
func TestBridgeApplyReconfiguresTheStream(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())

	var gotBoundary string
	var gotHold bool
	b.SetStreamConfigurator(streamConfiguratorFunc(func(enc core.MultipartEncoder, hold bool) {
		gotBoundary, gotHold = enc.Boundary(), hold
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Server.Boundary = "othermark"
	updated.Server.HoldOnSourceLoss = true
	if _, err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if gotBoundary != "othermark" || !gotHold {
		t.Errorf("server was given boundary %q hold %v, want othermark true", gotBoundary, gotHold)
	}
}

func TestBridgeApplyPersistsSettings(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Transform.Rotate = 180
	if _, err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Transform.Rotate != 180 {
		t.Errorf("saved rotate = %d, want 180", reloaded.Transform.Rotate)
	}
}

func TestBridgeApplyRejectsInvalidSettings(t *testing.T) {
	b := New(config.Default(), "", hub.New(), status.New(), discardLogger())

	invalid := config.Default()
	invalid.Server.Listen = "not-an-address"
	if _, err := b.Apply(context.Background(), invalid); err == nil {
		t.Fatal("expected invalid settings to be rejected")
	}
}

// 停止はドライバの終了を待たなければならない。UVC では、それが「何かが開き直す前に
// 排他的なデバイスハンドルが解放されている」ことの保証になる。
func TestBridgeStopWaitsForTheDriver(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFrame(t, frames, 5*time.Second)

	b.Stop()
	published := frames.Stats().Published
	time.Sleep(100 * time.Millisecond)
	if after := frames.Stats().Published; after != published {
		t.Errorf("%d frames arrived after Stop returned, so the driver was still running", after-published)
	}

	b.Stop() // must be safe to call twice
}

func TestBridgeStartTwiceIsAnError(t *testing.T) {
	b := New(config.Default(), "", hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 既定のソースはデバイス名の無い UVC なので起動は失敗する。それはソースの
	// エラーであって、生存期間のエラーではない。
	_ = b.Start(ctx)
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected the second Start to be rejected")
	}
}

// shortenVerify は、起動時の検証がテストの実行時間を占領しないようにする。
func shortenVerify(t *testing.T, d time.Duration) {
	t.Helper()
	previous := startVerifyTimeout
	startVerifyTimeout = d
	t.Cleanup(func() { startVerifyTimeout = previous })
}

// ソースが機能することを証明するのはフレームだけ。起動し、カメラに届かず、再接続に
// 落ち着いたドライバは、ユーザーが使えるものを何も始めていない。だから Apply は
// その設定を残してはいけない。
func TestBridgeApplyRevertsWhenTheNewSourceNeverDelivers(t *testing.T) {
	shortenVerify(t, 1500*time.Millisecond)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	// 接続は受け入れるが、その後何も言わない上流。報告すべきエラーも、フレームも
	// 出てこない。
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer silent.Close()

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	broken := b.Snapshot()
	broken.Source.MJPEG.URL = silent.URL

	if _, err := b.Apply(ctx, broken); err == nil {
		t.Fatal("expected Apply to fail for a source that never produced a frame")
	}
	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the working upstream restored", got)
	}

	before := frames.Stats().Published
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames.Stats().Published > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the restored source is not producing frames")
}

// 検証はそもそも、変更を求めたリクエストのために走っている。それが居なくなった後 —
// 切断したクライアントや HTTP のタイムアウト — にそれでも変更を完了させると、
// ブリッジと設定ファイルは、呼び出し側が何も知らされず、しかも失敗したと告げられた
// ソースの上に残る。
func TestBridgeApplyStopsWhenTheRequestIsCancelled(t *testing.T) {
	// たまたま通ってしまうのではなく、テストがそこで止まる程度には長く。
	shortenVerify(t, time.Minute)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	// 接続は受け入れ、その後何も言わない。検証は待つことになる。
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer silent.Close()

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	req, cancelReq := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancelReq)
	defer cancelReq()

	broken := b.Snapshot()
	broken.Source.MJPEG.URL = silent.URL

	done := make(chan error, 1)
	go func() { _, err := b.Apply(req, broken); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Apply to fail once the request was cancelled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Apply error = %v, want it to carry context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Apply kept verifying after the request that asked for it had gone")
	}

	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the working upstream restored", got)
	}
}

// フレームとリクエストの期限は同時に準備完了になり得て、その場合 select はどちらを
// 取ってもおかしくない。つまりフレームの case を読んだ検証は、まだ誰かが答えを
// 待っている証拠にはならない。それを根拠に変更を残すと、失敗を告げられたクライアントの
// 背後で設定ファイルに書くことになり、どちらが起きるかはコイン投げになる。
//
// 競合そのものはテストから作れない。2 つの事象が同じ瞬間に準備完了になる必要が
// あり、外からそれを起こそうとすれば、select がどちらを先に見るかまで決めてしまう。
// テストできるのは select が渡す先の判断であり、それを関数にしてあるのはそのため。
func TestVerifyOutcomeRejectsAFrameNobodyIsWaitingFor(t *testing.T) {
	cancelled := context.Canceled
	deadline := context.DeadlineExceeded

	if err := verifyOutcome("mjpeg", nil, nil, nil); err != nil {
		t.Errorf("a good frame with the request still there = %v, want success", err)
	}

	err := verifyOutcome("mjpeg", nil, cancelled, nil)
	if err == nil {
		t.Fatal("a frame that arrived after the request had gone was accepted")
	}
	if !errors.Is(err, cancelled) {
		t.Errorf("error = %v, want it to carry the request's own error", err)
	}

	if err := verifyOutcome("mjpeg", nil, deadline, nil); !errors.Is(err, deadline) {
		t.Errorf("a frame that arrived after the deadline = %v, want it rejected", err)
	}

	// 停止中であることも証拠にはならない。その設定の上では何も動かないが、次の
	// 起動はその上で立ち上がる。
	if err := verifyOutcome("mjpeg", nil, nil, context.Canceled); err == nil {
		t.Error("a frame delivered as the bridge stopped was accepted")
	}

	// 使えないフレームはやはり何も無いのに等しく、その理由を述べる。
	if err := verifyOutcome("mjpeg", errors.New("not a JPEG"), nil, nil); err == nil {
		t.Error("an undecodable frame was accepted")
	}
}

// 同じことを端から端まで。クライアントが諦めた後になってソースが立ち上がる。
// ブリッジは動いていた方のソースの上に残らなければならない。
func TestBridgeApplyRollsBackASourceThatCameUpTooLate(t *testing.T) {
	shortenVerify(t, time.Minute)
	jpg := testJPEG(t, 16, 16)
	upstream := mjpegUpstream(t, jpg)

	connected := make(chan struct{}, 1)
	release := make(chan struct{})
	late := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case connected <- struct{}{}:
		default:
		}
		// テストが指示するまで何も送らない。指示するのは、このソースを求めた
		// リクエストがキャンセルされた後だけ。
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpg))
		w.Write(jpg)
		fmt.Fprint(w, "\r\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer late.Close()

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	req, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	next := b.Snapshot()
	next.Source.MJPEG.URL = late.URL

	done := make(chan error, 1)
	go func() { _, err := b.Apply(req, next); done <- err }()

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the new source never connected")
	}
	cancelReq()
	close(release)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Apply kept a source that only came up after the request had gone")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Apply never returned")
	}

	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the working upstream still in place", got)
	}
	if !b.captureRunningForTest() {
		t.Error("the bridge was left with nothing running")
	}
}

// サーバ設定だけを触る変更は検証に到達しないので、リクエストのコンテキストは入口でも
// 確認しなければならない。ロック待ちは別の呼び出し側の検証まるごと分の長さになり得て、
// PUT が諦めるのはまさにそのとき。
func TestBridgeApplyRejectsAnAlreadyCancelledRequest(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	// サーバ設定だけなので captureUnchanged が成り立ち、ソースは再起動されない。
	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = true

	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()

	if _, err := b.Apply(dead, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context.Canceled", err)
	}
	if b.Snapshot().Server.HoldOnSourceLoss {
		t.Error("the change was applied for a request that had already gone")
	}
}

// 上流のパーサーが構造を見るのは、フレームごとに許されるのがそこまでだから。構造は
// 画像ではないので、SOI/EOI しか出さないソースが、そうしなければ「動いている」として
// 保存され、その間トラッカーは何も受け取らない。
func TestBridgeApplyRejectsASourceSendingUndecodableFrames(t *testing.T) {
	shortenVerify(t, 3*time.Second)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	// 構造上は JPEG で、中身は空。
	hollow := mjpegUpstream(t, []byte{0xFF, 0xD8, 0xFF, 0xD9})

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	broken := b.Snapshot()
	broken.Source.MJPEG.URL = hollow.URL

	_, err := b.Apply(ctx, broken)
	if err == nil {
		t.Fatal("expected a source with no decodable image to be refused")
	}
	// タイムアウトではない。フレームは届いており、拒んだのはデコード。ここで
	// タイムアウトを許すと、単に何も届けなかったソースでもテストが通ってしまう。
	if !strings.Contains(err.Error(), "usable JPEG") {
		t.Errorf("error = %v, want the frame rejected as undecodable rather than missing", err)
	}
	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the working upstream restored", got)
	}
}

// 起動時は逆の場合。まだ存在しないカメラは再接続に任せなければならない。それを
// 二度目に起動してくれるものは無いから。
func TestBridgeStartLeavesAnUnreachableSourceRetrying(t *testing.T) {
	shortenVerify(t, 500*time.Millisecond)

	// ここでは何も listen していないので、すべての試行が失敗して再試行になる。
	cfg := mjpegConfig("http://127.0.0.1:1/")
	frames := hub.New()
	b := New(cfg, "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start reported a failure for a source that should keep retrying: %v", err)
	}
	defer b.Stop()

	// ドライバは生きていなければならない。これを止めることが、後から挿された
	// カメラが一度も拾われない原因になる。
	time.Sleep(750 * time.Millisecond)
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the configured mjpeg source", got)
	}
	if b.stopped == nil {
		t.Error("no driver is running after Start; a source appearing later would never be picked up")
	}
}

// 一時停止中は何も動かないが、それでも Apply は起動できない設定を拒否しなければ
// ならない。さもないと後の再開が失敗し、そのとき動いていた元の設定は既に失われている。
func TestBridgeApplyValidatesTheDriverWhilePaused(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	unbuildable := b.Snapshot()
	unbuildable.Source.Type = config.SourceUVC
	unbuildable.Source.UVC.Device = "" // no device: the driver refuses to build

	if _, err := b.Apply(ctx, unbuildable); err == nil {
		t.Fatal("expected settings that cannot build a driver to be rejected while paused")
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source type = %q, want the previous settings kept", got)
	}

	// したがって再開は依然として成功しなければならない。
	if err := b.SetPaused(false); err != nil {
		t.Errorf("resume after a rejected change: %v", err)
	}
}

// HTTP サーバが受け持つ設定だけを変えることが、カメラを中断させてはいけない。再起動
// すれば何の得も無くストリームが切れるし、カメラがたまたま再接続中であれば起動時の
// 検証が変更を丸ごと拒否する。hold_on_source_loss を触るのは、まさにそのときだ。
func TestBridgeApplyDoesNotRestartCaptureForServerOnlySettings(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	frames := hub.New()
	tracker := status.New()
	b := New(mjpegConfig(upstream.URL), "", frames, tracker, discardLogger())

	var gotHold bool
	b.SetStreamConfigurator(streamConfiguratorFunc(func(_ core.MultipartEncoder, hold bool) {
		gotHold = hold
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	// 変更前にあったドライバの実体が、変更後も同じでなければならない。再起動すれば
	// 置き換わる。
	before := b.stopped

	updated := b.Snapshot()
	updated.Server.HoldOnSourceLoss = true
	if _, err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply of a server-only change: %v", err)
	}

	if b.stopped != before {
		t.Error("the capture source was restarted for a change it does not depend on")
	}
	if !gotHold {
		t.Error("the server was not told about the new hold_on_source_loss")
	}
	if !b.Snapshot().Server.HoldOnSourceLoss {
		t.Error("the change was not kept")
	}
}

// ドライバは 1 枚解析した時点でフレームを告げるが、そこと hub の間には変換段がある。
// そこで落とされたフレームは、どのクライアントにも何も見えないということなので、
// 動いているソースとして数えてはいけない。
func TestBridgeApplyRevertsWhenTheTransformDropsEveryFrame(t *testing.T) {
	shortenVerify(t, 1500*time.Millisecond)
	good := mjpegUpstream(t, testJPEG(t, 16, 16))

	// 構造上は妥当な JPEG — ドライバは解析して流す — だが、ヘッダーが
	// 65535x65535 を主張しているので、変換段はデコードを拒む。
	oversized := bytes.Clone(testJPEG(t, 16, 16))
	sof := bytes.Index(oversized, []byte{0xFF, 0xC0})
	if sof < 0 {
		t.Fatal("fixture has no baseline SOF0 to rewrite")
	}
	copy(oversized[sof+5:sof+9], []byte{0xFF, 0xFF, 0xFF, 0xFF})
	huge := mjpegUpstream(t, oversized)

	frames := hub.New()
	cfg := mjpegConfig(good.URL)
	cfg.Transform.Rotate = 90
	b := New(cfg, "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	broken := b.Snapshot()
	broken.Source.MJPEG.URL = huge.URL

	if _, err := b.Apply(ctx, broken); err == nil {
		t.Fatal("expected Apply to fail when no frame survives the transform")
	}
	if got := b.Snapshot().Source.MJPEG.URL; got != good.URL {
		t.Errorf("URL = %q, want the working upstream restored", got)
	}
}

// 検証の最中に落ちた停止は何の証拠にもならないし、誰も確認していない設定を保存しては
// いけない。
func TestBridgeApplyFailsWhenShutdownInterruptsVerification(t *testing.T) {
	shortenVerify(t, 10*time.Second)
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))

	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer silent.Close()

	path := filepath.Join(t.TempDir(), config.FileName)
	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), path, frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	// Apply がまだ無言のソースを待っている間に停止する。
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	unverified := b.Snapshot()
	unverified.Source.MJPEG.URL = silent.URL
	if _, err := b.Apply(ctx, unverified); err == nil {
		t.Fatal("expected a shutdown during verification to fail the apply")
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		saved, _ := config.Load(path)
		t.Errorf("an unverified configuration was persisted: %q", saved.Source.MJPEG.URL)
	}
}

// 一度きりの実行のための上書きが、無関係なものを変えた最初の瞬間に恒久化しては
// いけない。
func TestBridgeApplySavesOnlyWhatChanged(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	// ファイルが述べている内容。
	fileCfg := mjpegConfig(upstream.URL)
	fileCfg.Source.UVC.Device = "the camera the user configured"

	// -device による上書きの後、この実行が実際に使っている内容。
	effective := fileCfg
	effective.Source.UVC.Device = "just for this run"

	if err := config.Save(path, fileCfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(effective, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(fileCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// まったく別のものを変更する。
	updated := b.Snapshot()
	updated.Server.HoldOnSourceLoss = true
	if _, err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the change the caller made was not saved")
	}
	if got := saved.Source.UVC.Device; got != "the camera the user configured" {
		t.Errorf("saved device = %q, want the run override left out of the file", got)
	}
}

// 上書きが呼び出し側の変更したものそのものである場合は、当然保存されなければならない。
func TestBridgeApplySavesADeliberateChangeToAnOverriddenField(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	fileCfg := mjpegConfig(upstream.URL)
	fileCfg.Source.UVC.Device = "from the file"
	effective := fileCfg
	effective.Source.UVC.Device = "from the command line"

	b := New(effective, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(fileCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Source.UVC.Device = "picked in the tray"
	if _, err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := saved.Source.UVC.Device; got != "picked in the tray" {
		t.Errorf("saved device = %q, want the deliberate change", got)
	}
}

// 動いていないソースの設定は、カメラがしていることを何も表していない。切替に備えて
// それを埋めることが、カメラを中断させてはいけない。
func TestCaptureUnchangedIgnoresTheInactiveSources(t *testing.T) {
	base := mjpegConfig("http://camera.invalid/")

	preparing := base
	preparing.Source.UVC.Device = "a camera to switch to later"
	preparing.Source.Serial.Port = "COM7"
	if !captureUnchanged(base, preparing) {
		t.Error("filling in an inactive source was treated as a capture change")
	}

	switched := base
	switched.Source.MJPEG.URL = "http://other.invalid/"
	if captureUnchanged(base, switched) {
		t.Error("changing the active source's URL was not treated as a capture change")
	}

	resized := base
	resized.Source.MaxFrameSize = 1 << 20
	if captureUnchanged(base, resized) {
		t.Error("changing max_frame_size was not treated as a capture change")
	}

	rotated := base
	rotated.Transform.Rotate = 90
	if captureUnchanged(base, rotated) {
		t.Error("changing the transform was not treated as a capture change")
	}
}

// 失敗した保存は「これは再起動で失われる」と報告される。それでも基点を進めると、
// その変更が次の成功した保存に畳み込まれ、あのエラーが一時的だと約束したものが
// そのまま書き出される。
// startWithAnUnwritableConfig は、設定ファイルを書けないブリッジを走らせ、障害物を
// 取り除いたうえでパスと一緒に返す。呼び出し側が、次の保存が何をするかを観察できる
// ようにするため。
func startWithAnUnwritableConfig(t *testing.T, ctx context.Context) (*Bridge, string) {
	t.Helper()

	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	// ファイルがあるべき場所のディレクトリが rename を失敗させる。
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(b.Stop)

	lost := b.Snapshot()
	lost.Server.Boundary = "unwritable"
	if _, err := b.Apply(ctx, lost); !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("Apply error = %v, want config.ErrNotSaved", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the obstruction: %v", err)
	}
	return b, path
}

// 適用されたが保存されなかった変更に対して、再試行するのは当然の手であり、それは
// 実際に書けなければならない。再試行が送るのは動作中のブリッジが既に持っている設定
// なので、それとの差分は無い。ファイルが遅れていることを示す唯一の記録が、保留中の
// 書き込みだ。
func TestBridgeApplyRetryPersistsAChangeThatFailedToSave(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, path := startWithAnUnwritableConfig(t, ctx)

	if _, err := b.Apply(ctx, b.Snapshot()); err != nil {
		t.Fatalf("retrying the same settings: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Server.Boundary != "unwritable" {
		t.Errorf("saved boundary = %q, want the change the retry was asking to persist", saved.Server.Boundary)
	}
}

// 同じ保留中の書き込みは、次の無関係な変更にも相乗りする。動作中のブリッジはずっと
// その値を使ってきたので、置き去りにすればファイルは、有効ではない設定を記述し
// 続けることになる。
func TestBridgeApplyCarriesAnUnsavedChangeIntoTheNextSave(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, path := startWithAnUnwritableConfig(t, ctx)

	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = true
	if _, err := b.Apply(ctx, next); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the change that did save is missing from the file")
	}
	if saved.Server.Boundary != "unwritable" {
		t.Errorf("saved boundary = %q, want the earlier change that is still in effect", saved.Server.Boundary)
	}
}

// 設定ファイルを書くのはここだけではない。トレイには「設定を編集」があり、再起動を
// 要する値はその方法でしか変えられない。起動時のファイルを土台にした保存は、他の
// 何かが変わった瞬間にその編集を上書きしてしまう。
func TestBridgeApplyKeepsAnEditMadeToTheFileWhileRunning(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	fileCfg := mjpegConfig(upstream.URL)
	if err := config.Save(path, fileCfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(fileCfg, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(fileCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// ユーザーが手でファイルを編集する。server.listen はこの方法でしか変えられない
	// 設定の 1 つであり、だからこそ失われやすい。
	edited := fileCfg
	edited.Server.Listen = "127.0.0.1:19999"
	edited.Source.UVC.Device = "picked while running"
	if err := config.Save(path, edited); err != nil {
		t.Fatalf("Save the edit: %v", err)
	}

	// そして再起動する前に、トレイから無関係なものを変更する。
	updated := b.Snapshot()
	updated.Server.HoldOnSourceLoss = true
	if _, err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the change the caller made was not saved")
	}
	if saved.Server.Listen != "127.0.0.1:19999" {
		t.Errorf("saved listen = %q, want the hand edit kept", saved.Server.Listen)
	}
	if saved.Source.UVC.Device != "picked while running" {
		t.Errorf("saved device = %q, want the hand edit kept", saved.Source.UVC.Device)
	}
}

// extra_headers は、独立した設定の集まりがたまたま 1 つのテーブルとして書かれている
// もの。これを 1 つの値として扱うと、API 越しにヘッダーを 1 つ変えたことで、ユーザーが
// ファイルに足したヘッダーが消える。葉ごとの併合は、まさにそれを防ぐために存在する。
func TestBridgeApplyKeepsHeadersAddedToTheFileByHand(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	fileCfg := mjpegConfig(upstream.URL)
	fileCfg.Server.ExtraHeaders = map[string]string{"X-Original": "kept"}
	if err := config.Save(path, fileCfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(fileCfg, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(fileCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// ブリッジが動いている間に、ユーザーが手でファイルにヘッダーを足す。
	edited := fileCfg
	edited.Server.ExtraHeaders = map[string]string{"X-Original": "kept", "X-Added-By-Hand": "also kept"}
	if err := config.Save(path, edited); err != nil {
		t.Fatalf("Save the edit: %v", err)
	}

	// そして API 越しに別のヘッダーを変更する。API 側が持つテーブルの写しは、その
	// 編集より前のもの。
	updated := b.Snapshot()
	updated.Server.ExtraHeaders = map[string]string{"X-Original": "changed"}
	if _, err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.Server.ExtraHeaders["X-Original"] != "changed" {
		t.Errorf("X-Original = %q, want the change the caller made", saved.Server.ExtraHeaders["X-Original"])
	}
	if saved.Server.ExtraHeaders["X-Added-By-Hand"] != "also kept" {
		t.Errorf("the header added to the file by hand was dropped: %v", saved.Server.ExtraHeaders)
	}
}

// ヘッダーの削除も、他と同じくそのキーに対する変更であり、ファイルまで届かなければ
// ならない。
func TestBridgeApplyRemovesAHeaderTheCallerDropped(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	fileCfg := mjpegConfig(upstream.URL)
	fileCfg.Server.ExtraHeaders = map[string]string{"X-One": "1", "X-Two": "2"}
	if err := config.Save(path, fileCfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(fileCfg, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(fileCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Server.ExtraHeaders = map[string]string{"X-One": "1"}
	if _, err := b.Apply(ctx, updated); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if _, still := saved.Server.ExtraHeaders["X-Two"]; still {
		t.Errorf("the header the caller removed is still in the file: %v", saved.Server.ExtraHeaders)
	}
	if saved.Server.ExtraHeaders["X-One"] != "1" {
		t.Errorf("the header the caller kept was lost: %v", saved.Server.ExtraHeaders)
	}
}

// 一時停止中は何も起動できないので、何も証明できない。それでも変更を受け取ると、
// 動いている設定を未検証のものと取り換えて書き出すことになるし、再開もまた検証しない。
func TestBridgeApplyRejectsASourceChangeWhilePaused(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	other := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	cfg := mjpegConfig(upstream.URL)
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	frames := hub.New()
	b := New(cfg, path, frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}

	next := b.Snapshot()
	next.Source.MJPEG.URL = other.URL
	if _, err := b.Apply(ctx, next); err == nil {
		t.Fatal("expected a source change to be refused while paused")
	}
	if got := b.Snapshot().Source.MJPEG.URL; got != upstream.URL {
		t.Errorf("URL = %q, want the settings left alone", got)
	}

	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.Source.MJPEG.URL != upstream.URL {
		t.Errorf("saved URL = %q, want the unverified change kept out of the file", saved.Source.MJPEG.URL)
	}
}

// 一時停止はカメラを解放するためのものなので、カメラに触れない設定は一時停止中でも
// 変更できなければならない。ソースが無いときにどうするか、という設定も含めて。
func TestBridgeApplyAllowsAServerChangeWhilePaused(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}

	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = true
	if _, err := b.Apply(ctx, next); err != nil {
		t.Fatalf("Apply a server-only change while paused: %v", err)
	}
	if !b.Snapshot().Server.HoldOnSourceLoss {
		t.Error("the server-only change was not applied")
	}
}

// 解析できない設定ファイルは、ユーザーが編集の途中である可能性が最も高い。既に
// 有効になっている変更を保存するためにそれを上書きすることは、少し後でも書ける
// ものと引き換えに、その編集を捨てることになる。
func TestBridgeApplyWillNotOverwriteAnUnparsableFile(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	cfg := mjpegConfig(upstream.URL)
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(cfg, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// 編集の途中。打ちかけのテーブルヘッダー。
	halfEdited := "[server\nlisten = '127.0.0.1:18080'\n"
	if err := os.WriteFile(path, []byte(halfEdited), 0o644); err != nil {
		t.Fatalf("write the half-edited file: %v", err)
	}

	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = true
	_, err := b.Apply(ctx, next)
	if !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("Apply error = %v, want config.ErrNotSaved", err)
	}

	// 書けなかったにもかかわらず、変更は有効になっている。
	if !b.Snapshot().Server.HoldOnSourceLoss {
		t.Error("the change was not applied to the running settings")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(onDisk) != halfEdited {
		t.Errorf("the file was rewritten:\n%s", onDisk)
	}

	// ファイルが再び解析できるようになれば、保持していた変更が書かれる。
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save the finished edit: %v", err)
	}
	if _, err := b.Apply(ctx, b.Snapshot()); err != nil {
		t.Fatalf("retry after the edit was finished: %v", err)
	}
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the held change was not written once the file could be read")
	}
}

// ドライバは、再試行では直らないエラーで自力で止まることがある。それを再起動する
// ものは無く、それを生んだ設定は今も現在の設定のままなので、原因に対処した後で同じ
// ソースを選び直すのがユーザーの復帰手段になる。設定が同じだからと再起動を省けば、
// それに成功で応えたうえで、ブリッジは何も出さないまま残る。
func TestBridgeApplyRestartsASourceThatDiedOnItsOwn(t *testing.T) {
	shortenVerify(t, 5*time.Second)
	jpg := testJPEG(t, 16, 16)

	// 404 は MJPEG ドライバにとって致命的。再接続せず停止する。
	var serving atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serving.Load() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for {
			fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpg))
			if _, err := w.Write(jpg); err != nil {
				return
			}
			fmt.Fprint(w, "\r\n")
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	defer upstream.Close()

	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// ドライバが諦めるのを待つ。
	deadline := time.Now().Add(5 * time.Second)
	for b.captureRunningForTest() {
		if time.Now().After(deadline) {
			t.Fatal("the driver did not stop on a 404")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := frames.Latest(); ok {
		t.Fatal("a frame arrived from an upstream that was answering 404")
	}

	// 原因に対処し、ユーザーが同じソースをもう一度選ぶ。
	serving.Store(true)
	if _, err := b.Apply(ctx, b.Snapshot()); err != nil {
		t.Fatalf("re-applying the same settings after a fatal stop: %v", err)
	}
	waitForFrame(t, frames, 5*time.Second)
}

// そもそもドライバを生み出せない設定は、再試行ではなく行き止まり。再接続すべき
// ものが動いていない。記録しなければトレイは "connecting..." と表示し、/healthz は
// ソースが未接続だとしか言わない。どちらも「試している何か」を描写している。既定の
// 設定は UVC のデバイスを指定していないので、新しいユーザーが最初に出会うのがこれ。
func TestBridgeRecordsSettingsThatCannotBuildADriver(t *testing.T) {
	cfg := config.Default() // uvc, with no device name
	tracker := status.New()
	b := New(cfg, "", hub.New(), tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected settings with no capture device to fail")
	}
	defer b.Stop()

	snapshot := tracker.Snapshot()
	if snapshot.Connected {
		t.Error("the status says connected with no driver built")
	}
	if snapshot.Source != config.SourceUVC {
		t.Errorf("source = %q, want the selected type so the reason has something to hang on", snapshot.Source)
	}
	if snapshot.LastError == "" {
		t.Fatal("nothing was recorded, so the tray and /healthz show a source that is merely connecting")
	}
	if !strings.Contains(snapshot.LastError, "no camera configured") {
		t.Errorf("last error = %q, want it to name what is missing", snapshot.LastError)
	}
}

// 設定ファイルが解析できない間に行われた 2 つの変更は、どちらも生き延びなければ
// ならない。
//
// 2 つ目は 1 つ目との差分が無い。その時点で動作中の設定が既にそれを持っているから。
// つまり 1 つ目の変更が存在することを示す唯一の記録が保留中の書き込みであり、先の
// 分を持ち越さずにそれを組み立て直せば、ファイルが再び読めるようになった瞬間に
// 失われる。
func TestBridgeKeepsEveryUnsavedChangeWhileTheFileIsBroken(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	path := filepath.Join(t.TempDir(), config.FileName)

	cfg := mjpegConfig(upstream.URL)
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(cfg, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	if err := os.WriteFile(path, []byte("[server\n"), 0o644); err != nil {
		t.Fatalf("break the file: %v", err)
	}

	first := b.Snapshot()
	first.Server.Boundary = "first-change"
	if _, err := b.Apply(ctx, first); !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("first Apply = %v, want config.ErrNotSaved", err)
	}

	second := b.Snapshot()
	second.Server.HoldOnSourceLoss = true
	if _, err := b.Apply(ctx, second); !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("second Apply = %v, want config.ErrNotSaved", err)
	}

	// ユーザーが編集を終え、次の変更がすべてを書き出す。
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("repair the file: %v", err)
	}
	if _, err := b.Apply(ctx, b.Snapshot()); err != nil {
		t.Fatalf("Apply once the file is readable again: %v", err)
	}

	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.Server.Boundary != "first-change" {
		t.Errorf("saved boundary = %q, want the first change kept", saved.Server.Boundary)
	}
	if !saved.Server.HoldOnSourceLoss {
		t.Error("the second change was not saved")
	}
}

// 保留中の書き込みは設定全体を運んでおり、その大半は保存が失敗した時点のファイルの
// 写しでしかない。ファイルの上に敷き直してよいのは、ブリッジ自身が変えた葉だけ。
// 残りはその後ユーザーが編集したものに道を譲らなければならない。さもないと再試行が
// 黙ってそれらを元に戻す。
func TestBridgeUnsavedChangesDoNotRevertLaterFileEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), config.FileName)

	// 保存が失敗した時点でファイルが持っていた内容。
	whenItFailed := config.Default()
	whenItFailed.Server.Listen = "127.0.0.1:11111"

	// その保存が書こうとしていた内容。同じファイルに、ブリッジが変更を求められた
	// boundary を加えたもの。
	wanted := whenItFailed
	wanted.Server.Boundary = "from-the-bridge"

	// その後ユーザーがファイルをまた編集している。
	editedSince := config.Default()
	editedSince.Server.Listen = "127.0.0.1:22222"
	editedSince.Source.UVC.Device = "picked by hand"
	if err := config.Save(path, editedSince); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := New(config.Default(), path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(config.Default())
	b.holdPendingForTest(whenItFailed, wanted)

	base, err := b.saveBaseForTest()
	if err != nil {
		t.Fatalf("saveBase: %v", err)
	}

	if base.Server.Boundary != "from-the-bridge" {
		t.Errorf("boundary = %q, want the pending change carried forward", base.Server.Boundary)
	}
	if base.Server.Listen != "127.0.0.1:22222" {
		t.Errorf("listen = %q, want the newer hand edit, not the value from when the save failed", base.Server.Listen)
	}
	if base.Source.UVC.Device != "picked by hand" {
		t.Errorf("device = %q, want the newer hand edit kept", base.Source.UVC.Device)
	}
}

// 書き込みが保留されている間に削除された設定ファイルは、それについて分かっている
// 最も新しいもの — 起動時の写しではなく、保留中の書き込みが記録した内容 — から
// 作り直さなければならない。さもないと、ファイルが消える前に行われた編集が、
// 取り消された形で戻ってくる。
func TestBridgeRecreatesADeletedFileFromTheNewestKnownContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), config.FileName)
	// 意図的に書かない。ファイルは消えている。

	whenItFailed := config.Default()
	whenItFailed.Server.Listen = "127.0.0.1:11111"
	whenItFailed.Source.UVC.Device = "edited by hand"

	wanted := whenItFailed
	wanted.Server.Boundary = "from-the-bridge"

	b := New(config.Default(), path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(config.Default())
	b.holdPendingForTest(whenItFailed, wanted)

	base, err := b.saveBaseForTest()
	if err != nil {
		t.Fatalf("saveBase: %v", err)
	}

	if base.Server.Boundary != "from-the-bridge" {
		t.Errorf("boundary = %q, want the pending change carried forward", base.Server.Boundary)
	}
	if base.Server.Listen != "127.0.0.1:11111" {
		t.Errorf("listen = %q, want the hand edit the pending write recorded", base.Server.Listen)
	}
	if base.Source.UVC.Device != "edited by hand" {
		t.Errorf("device = %q, want the hand edit the pending write recorded", base.Source.UVC.Device)
	}
}

func shortenRecheck(t *testing.T, d time.Duration) {
	t.Helper()
	previous := undecodableRecheckInterval
	undecodableRecheckInterval = d
	t.Cleanup(func() { undecodableRecheckInterval = previous })
}

// 構造は画像ではない。SOI/EOI を送るソースはフレームごとの検査をすべて通るので、
// デコードが無ければ hub はそれを運び、/healthz は 200 を返し、トレイには fps が
// 出る。どれもトラッカーが使えないストリームを描写している。1 枚デコードできるまで
// 何も配信しない。
func TestPumpPublishesNothingUntilAFrameDecodes(t *testing.T) {
	shortenRecheck(t, 0)

	jpg := testJPEG(t, 16, 16)
	hollow := []byte{0xFF, 0xD8, 0xFF, 0xD9}

	frames := make(chan core.Frame, 4)
	frames <- core.Frame{Data: hollow}
	frames <- core.Frame{Data: hollow}
	close(frames)

	h := hub.New()
	var reported []error
	pump(frames, core.Transform{}, h, discardLogger(), 0, func(err error) { reported = append(reported, err) })

	if _, ok := h.Latest(); ok {
		t.Error("a frame with no image in it reached the hub")
	}
	if len(reported) != 1 || reported[0] == nil {
		t.Errorf("first frame reported as %v, want the decode failure exactly once", reported)
	}

	// そして本物が届けば、流れ出す。
	frames = make(chan core.Frame, 4)
	frames <- core.Frame{Data: hollow}
	frames <- core.Frame{Data: jpg}
	close(frames)

	h = hub.New()
	reported = nil
	pump(frames, core.Transform{}, h, discardLogger(), 0, func(err error) { reported = append(reported, err) })

	got, ok := h.Latest()
	if !ok {
		t.Fatal("the decodable frame never reached the hub")
	}
	if !bytes.Equal(got.Data, jpg) {
		t.Error("the frame on the hub is not the one that decoded")
	}
	if len(reported) != 1 {
		t.Errorf("the first frame was reported %d times, want once", len(reported))
	}
}

// そのまま流すフレームはここでデコードされないので、ピクセルの上限は代わりに
// ヘッダーへ適用する。デコードするのはクライアントであり、巨大な画像を宣言する
// 数百バイトは、この上限が拒むために存在するその確保を、クライアントに要求する。
func TestPumpDropsAnOversizedFrameEvenWithNoTransform(t *testing.T) {
	shortenRecheck(t, 0)

	jpg := testJPEG(t, 16, 16)
	huge := hugeDimensions(t, testJPEG(t, 16, 16))

	frames := make(chan core.Frame, 4)
	// まず本物のフレームを 1 枚。デコードの関門を抜けて、ここで問題にしている
	// フレームごとの経路に入るため。
	frames <- core.Frame{Data: jpg}
	frames <- core.Frame{Data: huge}
	close(frames)

	h := hub.New()
	pump(frames, core.Transform{}, h, discardLogger(), 0, func(error) {})

	got, ok := h.Latest()
	if !ok {
		t.Fatal("the good frame never reached the hub")
	}
	if !bytes.Equal(got.Data, jpg) {
		t.Error("a frame declaring an image over the pixel limit was forwarded to the client")
	}
}

// hugeDimensions は、フレームのヘッダーを書き換えてピクセル上限をはるかに超える
// 画像を宣言させる。圧縮データの大きさは元のまま。それがこの問題の形そのもの。
// バイト数は、デコードが何を要求するかについて何も語らない。
func hugeDimensions(t *testing.T, jpg []byte) []byte {
	t.Helper()
	out := bytes.Clone(jpg)
	for i := 0; i+9 < len(out); i++ {
		// SOF0。FF C0、長さ、精度、そして高さと幅。
		if out[i] == 0xFF && out[i+1] == 0xC0 {
			out[i+5], out[i+6] = 0xFF, 0xFF
			out[i+7], out[i+8] = 0xFF, 0xFF
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("the patched header does not parse: %v", err)
			}
			if cfg.Width != 65535 || cfg.Height != 65535 {
				t.Fatalf("patched header reads %dx%d, want 65535x65535", cfg.Width, cfg.Height)
			}
			return out
		}
	}
	t.Fatal("no SOF0 marker in the fixture")
	return nil
}

// カメラより遅い変換は、そうしなければドライバを満杯の枠の上でブロックさせる。
// ブロックしたドライバはソケットを読むのをやめるので、画像は代わりにそちらへ並び、
// その 1 枚ごとにストリームは実時間からさらに遅れる。口の動きを追う用途で保つ価値が
// あるのは最新のフレームだけ。
func TestLatestOnlyKeepsOnlyTheNewestFrameWaiting(t *testing.T) {
	in := make(chan core.Frame)
	out := latestOnly(in, discardLogger())

	// まだ誰も読んでいない状態でフレームを 3 枚。1 枚目が枠を埋め、残りの 2 枚は
	// 待っているものを置き換える。
	for i := 1; i <= 3; i++ {
		in <- core.Frame{Seq: uint64(i)}
	}
	// 転送側がまだ最後の 1 枚を運んでいる最中かもしれない。
	deadline := time.Now().Add(2 * time.Second)
	var got core.Frame
	for time.Now().Before(deadline) {
		got = <-out
		if got.Seq == 3 {
			break
		}
		in <- core.Frame{Seq: 3}
	}
	if got.Seq != 3 {
		t.Fatalf("read frame %d, want the newest one", got.Seq)
	}

	// その後ろに並んでいるものは無い。
	select {
	case extra := <-out:
		t.Errorf("frame %d was still waiting, want the older ones dropped", extra.Seq)
	case <-time.After(50 * time.Millisecond):
	}

	// ドライバのチャネルを閉じるとこちらも閉じ、それが pump を終わらせ、キャプチャの
	// goroutine を終了させる。
	close(in)
	select {
	case _, open := <-out:
		if open {
			t.Error("expected the forwarded channel to be closed")
		}
	case <-time.After(2 * time.Second):
		t.Error("the forwarded channel was never closed")
	}
}

// ドライバを変換待ちにしてはいけない。この性質のためにこの枠が存在する。下流が
// 何も読んでいない間でも送信は通る
// reading.
func TestLatestOnlyNeverBlocksTheDriver(t *testing.T) {
	in := make(chan core.Frame)
	out := latestOnly(in, discardLogger())
	defer func() {
		close(in)
		for range out {
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			in <- core.Frame{Seq: uint64(i + 1)}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the driver was blocked by a consumer that never read")
	}
}

// トレイはメニューを一度だけ、その言語が与えたラベルで組み立てる。だから言語の
// 変更は保存するが、動作中のブリッジには適用しない。適用したように振る舞えば、
// 値は変わったのに画面上の言葉は一つも変わらないという食い違いが残る。
func TestApplyDefersALanguageChangeWhileRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))
	frames := hub.New()
	b := New(mjpegConfig(upstream.URL), path, frames, status.New(), discardLogger())
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	cfg := b.Snapshot()
	cfg.UI.Language = "ja"

	deferred, err := b.Apply(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(deferred, "ui.language") {
		t.Errorf("deferred = %v, want it to name ui.language", deferred)
	}
	// 動作中の設定は変わらない。
	if got := b.Snapshot().UI.Language; got == "ja" {
		t.Error("the deferred language took effect while running")
	}
	// しかしファイルには入っていなければならない。それが「次の起動で反映される」
	// ということであり、この変更の眼目そのもの。
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.UI.Language != "ja" {
		t.Errorf("saved language = %q, want %q", saved.UI.Language, "ja")
	}
}

// 保留になる変更と、今すぐ効く変更が同じ要求に混ざっていることはある。前者だけを
// 取り除き、後者はそのまま適用しなければならない。
func TestApplyDefersOnlyWhatItMust(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))
	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	cfg := b.Snapshot()
	cfg.UI.Language = "ja"                                     // 次の起動まで待つもの
	cfg.Server.HoldOnSourceLoss = !cfg.Server.HoldOnSourceLoss // 今すぐ効くもの
	want := cfg.Server.HoldOnSourceLoss

	deferred, err := b.Apply(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(deferred) != 1 || deferred[0] != "ui.language" {
		t.Errorf("deferred = %v, want only ui.language", deferred)
	}
	if got := b.Snapshot().Server.HoldOnSourceLoss; got != want {
		t.Errorf("hold_on_source_loss = %v, want %v; a deferred setting must not hold back the rest", got, want)
	}
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.UI.Language != "ja" || saved.Server.HoldOnSourceLoss != want {
		t.Errorf("saved = %+v, want both changes on disk", saved)
	}
}

// waitFor は want が成り立つまで polling する。ドライバが何かに気づくまでの時間を
// テストが当てずっぽうで決めずに済むようにするため。
func waitFor(t *testing.T, timeout time.Duration, what string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// silentUpstream は応答した後に何も言わない。ドライバは接続して待ち続け、検証は
// そこで待てない。
func silentUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(done); ts.Close() })
	return ts
}

// 実機で起きた袋小路。そもそも起動できない設定で立ち上がったブリッジに、検証で
// 失敗する変更が来る。動いていなかった設定へ戻ろうとすると、2 つ目の無関係な理由で
// また失敗し、両方を報告すれば、呼び出し側が尋ねた方が埋もれる。より重要なのは
// その後に何ができるか。ユーザーが動くソースを選べなければならない。
func TestBridgeStaysReachableWhenTheSettingsItRevertsToNeverStarted(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)

	frames := hub.New()
	// デバイス名の無い uvc。そもそも組み立てられないドライバ。
	b := New(config.Default(), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected the unconfigured camera to fail to start")
	}
	defer b.Stop()

	// 接続はするが何も届けない変更。検証はこれを拒否する。
	silent := mjpegConfig(silentUpstream(t).URL)
	_, err := b.Apply(ctx, silent)
	if err == nil {
		t.Fatal("expected the silent upstream to fail verification")
	}
	if strings.Contains(err.Error(), "could not be restored") {
		t.Errorf("error = %v, want only the failure the caller asked about", err)
	}

	// すべての目的はここ。別のソースを選べること。
	working := mjpegUpstream(t, testJPEG(t, 32, 32))
	if _, err := b.Apply(ctx, mjpegConfig(working.URL)); err != nil {
		t.Fatalf("could not switch to a working source afterwards: %v", err)
	}
	waitForFrame(t, frames, 5*time.Second)
}

// 再開は、起動と同じくソースが機能するという主張ではない。これを失敗させると
// ブリッジは一時停止のまま残り、一時停止したブリッジは別のソースを受け付けない。
// この 2 つが揃うと、設定ファイル以外に出口が無くなる。
//
// ここでの一時停止と再開は、報告されたログにあるものと同じ。組み立てられない
// カメラで立ち上がったブリッジの上での話。
func TestBridgeResumesEvenWhenTheSourceCannotStart(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)

	tracker := status.New()
	b := New(config.Default(), "", hub.New(), tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected the unconfigured camera to fail to start")
	}
	defer b.Stop()

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := b.SetPaused(false); err != nil {
		t.Errorf("resume reported %v, want it to succeed and leave the driver retrying", err)
	}
	if b.Paused() {
		t.Fatal("the bridge is still paused after resuming")
	}

	snapshot := tracker.Snapshot()
	if snapshot.Paused {
		t.Error("the status still says paused")
	}
	if snapshot.LastError == "" {
		t.Error("the status has no reason, so nothing tells the user why there is no picture")
	}

	// そして出口は開いている。動くソースを選べる。
	working := mjpegUpstream(t, testJPEG(t, 32, 32))
	if _, err := b.Apply(ctx, mjpegConfig(working.URL)); err != nil {
		t.Fatalf("could not switch to a working source after resuming: %v", err)
	}
}

// 一時停止中の変更の拒否は、動いているソースが未検証のものと引き換えにされるのを
// 守っている。動いているソースが無ければ何も守らず、残された唯一の手を奪うだけ。
func TestBridgeAcceptsASourceChangeWhilePausedWithNothingRunning(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)

	frames := hub.New()
	b := New(config.Default(), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected the unconfigured camera to fail to start")
	}
	defer b.Stop()

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	working := mjpegUpstream(t, testJPEG(t, 32, 32))
	if _, err := b.Apply(ctx, mjpegConfig(working.URL)); err != nil {
		t.Fatalf("changing source while paused with nothing running: %v", err)
	}
	if got := b.Snapshot().Source.Type; got != config.SourceMJPEG {
		t.Errorf("source = %q, want the change to have been kept", got)
	}

	// 依然として一時停止中なので、変更は起動されず保持される。
	if !b.Paused() {
		t.Error("the change unpaused the bridge")
	}
	if err := b.SetPaused(false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitForFrame(t, frames, 5*time.Second)
}

// 保護そのものは生き延びなければならない。ブリッジが一時停止しているというだけで、
// 動いているソースが未検証のものと引き換えにされてはいけない。
func TestBridgeStillRefusesASourceChangeWhilePausedWithAWorkingSource(t *testing.T) {
	frames := hub.New()
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))
	b := New(mjpegConfig(upstream.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitForFrame(t, frames, 5*time.Second)

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	other := mjpegConfig(mjpegUpstream(t, testJPEG(t, 32, 32)).URL)
	_, err := b.Apply(ctx, other)
	if err == nil {
		t.Fatal("a working source was traded for an unproven one while paused")
	}
	if !strings.Contains(err.Error(), "resume first") {
		t.Errorf("error = %v, want it to say what to do", err)
	}
}

// 起動と再開はフレームを待たずに立ち上げるので、記録できるのは「ドライバが動き
// 始めた」ことだけ。その直後に自力で止まるドライバ — 404、ディスクから消えた
// ffmpeg — は、proven に見えて実はそうでない設定を残す。すると、動いているソースが
// 一時停止中に引き換えにされるのを守るはずの拒否が、何も守らないまま唯一の出口を
// 塞ぐことになる。
func TestBridgeAcceptsAChangeWhilePausedAfterTheSourceDiedOnItsOwn(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)

	// 404 は MJPEG ドライバにとって致命的。再接続せず停止する。
	dead := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer dead.Close()

	frames := hub.New()
	b := New(mjpegConfig(dead.URL), "", frames, status.New(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 起動時は検証しないので、これは成功する。ドライバは立ち上がった。
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	waitFor(t, 5*time.Second, "the source to stop on its own", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return !b.provenLocked()
	})

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	working := mjpegUpstream(t, testJPEG(t, 32, 32))
	if _, err := b.Apply(ctx, mjpegConfig(working.URL)); err != nil {
		t.Fatalf("changing source while paused after the old one died: %v", err)
	}
	if err := b.SetPaused(false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitForFrame(t, frames, 5*time.Second)
}

// 失敗したソースを畳むとステータスが消える。動いていなかった設定へ戻ることには、
// その理由だけでもやる価値がある。さもないと、失敗したカメラ変更が、具体的な訴えに
// 「ソース無し」で応えることになる。
func TestBridgeKeepsTheDiagnosisWhenTheRevertCannotStartEither(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)

	tracker := status.New()
	// デバイス名の無い uvc。そもそも組み立てられないドライバ。
	b := New(config.Default(), "", hub.New(), tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected the unconfigured camera to fail to start")
	}
	defer b.Stop()

	before := tracker.Snapshot()
	if before.LastError == "" {
		t.Fatal("the starting point has no diagnosis to keep")
	}

	if _, err := b.Apply(ctx, mjpegConfig(silentUpstream(t).URL)); err == nil {
		t.Fatal("expected the silent upstream to fail verification")
	}

	after := tracker.Snapshot()
	if after.Source != config.SourceUVC {
		t.Errorf("source = %q, want the settings that are back in effect", after.Source)
	}
	if after.LastError != before.LastError {
		t.Errorf("reason = %q, want the original %q", after.LastError, before.LastError)
	}
}

// 一時停止中に変更されたソースは、ステータスが名乗るべきソース。放っておくと
// 置き換えられた側を、そのソースが抱えていたエラーごと名乗り続けるので、一時停止中に
// 交換したカメラが「古いカメラがまだ失敗している」ように見える。
func TestBridgeNamesTheNewSourceWhenItIsChangedWhilePaused(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)

	tracker := status.New()
	b := New(config.Default(), "", hub.New(), tracker, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected the unconfigured camera to fail to start")
	}
	defer b.Stop()

	if err := b.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := b.Apply(ctx, mjpegConfig(mjpegUpstream(t, testJPEG(t, 32, 32)).URL)); err != nil {
		t.Fatalf("changing source while paused: %v", err)
	}

	snapshot := tracker.Snapshot()
	if snapshot.Source != config.SourceMJPEG {
		t.Errorf("source = %q, want the source that was just chosen", snapshot.Source)
	}
	if snapshot.LastError != "" {
		t.Errorf("reason = %q, want the replaced source's error gone", snapshot.LastError)
	}
}
