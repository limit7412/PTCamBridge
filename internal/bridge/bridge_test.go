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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/server"
	"github.com/limit7412/PTCamBridge/internal/source"
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

	if _, _, err := b.Apply(ctx, broken, nil); err == nil {
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

	if _, _, err := b.Apply(ctx, broken, nil); err == nil {
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

	_, _, err := b.Apply(ctx, updated, nil)
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

	moved := b.Snapshot()
	moved.Server.Listen = "127.0.0.1:19999"

	_, deferred, err := b.Apply(ctx, moved, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 黙って受け入れてはいけない。すでに bind してあるソケットは動かないので、
	// 呼び出し側が「効くのは次の起動から」と言えなければ、設定を変えたのに
	// アドレスが変わらない理由が誰にも分からなくなる。
	if !slices.Contains(deferred, "server.listen") {
		t.Errorf("deferred = %v, want it to name server.listen", deferred)
	}
	// 設定としては受け入れる。据え置くと、動作中の値とファイルの中身が食い違った
	// まま、その差を覗く手段がどこにも無くなる。
	if got := b.Snapshot().Server.Listen; got != moved.Server.Listen {
		t.Errorf("listen = %q, want the requested %q", got, moved.Server.Listen)
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
	if _, _, err := b.Apply(ctx, updated, nil); err != nil {
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
	if _, _, err := b.Apply(ctx, updated, nil); err != nil {
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
	if _, _, err := b.Apply(context.Background(), invalid, nil); err == nil {
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

	if _, _, err := b.Apply(ctx, broken, nil); err == nil {
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
	go func() { _, _, err := b.Apply(req, broken, nil); done <- err }()

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
	go func() { _, _, err := b.Apply(req, next, nil); done <- err }()

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

	if _, _, err := b.Apply(dead, next, nil); !errors.Is(err, context.Canceled) {
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

	_, _, err := b.Apply(ctx, broken, nil)
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

	if _, _, err := b.Apply(ctx, unbuildable, nil); err == nil {
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
	if _, _, err := b.Apply(ctx, updated, nil); err != nil {
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

	if _, _, err := b.Apply(ctx, broken, nil); err == nil {
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
	if _, _, err := b.Apply(ctx, unverified, nil); err == nil {
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
	b.SetPersistBase(fileCfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// まったく別のものを変更する。
	updated := b.Snapshot()
	updated.Server.HoldOnSourceLoss = true
	if _, _, err := b.Apply(ctx, updated, nil); err != nil {
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
	b.SetPersistBase(fileCfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Source.UVC.Device = "picked in the tray"
	if _, _, err := b.Apply(ctx, updated, nil); err != nil {
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
	if _, _, err := b.Apply(ctx, lost, nil); !errors.Is(err, config.ErrNotSaved) {
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

	if _, _, err := b.Apply(ctx, b.Snapshot(), nil); err != nil {
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
	if _, _, err := b.Apply(ctx, next, nil); err != nil {
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
	b.SetPersistBase(fileCfg, nil)

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
	if _, _, err := b.Apply(ctx, updated, nil); err != nil {
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
	b.SetPersistBase(fileCfg, nil)

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
	if _, _, err := b.Apply(ctx, updated, nil); err != nil {
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
	b.SetPersistBase(fileCfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	updated := b.Snapshot()
	updated.Server.ExtraHeaders = map[string]string{"X-One": "1"}
	if _, _, err := b.Apply(ctx, updated, nil); err != nil {
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
	if _, _, err := b.Apply(ctx, next, nil); err == nil {
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
	if _, _, err := b.Apply(ctx, next, nil); err != nil {
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
	b.SetPersistBase(cfg, nil)

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
	_, _, err := b.Apply(ctx, next, nil)
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
	if _, _, err := b.Apply(ctx, b.Snapshot(), nil); err != nil {
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
	if _, _, err := b.Apply(ctx, b.Snapshot(), nil); err != nil {
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
	b.SetPersistBase(cfg, nil)

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
	if _, _, err := b.Apply(ctx, first, nil); !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("first Apply = %v, want config.ErrNotSaved", err)
	}

	second := b.Snapshot()
	second.Server.HoldOnSourceLoss = true
	if _, _, err := b.Apply(ctx, second, nil); !errors.Is(err, config.ErrNotSaved) {
		t.Fatalf("second Apply = %v, want config.ErrNotSaved", err)
	}

	// ユーザーが編集を終え、次の変更がすべてを書き出す。
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("repair the file: %v", err)
	}
	if _, _, err := b.Apply(ctx, b.Snapshot(), nil); err != nil {
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
	b.SetPersistBase(config.Default(), nil)
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
	b.SetPersistBase(config.Default(), nil)
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

	_, deferred, err := b.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(deferred, "ui.language") {
		t.Errorf("deferred = %v, want it to name ui.language", deferred)
	}
	// ファイルには入っていなければならない。それが「次の起動で反映される」という
	// ことであり、この変更の眼目そのもの。
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.UI.Language != "ja" {
		t.Errorf("saved language = %q, want %q", saved.UI.Language, "ja")
	}
}

// 気が変わったら元に戻せなければならない。動作中の値を据え置くと、取り消しの要求は
// 「今の値」と同じなので変更として現れず、ファイルに入ったままの値が書き直される
// だけになる。保存した設定を確かめる手段も無くなる。
func TestApplyCanTakeBackADeferredChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))
	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	before := b.Snapshot().UI.Language

	cfg := b.Snapshot()
	cfg.UI.Language = "ja"
	if _, _, err := b.Apply(context.Background(), cfg, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 保存した値は読み返せる。見えないものは取り消せない。
	if got := b.Snapshot().UI.Language; got != "ja" {
		t.Fatalf("language = %q, want the saved %q to be visible", got, "ja")
	}

	back := b.Snapshot()
	back.UI.Language = before
	if _, _, err := b.Apply(context.Background(), back, nil); err != nil {
		t.Fatalf("Apply (taking it back): %v", err)
	}
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.UI.Language != before {
		t.Errorf("saved language = %q, want it back at %q", saved.UI.Language, before)
	}
}

// 壊れたソースを抱えているユーザーこそ、GUI から設定を直したい。起動時にしか
// 読まれない設定の変更が、カメラの状態に巻き込まれて保存できないのでは、この
// 機能そのものが要るときに使えない。
func TestApplySavesStartupOnlyChangesWhileTheSourceIsBroken(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")

	// 404 は MJPEG ドライバにとって致命的。再接続せず停止する。
	dead := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer dead.Close()

	b := New(mjpegConfig(dead.URL), path, hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	waitFor(t, 5*time.Second, "the source to stop on its own", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return !b.provenLocked()
	})

	cfg := b.Snapshot()
	cfg.UI.Language = "ja"
	cfg.Log.Level = "debug"
	_, deferred, err := b.Apply(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Apply while the source is broken: %v", err)
	}
	if len(deferred) == 0 {
		t.Error("deferred is empty, want the settings that wait for a restart")
	}
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.UI.Language != "ja" || saved.Log.Level != "debug" {
		t.Errorf("saved = %+v, want both startup-only changes on disk", saved)
	}
}

// とはいえ、何も動かさない適用は素通りさせてはいけない。同じソースを選び直すのは、
// 死んだドライバをもう一度起こす手段そのもの。
func TestApplyStillRestartsWhenNothingStartupOnlyMoved(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)

	dead := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer dead.Close()

	b := New(mjpegConfig(dead.URL), "", hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	waitFor(t, 5*time.Second, "the source to stop on its own", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return !b.provenLocked()
	})

	// 同じ設定をそのまま適用する。ドライバを起こし直そうとして、上流がまだ 404 な
	// ので失敗するのが正しい。黙って成功を返せば、/healthz が 503 のままなのに
	// 直ったことになる。
	if _, _, err := b.Apply(ctx, b.Snapshot(), nil); err == nil {
		t.Error("re-applying the same settings reported success without reviving the source")
	}
}

// 保留の判定は、直前に受け入れた設定ではなく、このプロセスが実際に使っているものと
// 比べなければならない。直前と比べると答えが 2 通りに裏返る。
func TestDeferredNamesFollowWhatIsRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))
	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	running := b.Snapshot().UI.Language

	cfg := b.Snapshot()
	cfg.UI.Language = "ja"
	_, deferred, err := b.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(deferred, "ui.language") {
		t.Fatalf("deferred = %v, want it to name ui.language", deferred)
	}

	// 無関係な項目だけを保存する。言語はまだ効いていないので、名前は出続けなければ
	// ならない。直前の設定と比べていると、ここで黙る。
	next := b.Snapshot()
	next.Server.HoldOnSourceLoss = !next.Server.HoldOnSourceLoss
	_, deferred, err = b.Apply(context.Background(), next, nil)
	if err != nil {
		t.Fatalf("Apply (unrelated change): %v", err)
	}
	if !slices.Contains(deferred, "ui.language") {
		t.Errorf("deferred = %v, want ui.language to still be waiting for a restart", deferred)
	}

	// 元に戻す。動作中の値と同じになったので、もう待っているものは無い。直前の
	// 設定と比べていると、ここで逆に名前が出る。
	back := b.Snapshot()
	back.UI.Language = running
	_, deferred, err = b.Apply(context.Background(), back, nil)
	if err != nil {
		t.Fatalf("Apply (taking it back): %v", err)
	}
	if slices.Contains(deferred, "ui.language") {
		t.Errorf("deferred = %v, want nothing waiting once the value matches what is running", deferred)
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

	_, deferred, err := b.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(deferred) != 1 || deferred[0] != "ui.language" {
		t.Errorf("deferred = %v, want only ui.language", deferred)
	}
	// 保留になった設定が、隣の設定の適用を止めてはいけない。
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
// silentUpstreamConnected は silentUpstream に「繋がった」合図を足したもの。
//
// 検証の最中にキャンセルするテストには、固定の sleep ではなくこれが要る。applyLocked は
// 冒頭で ctx を見て、既に終わっていれば検証へ進まずに返す。負荷の高い CI で goroutine が
// sleep の間に検証まで辿り着けないと、実装が正しくてもテストが落ちる。
//
// 上流に接続が来たことは、ドライバが起動して applyLocked が verifyStartLocked の中で
// 待っていることを意味する。ドライバが起動する経路は、その冒頭の判定を通った先にしか
// 無いため。
func silentUpstreamConnected(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	connected := make(chan struct{})
	var once sync.Once
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 再接続で複数回来るので once。閉じるのは最初の 1 回だけ。
		once.Do(func() { close(connected) })
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(done); ts.Close() })
	return ts, connected
}

// waitForVerify は、新しいソースが上流に繋がるまで待つ。ここから先でキャンセルすれば、
// 検証の最中に割り込んだことになる。
func waitForVerify(t *testing.T, connected <-chan struct{}) {
	t.Helper()
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the new source never reached the upstream, so nothing was interrupted mid-verification")
	}
}

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
	_, _, err := b.Apply(ctx, silent, nil)
	if err == nil {
		t.Fatal("expected the silent upstream to fail verification")
	}
	if strings.Contains(err.Error(), "could not be restored") {
		t.Errorf("error = %v, want only the failure the caller asked about", err)
	}

	// すべての目的はここ。別のソースを選べること。
	working := mjpegUpstream(t, testJPEG(t, 32, 32))
	if _, _, err := b.Apply(ctx, mjpegConfig(working.URL), nil); err != nil {
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
	if _, _, err := b.Apply(ctx, mjpegConfig(working.URL), nil); err != nil {
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
	if _, _, err := b.Apply(ctx, mjpegConfig(working.URL), nil); err != nil {
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
	_, _, err := b.Apply(ctx, other, nil)
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
	if _, _, err := b.Apply(ctx, mjpegConfig(working.URL), nil); err != nil {
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

	if _, _, err := b.Apply(ctx, mjpegConfig(silentUpstream(t).URL), nil); err == nil {
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
	if _, _, err := b.Apply(ctx, mjpegConfig(mjpegUpstream(t, testJPEG(t, 32, 32)).URL), nil); err != nil {
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

// 応答の設定と保留の名前は同じ瞬間のものでなければならない。呼び出し側が後から
// Snapshot を読むと、その隙間に入った別の要求の設定と、こちらの要求について数えた
// 名前が並ぶ。
func TestApplyReturnsTheSettlingConfigWithTheNames(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	cfg := b.Snapshot()
	cfg.UI.Language = "ja"

	applied, deferred, err := b.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.UI.Language != "ja" {
		t.Errorf("applied language = %q, want %q", applied.UI.Language, "ja")
	}
	if !slices.Contains(deferred, "ui.language") {
		t.Errorf("deferred = %v, want it to name ui.language", deferred)
	}
}

// 書けなかった保存は、その後の適用が失敗しても捨てない。書けなかったのは前の変更で、
// 今の要求の成否とは関係が無い。ファイルが書けるようになってもカメラが直るまで
// 書けないままでは、起動時専用の設定を保存できるようにしたことの意味が半分になる。
func TestApplyFlushesAnEarlierUnsavedChangeWhenTheSourceIsBroken(t *testing.T) {
	shortenVerify(t, 200*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")

	dead := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer dead.Close()

	b := New(mjpegConfig(dead.URL), path, hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	waitFor(t, 5*time.Second, "the source to stop on its own", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return !b.provenLocked()
	})

	// 書けない場所を指させて、起動時専用の変更を保留に落とす。ディレクトリの上には
	// ファイルを rename できない。
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	b.mu.Lock()
	b.cfgPath = blocked
	b.mu.Unlock()

	cfg := b.Snapshot()
	cfg.UI.Language = "ja"
	if _, _, err := b.Apply(ctx, cfg, nil); err == nil {
		t.Fatal("saving to an unwritable path reported success")
	}
	b.mu.Lock()
	pending := b.unsaved != nil
	b.cfgPath = path
	b.mu.Unlock()
	if !pending {
		t.Fatal("the change that could not be saved was not held")
	}

	// 書ける場所に戻して同じ設定を送り直す。カメラはまだ壊れているので、この要求
	// そのものは失敗する。それでも保留は書かれなければならない。
	if _, _, err := b.Apply(ctx, b.Snapshot(), nil); err == nil {
		t.Error("re-applying reported success while the source was still broken")
	}
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.UI.Language != "ja" {
		t.Errorf("saved language = %q, want the held change %q to reach the file", saved.UI.Language, "ja")
	}
}

// 検証を通っていない設定を外へ見せてはいけない。最大 30 秒のあいだ Snapshot が
// 「これから取り消されるかもしれない設定」を返すと、それを読んだクライアントは
// その値を土台に次の変更を組み立て、巻き戻ったはずのソースを自分で復活させる。
func TestSnapshotDoesNotShowAnUnverifiedSource(t *testing.T) {
	shortenVerify(t, 500*time.Millisecond)
	working := mjpegUpstream(t, testJPEG(t, 32, 32))
	silent := silentUpstream(t)

	b := New(mjpegConfig(working.URL), "", hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitFor(t, 5*time.Second, "the first source to prove itself", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.provenLocked()
	})

	// 応答するだけでフレームを出さない上流へ切り替える。検証はタイムアウトし、
	// 設定は巻き戻る。
	next := mjpegConfig(silent.URL)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := b.Apply(ctx, next, nil); err == nil {
			t.Error("a source that never sent a frame was accepted")
		}
	}()

	// 検証の最中に読む。要求された URL が見えてはいけない。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := b.Snapshot().Source.MJPEG.URL; got == silent.URL {
			t.Fatalf("Snapshot showed %q while it was still being verified", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-done

	if got := b.Snapshot().Source.MJPEG.URL; got != working.URL {
		t.Errorf("url = %q, want it back at %q", got, working.URL)
	}
}

// 保留の名前は、保存した内容から数える。動作中の設定から数えると、ユーザーが
// 設定ファイルを直接編集した分を見落とす。
func TestDeferredNamesFollowWhatWasSaved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))
	b := New(mjpegConfig(upstream.URL), path, hub.New(), status.New(), discardLogger())
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// まず 1 度保存して、ファイルを作る。
	if _, _, err := b.Apply(context.Background(), b.Snapshot(), nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// ユーザーが設定ファイルを直接編集した、という状況。
	edited, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	edited.UI.Language = "ja"
	if err := config.Save(path, edited); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// API からは無関係な項目だけを変える。
	cfg := b.Snapshot()
	cfg.Server.HoldOnSourceLoss = !cfg.Server.HoldOnSourceLoss
	_, deferred, err := b.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// ファイルには ja が残っているので、次の起動で言語は変わる。応答はそれを
	// 言わなければならない。
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if saved.UI.Language != "ja" {
		t.Fatalf("saved language = %q, want the hand edit %q to survive", saved.UI.Language, "ja")
	}
	if !slices.Contains(deferred, "ui.language") {
		t.Errorf("deferred = %v, want it to name ui.language", deferred)
	}
}

// 起動時に環境変数やコマンドラインで上書きされた設定は、次の起動を待っている
// のではない。ファイルに何を書いても同じ上書きが勝つので、保留として数えると
// 画面は起きない変更を毎回知らせることになる。
func TestOverriddenSettingsAreNotPending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))

	// ファイルは en、起動時の実効設定は ja。PTCAMBRIDGE_LANGUAGE=ja で起動した
	// ときと同じ形。
	file := mjpegConfig(upstream.URL)
	file.UI.Language = "en"
	effective := file
	effective.UI.Language = "ja"

	b := New(effective, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(file, []string{"ui.language"})
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	if got := b.Overridden(); !slices.Contains(got, "ui.language") {
		t.Errorf("Overridden = %v, want it to name ui.language", got)
	}

	// 無関係な項目だけを保存する。ファイルは en のままなので起動時の ja とは
	// 食い違うが、次の起動でも環境変数が勝つので保留ではない。
	cfg := b.Snapshot()
	cfg.Server.HoldOnSourceLoss = !cfg.Server.HoldOnSourceLoss
	_, deferred, err := b.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if slices.Contains(deferred, "ui.language") {
		t.Errorf("deferred = %v, want ui.language left out; the override wins next time too", deferred)
	}
}

// 上書きは値の差ではなく、指定されたという事実です。ファイルと同じ値を指定した
// 上書き — PTCAMBRIDGE_LANGUAGE=en をファイルの en に重ねる — は差を作りませんが、
// 次の起動でもやはり環境変数が勝つので、ja を保存しても保留にはなりません。
func TestOverriddenSettingsAreNotPendingEvenWithTheSameValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))

	file := mjpegConfig(upstream.URL)
	file.UI.Language = "en"

	// ファイルも実効設定も en。差はどこにも無い。
	b := New(file, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(file, []string{"ui.language"})
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	if got := b.Overridden(); !slices.Contains(got, "ui.language") {
		t.Errorf("Overridden = %v, want it to name ui.language even though the values match", got)
	}

	cfg := b.Snapshot()
	cfg.UI.Language = "ja"
	_, deferred, err := b.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if slices.Contains(deferred, "ui.language") {
		t.Errorf("deferred = %v, want ui.language left out; the override wins next time too", deferred)
	}
}

// 上書きの差し引きは葉ごとです。papertracker.install_dir を上書きしている機械でも、
// write_cache の保留はそのまま挙がらなければなりません。次の起動では実際に変わる
// からです。
func TestOverriddenLeavesDoNotHideTheirNeighbours(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ptcambridge.toml")
	upstream := mjpegUpstream(t, testJPEG(t, 32, 32))

	file := mjpegConfig(upstream.URL)
	file.PaperTracker.InstallDir = "A"
	file.PaperTracker.WriteCache = false
	effective := file
	effective.PaperTracker.InstallDir = "B" // 環境変数で上書きした側

	b := New(effective, path, hub.New(), status.New(), discardLogger())
	b.SetPersistBase(file, []string{"papertracker.install_dir"})
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	cfg := b.Snapshot()
	cfg.PaperTracker.WriteCache = true
	_, deferred, err := b.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(deferred, "papertracker.write_cache") {
		t.Errorf("deferred = %v, want papertracker.write_cache; only install_dir is overridden", deferred)
	}
	if slices.Contains(deferred, "papertracker.install_dir") {
		t.Errorf("deferred = %v, want papertracker.install_dir left out", deferred)
	}
}

// Overridden は画面が毎秒読むものなので、ソースの検証を待ってはいけません。
// Apply は最初のフレームを待つあいだ最長 30 秒 mu を握るので、その下に置くと
// 診断画面も設定画面もその間ずっと止まります。
func TestOverriddenDoesNotWaitForTheLock(t *testing.T) {
	b := New(config.Default(), "", hub.New(), status.New(), discardLogger())
	b.SetPersistBase(config.Default(), []string{"ui.language"})

	b.mu.Lock()
	defer b.mu.Unlock()

	got := make(chan []string, 1)
	go func() { got <- b.Overridden() }()

	select {
	case names := <-got:
		if !slices.Contains(names, "ui.language") {
			t.Errorf("Overridden = %v, want it to name ui.language", names)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Overridden blocked while something else held the lock")
	}
}

// restartOnlyLeaves と restartDeferred は同じ集合を指していなければなりません。
// 片方だけに名前が増えると、上書きの差し引きが静かに効かなくなります。
func TestRestartOnlyLeavesMatchWhatCanBeDeferred(t *testing.T) {
	base := config.Default()

	moved := map[string]config.Config{}
	for name, change := range map[string]func(*config.Config){
		"server.listen":            func(c *config.Config) { c.Server.Listen = "127.0.0.1:1" },
		"log.level":                func(c *config.Config) { c.Log.Level = "debug" },
		"log.dir":                  func(c *config.Config) { c.Log.Dir = "elsewhere" },
		"output.serial.enabled":    func(c *config.Config) { c.Output.Serial.Enabled = !c.Output.Serial.Enabled },
		"output.serial.port":       func(c *config.Config) { c.Output.Serial.Port = "COM7" },
		"output.serial.baud":       func(c *config.Config) { c.Output.Serial.Baud = 115200 },
		"output.serial.header":     func(c *config.Config) { c.Output.Serial.Header = []int{0xFF, 0xA0} },
		"papertracker.install_dir": func(c *config.Config) { c.PaperTracker.InstallDir = "elsewhere" },
		"papertracker.write_cache": func(c *config.Config) { c.PaperTracker.WriteCache = !c.PaperTracker.WriteCache },
		"ui.language":              func(c *config.Config) { c.UI.Language = "ja" },
	} {
		next := base
		change(&next)
		moved[name] = next
	}

	// restartOnlyLeaves の名前はすべて、実際に動かすと restartDeferred が挙げる。
	for _, name := range restartOnlyLeaves {
		next, ok := moved[name]
		if !ok {
			t.Fatalf("restartOnlyLeaves names %q but this test does not know how to move it", name)
		}
		if got := restartDeferred(base, next); !slices.Contains(got, name) {
			t.Errorf("restartDeferred after moving %s = %v, want it to name %s", name, got, name)
		}
	}

	// 逆向き。restartDeferred が挙げる名前はすべて restartOnlyLeaves にある。
	for name, next := range moved {
		for _, got := range restartDeferred(base, next) {
			if !slices.Contains(restartOnlyLeaves, got) {
				t.Errorf("restartDeferred after moving %s named %q, which restartOnlyLeaves does not have", name, got)
			}
		}
	}
}

// 要求が途中で終わったのは、ソースの失敗ではない。アプリの終了や、去った呼び出し側に
// ついて分かることであって、カメラについては何も分かっていない。ERROR で
// 「ソースを起動できなかった」と書くと、終了のたびに記録が残り、後から読む人は
// あるはずのない不具合を探すことになる。
func TestACancelledRequestIsNotReportedAsASourceFailure(t *testing.T) {
	shortenVerify(t, 5*time.Second)
	working := mjpegUpstream(t, testJPEG(t, 32, 32))
	silent, connected := silentUpstreamConnected(t)

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	b := New(mjpegConfig(working.URL), "", hub.New(), status.New(), log)
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitFor(t, 5*time.Second, "the first source to prove itself", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.provenLocked()
	})

	// 応答するだけでフレームを出さない上流へ切り替え、検証の最中に要求を打ち切る。
	// 終了がキューに積まれた切替を捕まえたときと同じ形。
	reqCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := b.Apply(reqCtx, mjpegConfig(silent.URL), nil)
		done <- err
	}()
	waitForVerify(t, connected)
	cancel()

	if err := <-done; err == nil {
		t.Fatal("Apply reported success for a request that was cut short")
	}

	out := logged.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("a cancelled request was reported as a source failure:\n%s", out)
	}
	if !strings.Contains(out, "the request ended before the new settings could be verified") {
		t.Errorf("the rollback left no record of why:\n%s", out)
	}

	// 巻き戻しは今までどおり行われる。呼び方を変えただけで、動きは変えていない。
	if got := b.Snapshot().Source.MJPEG.URL; got != working.URL {
		t.Errorf("url = %q, want it back at %q", got, working.URL)
	}
}

// 終了の経路は 2 つある。トレイからの Switch は Start と同じコンテキストを渡すので、
// 終了時には verifyCtx.Done() と b.root.Done() が同時に準備完了になり、select は
// どちらを選んでもおかしくない。片方だけが原因を運んでいると、正常な終了が半分の
// 確率で ERROR として記録される。
func TestShuttingDownTheBridgeIsNotASourceFailure(t *testing.T) {
	shortenVerify(t, 5*time.Second)
	working := mjpegUpstream(t, testJPEG(t, 32, 32))
	silent, connected := silentUpstreamConnected(t)

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	b := New(mjpegConfig(working.URL), "", hub.New(), status.New(), log)
	root, shutdown := context.WithCancel(context.Background())
	if err := b.Start(root); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	waitFor(t, 5*time.Second, "the first source to prove itself", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.provenLocked()
	})

	// 要求そのものは生きたまま、ブリッジの方を止める。b.root.Done() の case を
	// 確実に通す形。
	done := make(chan error, 1)
	go func() {
		_, _, err := b.Apply(context.Background(), mjpegConfig(silent.URL), nil)
		done <- err
	}()
	waitForVerify(t, connected)
	shutdown()

	err := <-done
	if err == nil {
		t.Fatal("Apply reported success while the bridge was shutting down")
	}
	// 呼び出し側 — トレイの reportCommandFailure — が中断だと見分けられなければ、
	// この経路を通った終了はやはり ERROR として記録される。
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to carry the cancellation so callers can tell it apart", err)
	}
	if strings.Contains(logged.String(), "level=ERROR") {
		t.Errorf("shutting down was reported as a source failure:\n%s", logged.String())
	}
}

// verifyOutcome は純粋関数なので、フレームと終了が同時に起きる — 実際に起こすのが
// 難しい — 場合をここで直接押さえられる。
func TestVerifyOutcomeCarriesTheCancellation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		frameErr   error
		requestErr error
		shutdown   error
	}{
		{name: "the request was cancelled", requestErr: context.Canceled},
		{name: "the request ran out of time", requestErr: context.DeadlineExceeded},
		{name: "the bridge is shutting down", shutdown: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyOutcome("mjpeg", tc.frameErr, tc.requestErr, tc.shutdown)
			if err == nil {
				t.Fatal("verifyOutcome = nil, want the frame rejected")
			}
			if !requestEnded(err) && !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want it to carry why nobody is waiting any more", err)
			}
		})
	}

	// 本当の失敗は中断と混ざってはいけない。
	err := verifyOutcome("mjpeg", errors.New("not a JPEG"), nil, nil)
	if errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want a real failure kept apart from a cancellation", err)
	}
}

// コンテキストの終わり方は 2 つあり、どちらもカメラについては何も語らない。
// キャンセルだけを見ていると、期限を付けたクライアント — HTTP のタイムアウトは
// その形 — がソースの失敗として記録される。
func TestRequestEndedCoversBothWaysAContextEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "cancelled", err: context.Canceled, want: true},
		{name: "out of time", err: context.DeadlineExceeded, want: true},
		{
			name: "wrapped the way verifyStartLocked wraps it",
			err:  fmt.Errorf("bridge: mjpeg was still starting when the request ended: %w", context.DeadlineExceeded),
			want: true,
		},
		// 本当の失敗を巻き込んではいけない。これを降格すると、カメラが壊れていても
		// ログには何も残らない。
		{name: "a real failure", err: errors.New("uvc: no camera configured"), want: false},
		{name: "no failure at all", err: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestEnded(tc.err); got != tc.want {
				t.Errorf("requestEnded(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// uvcConfig は、名前だけ与えた UVC の設定。ドライバは起動しない。
func uvcConfig(device string) config.Config {
	cfg := config.Default()
	cfg.Source.Type = config.SourceUVC
	cfg.Source.UVC.Device = device
	cfg.Normalise()
	return cfg
}

// fakeModeLister は listModes を差し替え、呼ばれた回数と、そのとき渡された名前を
// 記録する。
type fakeModeLister struct {
	mu      sync.Mutex
	calls   int
	devices []string
	modes   []source.Mode
	err     error
}

// onWindowsCameraRules は、カメラが排他的なプラットフォームの規則で走らせる。
// テストは Windows で走らないので、これが無いと掴んでいるカメラの筋を踏めない。
func onWindowsCameraRules(t *testing.T) {
	t.Helper()
	prev := exclusiveCameraAccess
	exclusiveCameraAccess = true
	t.Cleanup(func() { exclusiveCameraAccess = prev })
}

func (f *fakeModeLister) install(t *testing.T) {
	t.Helper()
	onWindowsCameraRules(t)
	prev := listModes
	listModes = func(_ context.Context, _, device string) ([]source.Mode, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		f.devices = append(f.devices, device)
		return f.modes, f.err
	}
	t.Cleanup(func() { listModes = prev })
}

func (f *fakeModeLister) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// 列挙はカメラを開く。UVC は排他的なので、キャプチャ中のカメラを訊かれた列挙は
// 失敗する。設定画面が最もよく訊くのがその「今のカメラ」なので、一度得た答えを
// 憶えておいて、そこから答えられなければならない。
func TestCameraModesAnswersTheRunningCameraFromWhatItLearnedEarlier(t *testing.T) {
	want := []source.Mode{{Format: "mjpeg", MinSize: "640x480", MaxSize: "640x480", MinFPS: 30, MaxFPS: 30}}
	lister := &fakeModeLister{modes: want}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())

	// まだ掴んでいないうちに一度訊く。ここは本当に列挙できる。
	b.SetPaused(true)
	got, err := b.CameraModes(context.Background(), "Bigeye")
	if err != nil {
		t.Fatalf("CameraModes while paused: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("modes = %v, want %v", got, want)
	}

	// 掴んだ後は列挙が失敗する。憶えたもので答えること。
	lister.modes, lister.err = nil, errors.New("uvc: ffmpeg listed no modes")
	b.SetPaused(false)
	got, err = b.CameraModes(context.Background(), "Bigeye")
	if err != nil {
		t.Fatalf("CameraModes while capturing: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("modes = %v, want the remembered %v", got, want)
	}
}

// 掴んでいると分かっているカメラに、開けないと分かっている列挙をぶつけない。
// 列挙は自前で 15 秒待つので、名前を打つたびにそれを払うことになる。
func TestCameraModesDoesNotReopenTheCameraItIsHolding(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Fatalf("CameraModes while paused: %v", err)
	}
	b.SetPaused(false)

	before := lister.count()
	for range 3 {
		if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
			t.Fatalf("CameraModes while capturing: %v", err)
		}
	}
	if got := lister.count() - before; got != 0 {
		t.Errorf("ffmpeg ran %d times for a camera the bridge is holding, want 0", got)
	}
}

// 掴んでいないはずのカメラの列挙が失敗したときも、憶えがあればそれで答える。
//
// 名前の比較は完全ではない。設定がフレンドリ名を持ち、画面が "@device_pnp_..."
// で訊けば (あるいはその逆なら)、同じ 1 台でも別物に見える。そこで列挙は「自分が
// 握っているせいで」失敗するが、こちらはそうと気付けない。憶えがあるなら、
// 気付けなくても正しい答えは出せる。
func TestCameraModesFallsBackToWhatItLearnedWhenTheLookupFails(t *testing.T) {
	want := []source.Mode{{Format: "mjpeg", MinSize: "640x480", MaxSize: "640x480", MinFPS: 30, MaxFPS: 30}}
	lister := &fakeModeLister{modes: want}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	if _, err := b.CameraModes(context.Background(), "@device_pnp_bigeye"); err != nil {
		t.Fatalf("CameraModes: %v", err)
	}

	lister.modes, lister.err = nil, errors.New("uvc: ffmpeg listed no modes")
	got, err := b.CameraModes(context.Background(), "@device_pnp_bigeye")
	if err != nil {
		t.Fatalf("CameraModes after the lookup broke: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("modes = %v, want the remembered %v", got, want)
	}
}

// 憶えが無いまま自分の握っているカメラを訊かれたら、開きに行かずにその場で言う。
//
// 開けないと分かっているものを開きに行っても、列挙が 15 秒を使い切ってから同じ
// 答えに辿り着くだけ。しかも失敗は憶えないので、画面がカメラ名に触れるたびに
// それを払うことになる。
func TestCameraModesSaysWhenItIsTheOneHoldingTheCamera(t *testing.T) {
	lister := &fakeModeLister{err: errors.New("uvc: ffmpeg listed no modes for \"Bigeye\"")}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	_, err := b.CameraModes(context.Background(), "Bigeye")
	if err == nil {
		t.Fatal("expected an error when the camera cannot be opened")
	}
	if !strings.Contains(err.Error(), "pause capture") {
		t.Errorf("error = %q, want it to say how to get the modes", err)
	}
	if got := lister.count(); got != 0 {
		t.Errorf("ffmpeg ran %d times for a camera the bridge is holding, want 0", got)
	}
}

// カメラの顔ぶれが変わったら、憶えは捨てなければならない。
//
// モードが変わらないのは同じ 1 台についてだけ。憶えの鍵は名前だが、名前は個体を
// 指さない — "USB Camera" は次に挿した別機種にも付く。抜き挿しで入れ替わった
// カメラに、前の機種のモードを勧め続けることになる。
func TestCameraModesForgetsWhatItLearnedWhenTheCamerasChange(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480", MinFPS: 30, MaxFPS: 30}}}
	lister.install(t)

	cameras := []source.Device{{Name: "USB Camera", Alternative: "@device_pnp_first"}}
	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) { return cameras, nil }
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("USB Camera"), "", hub.New(), status.New(), discardLogger())
	// 設定画面と同じ順序。まず一覧を読み、それからモードを訊く。
	b.Devices(context.Background())
	b.SetPaused(true)
	if _, err := b.CameraModes(context.Background(), "USB Camera"); err != nil {
		t.Fatalf("CameraModes while paused: %v", err)
	}
	b.SetPaused(false)

	// 同じ顔ぶれのままなら憶えは残る。一覧は 5 秒ごとに読まれるので、ここで
	// 捨てていては憶える意味が無い。
	b.Devices(context.Background())
	if _, err := b.CameraModes(context.Background(), "USB Camera"); err != nil {
		t.Fatalf("CameraModes with the same cameras attached: %v", err)
	}

	// 別の個体に入れ替わった。名前は同じでも、答えはもう前の機種のもの。
	cameras = []source.Device{{Name: "USB Camera", Alternative: "@device_pnp_second"}}
	b.Devices(context.Background())
	if _, err := b.CameraModes(context.Background(), "USB Camera"); err == nil {
		t.Error("expected the modes of the camera that was unplugged to be forgotten")
	}
}

// 関係のないカメラが増えても、素性の変わっていないカメラの憶えは残さなければ
// ならない。顔ぶれ全体で一致を見ると、1 台挿しただけで全部消える。そのとき今
// キャプチャしているカメラはもう調べ直せないので、正しかった答えを二度と出せない。
func TestCameraModesKeepsWhatItLearnedAboutTheCamerasThatDidNotChange(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	cameras := []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}
	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) { return cameras, nil }
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.Devices(context.Background())
	b.SetPaused(true)
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Fatalf("CameraModes while paused: %v", err)
	}
	b.SetPaused(false)

	// 別のカメラを挿した。Bigeye は何も変わっていない。
	cameras = append(cameras, source.Device{Name: "Webcam", Alternative: "@device_pnp_webcam"})
	b.Devices(context.Background())
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Errorf("plugging in another camera threw away what Bigeye said: %v", err)
	}
}

// 同じカメラを同時に訊かれても、開くのは 1 回でなければならない。
//
// 画面の側にも同じ抑止があるが、あちらが知っているのはそのページの中だけ。
// タブを 2 つ開けば、同じカメラへ ffmpeg が 2 本向かう。開くのはこちらなので、
// 抑止もこちらに要る。
func TestCameraModesOpensACameraOnceForConcurrentCallers(t *testing.T) {
	onWindowsCameraRules(t)
	want := []source.Mode{{MinSize: "640x480", MaxSize: "640x480", MinFPS: 30, MaxFPS: 30}}

	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	prev := listModes
	listModes = func(context.Context, string, string) ([]source.Mode, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return want, nil
	}
	t.Cleanup(func() { listModes = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)

	const callers = 4
	got := make(chan []source.Mode, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			modes, err := b.CameraModes(context.Background(), "Bigeye")
			got <- modes
			errs <- err
		}()
	}

	// 1 本目が走り出し、残りがその答えを待つところまで進めてから解放する。
	// 先に解放すると 4 本が順番に走るだけで、重なりを一度も作らない。
	<-started
	deadline := time.Now().Add(5 * time.Second)
	for b.listingWaitersForTest() < callers-1 {
		if time.Now().After(deadline) {
			// 待っていないということは、それぞれが自分でカメラを開いたということ。
			close(release)
			t.Fatalf("only %d of %d callers waited for the running enumeration", b.listingWaitersForTest(), callers-1)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)

	for range callers {
		if err := <-errs; err != nil {
			t.Errorf("CameraModes: %v", err)
		}
		if modes := <-got; !slices.Equal(modes, want) {
			t.Errorf("modes = %v, want %v", modes, want)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("opened the camera %d times for %d concurrent callers, want 1", n, callers)
	}
}

// 共有している列挙は、始めた要求と一緒に終わってはいけない。
//
// 始めたタブが閉じただけで止めると、同じ答えを待っている他の要求まで巻き添えに
// なる。待っている側の要求はまだ生きているのに、候補が出せない。
func TestCameraModesFinishesTheLookupTheFirstCallerAbandoned(t *testing.T) {
	onWindowsCameraRules(t)
	want := []source.Mode{{MinSize: "640x480", MaxSize: "640x480", MinFPS: 30, MaxFPS: 30}}

	started := make(chan struct{})
	release := make(chan struct{})
	prev := listModes
	listModes = func(ctx context.Context, _, _ string) ([]source.Mode, error) {
		close(started)
		// 本物の列挙と同じく、期限が切れればそこで終わる。
		select {
		case <-release:
			return want, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	t.Cleanup(func() { listModes = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)

	// 1 人目。これが列挙を始め、そして先に立ち去る。
	leaving, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	gone := make(chan error, 1)
	go func() {
		_, err := b.CameraModes(leaving, "Bigeye")
		gone <- err
	}()
	<-started

	// 2 人目。1 人目の答えを待つ。
	stays := make(chan []source.Mode, 1)
	staysErr := make(chan error, 1)
	go func() {
		modes, err := b.CameraModes(context.Background(), "Bigeye")
		stays <- modes
		staysErr <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for b.listingWaitersForTest() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the second caller never joined the running lookup")
		}
		time.Sleep(time.Millisecond)
	}

	// 1 人目が去る。列挙は 2 人目のために続かなければならない。
	giveUp()
	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Errorf("the caller that left got %v, want its own cancellation", err)
	}
	close(release)

	if err := <-staysErr; err != nil {
		t.Fatalf("the caller that stayed got %v, want the modes", err)
	}
	if modes := <-stays; !slices.Equal(modes, want) {
		t.Errorf("modes = %v, want %v", modes, want)
	}
}

// 終了は、走っている列挙も終わらせなければならない。
//
// 待たずに落ちると、ffmpeg が残ってカメラを掴んだままになり得る。Windows の
// 子プロセスは親と一緒には死なない。カメラを解放しないまま終わることは、この
// アプリケーションが最もしてはいけないこと。
func TestStopEndsALookupThatIsStillRunning(t *testing.T) {
	onWindowsCameraRules(t)

	running := make(chan struct{})
	ended := make(chan error, 1)
	prev := listModes
	listModes = func(ctx context.Context, _, _ string) ([]source.Mode, error) {
		close(running)
		<-ctx.Done()
		ended <- ctx.Err()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { listModes = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	// 起動は失敗してよい (この機械に ffmpeg は無い)。要るのは生存期間だけ。
	_ = b.Start(context.Background())
	b.SetPaused(true)

	go b.CameraModes(context.Background(), "Bigeye") //nolint:errcheck // 答えは見ない
	<-running

	b.Stop()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the lookup ended with %v, want it to be cancelled", err)
		}
	default:
		t.Error("Stop returned while a lookup was still holding the camera")
	}
}

// キャプチャがカメラを開く前に、列挙にそれを手放させなければならない。
//
// 排他的なデバイスなので両方は開けない。ここで衝突すると、モードを見てから保存
// した人 — つまりこの機能を使った人 — の設定だけが巻き戻る。
func TestStartingCaptureTakesTheCameraFromAPendingLookup(t *testing.T) {
	onWindowsCameraRules(t)

	running := make(chan struct{})
	prev := listModes
	listModes = func(ctx context.Context, _, _ string) ([]source.Mode, error) {
		close(running)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { listModes = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	_ = b.Start(context.Background())
	t.Cleanup(b.Stop)
	b.SetPaused(true)

	asked := make(chan error, 1)
	go func() {
		_, err := b.CameraModes(context.Background(), "Bigeye")
		asked <- err
	}()
	<-running

	// 再開はキャプチャを立ち上げる。その前に列挙が手放していなければならない。
	if err := b.SetPaused(false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case err := <-asked:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the lookup ended with %v, want it to have been asked to let go", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("capture started while a lookup still held the camera")
	}
}

// 開こうとしているカメラも、掴んでいるものとして扱わなければならない。
//
// 検証を通っていない設定は公開しないので、起動している間 view はまだ前のカメラを
// 指している。それを「空いている」と読ませると、最長 30 秒のあいだに来た問い合わせが
// カメラを開きに行き、起動と取り合う。
func TestCameraModesLeavesTheCameraThatIsBeingOpenedAlone(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	// 設定は別のカメラを指したまま。一時停止中でもある — どちらも「空いている」
	// と読ませる材料になる。
	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)
	b.openingCameraForTest("Newcomer")

	if _, err := b.CameraModes(context.Background(), "Newcomer"); err == nil {
		t.Error("expected the camera being opened to be left alone")
	}
	if got := lister.count(); got != 0 {
		t.Errorf("ffmpeg ran %d times for a camera being opened, want 0", got)
	}

	// 他のカメラは今までどおり調べられる。
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Errorf("CameraModes for another camera: %v", err)
	}
}

// キャプチャを起動している間じゅう、そのカメラは予約されていなければならない。
// そして開き終えたら下ろさなければならない — 残ると、そのカメラは二度と
// 調べられなくなる。
func TestStartingCaptureHoldsTheCameraForTheWholeLaunch(t *testing.T) {
	onWindowsCameraRules(t)

	var probes atomic.Int64
	var started sync.Once
	running := make(chan struct{})
	release := make(chan struct{})
	prev := listModes
	listModes = func(ctx context.Context, _, _ string) ([]source.Mode, error) {
		probes.Add(1)
		started.Do(func() { close(running) })
		// 諦めろと言われてもすぐには手放さない。本物の ffmpeg も、殺されてから
		// カメラを離すまでには間があります。その間が、ここで見たい窓です。
		<-release
		return nil, ctx.Err()
	}
	t.Cleanup(func() { listModes = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	_ = b.Start(context.Background())
	t.Cleanup(b.Stop)
	b.SetPaused(true)

	// この列挙は、起動が手放させるまで走り続ける。起動はその間ここで止まるので、
	// 「開いている最中」を外から覗ける。
	go b.CameraModes(context.Background(), "Bigeye") //nolint:errcheck // 答えは見ない
	<-running

	resumed := make(chan error, 1)
	go func() { resumed <- b.SetPaused(false) }()

	deadline := time.Now().Add(5 * time.Second)
	for b.isOpeningForTest() == "" {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the bridge never marked the camera as being opened")
		}
		time.Sleep(time.Millisecond)
	}

	// この瞬間に来た問い合わせは、カメラを開きに行ってはいけない。設定の側は
	// まだ動いていないので、そこだけを見ると「空いている」と読める。
	before := probes.Load()
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err == nil {
		t.Error("a lookup was allowed while the camera was being opened")
	}
	if got := probes.Load() - before; got != 0 {
		t.Errorf("ffmpeg ran %d times while the camera was being opened, want 0", got)
	}

	close(release)
	if err := <-resumed; err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := b.isOpeningForTest(); got != "" {
		t.Errorf("the bridge is still holding %q open after the launch returned", got)
	}
}

// 止め終えた後に、新しい列挙を始めてはいけない。
//
// Stop が待つのは、待ち始めた時点で走っていたものだけ。その後に登録されたものは
// 拾えないので、始めさせない。拾えないまま終わると、ffmpeg が残ってカメラを
// 掴んだままになり得る。
func TestCameraModesRefusesToStartAfterTheBridgeStopped(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	_ = b.Start(context.Background())
	b.SetPaused(true)
	b.Stop()

	if _, err := b.CameraModes(context.Background(), "Bigeye"); err == nil {
		t.Error("expected a stopped bridge to refuse a new lookup")
	}
	if got := lister.count(); got != 0 {
		t.Errorf("ffmpeg ran %d times after the bridge stopped, want 0", got)
	}
}

// 予約の確認と登録は、同じロックの下で行わなければならない。
//
// 呼び出し側の判定はロックの外なので、その後・登録の前に、キャプチャがカメラを
// 予約していることがある。登録のところで見なければ、その予約をすり抜けた列挙が
// 1 本走り、起動と取り合う。
func TestCameraModesRechecksTheReservationWhenItRegisters(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)

	// 呼び出し側の判定 (capturing) を通った後に、キャプチャが予約した状況。
	// listModesOnce を直に呼んで、その順序を作る。
	b.openingCameraForTest("Bigeye")
	if _, err := b.listModesOnceForTest(context.Background(), "Bigeye"); err == nil {
		t.Error("expected the registration to see the reservation the caller missed")
	}
	if got := lister.count(); got != 0 {
		t.Errorf("ffmpeg ran %d times for a camera already reserved, want 0", got)
	}
}

// 同じ 1 台は、どちらの名前で指されても同じ列挙として数えなければならない。
//
// 画面は "@device_pnp_..." でも訊けるので、打たれた文字列をそのまま鍵にすると、
// 別々の鍵で同じカメラを 2 回開く。画面が代替名で調べている最中に Apply が
// フレンドリ名で起動する、という形が実際に起こる。
func TestCameraModesCountsBothNamesOfACameraAsOne(t *testing.T) {
	onWindowsCameraRules(t)

	var probes atomic.Int64
	running := make(chan struct{})
	release := make(chan struct{})
	var started sync.Once
	prev := listModes
	listModes = func(ctx context.Context, _, _ string) ([]source.Mode, error) {
		probes.Add(1)
		started.Do(func() { close(running) })
		<-release
		return nil, ctx.Err()
	}
	t.Cleanup(func() { listModes = prev })

	prevDevices := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}, nil
	}
	t.Cleanup(func() { listDevices = prevDevices })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	_ = b.Start(context.Background())
	t.Cleanup(b.Stop)
	// 顔ぶれを数えておく。素性はここから引く。
	b.Devices(context.Background())
	b.SetPaused(true)

	// 画面は代替名で調べている。
	go b.CameraModes(context.Background(), "@device_pnp_bigeye") //nolint:errcheck // 答えは見ない
	<-running

	// キャプチャはフレンドリ名で起動する。走っている列挙を見つけられなければ、
	// 手放させないまま開きに行く。
	resumed := make(chan error, 1)
	go func() { resumed <- b.SetPaused(false) }()

	early := false
	select {
	case err := <-resumed:
		early = true
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		t.Error("capture started without taking the camera from the lookup running under the other name")
	case <-time.After(50 * time.Millisecond):
		// 起動は列挙が手放すのを待っている。これが正しい。
	}

	close(release)
	if !early {
		if err := <-resumed; err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("opened the camera %d times, want 1", got)
	}
}

// 初めて数えた顔ぶれは、変化ではない。
//
// カメラを訊く順序は決まっていない。一覧より先にモードを訊く経路があるので、
// 最初の一覧で捨てる作りにすると、そこで憶えたものが 5 秒後に流れる。
func TestCameraModesForgetsWhatItLearnedBeforeItKnewTheCameras(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}, nil
	}
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)
	// 一覧より先に訊く。ここで憶えるものに素性は付けられない。
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Fatalf("CameraModes while paused: %v", err)
	}
	b.SetPaused(false)

	// ここが最初の一覧。訊いてから数えるまでの間に同じ名前の別機種へ差し替わって
	// いても、それを言う材料は無い。今の素性を貼ると以後の照合も素通りするので、
	// 分からないものは捨てる。
	b.Devices(context.Background())
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err == nil {
		t.Error("kept modes it could not tie to a camera, and stamped them with the identity it happened to find")
	}

	// 掴んでいなければ訊き直せる。捨てたことが行き止まりにはならない。
	b.SetPaused(true)
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Errorf("could not learn the modes again after they were dropped: %v", err)
	}
	// 今度は素性が付いている。次の一覧では残る。
	b.Devices(context.Background())
	b.SetPaused(false)
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Errorf("dropped modes that were tied to a camera it had counted: %v", err)
	}
}

// 列挙そのものに失敗したときは、顔ぶれが変わったことにしてはいけない。
// 「1 台も見つからない」と「見に行けなかった」は違う。
func TestCameraModesKeepsItsMemoryWhenTheDeviceListFails(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	cameras := []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}
	var listErr error
	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		if listErr != nil {
			return nil, listErr
		}
		return cameras, nil
	}
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	// 一度は数えられている状態にしてから壊す。数える前の失敗は、初回として
	// 素通りするので何も確かめられない。
	b.Devices(context.Background())
	b.SetPaused(true)
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Fatalf("CameraModes while paused: %v", err)
	}
	b.SetPaused(false)

	listErr = errors.New("uvc: ffmpeg is not installed")
	b.Devices(context.Background())
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Errorf("a failed device listing threw the memory away: %v", err)
	}
}

// 排他的でないプラットフォームでは、掴んでいることを理由に断ってはいけない。
//
// 列挙が実際にデバイスを開くのは DirectShow だけで、他では ListModes が何も
// 開かずに空を返す。そこで断ると、無言のはずの機能が、案内した先 — トレイの
// 一時停止 — が存在しない環境で、直しようのないエラーになる。
func TestCameraModesDoesNotClaimExclusivityWhereThereIsNone(t *testing.T) {
	lister := &fakeModeLister{}
	lister.install(t)
	exclusiveCameraAccess = false

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	modes, err := b.CameraModes(context.Background(), "Bigeye")
	if err != nil {
		t.Fatalf("CameraModes: %v", err)
	}
	if len(modes) != 0 {
		t.Errorf("modes = %v, want the silent empty answer", modes)
	}
	if got := lister.count(); got != 1 {
		t.Errorf("asked %d times, want the platform's own answer to decide", got)
	}
}

// 握っていないカメラの失敗に、一時停止の助言を付けてはいけない。それはただの
// 名前の打ち間違いで、一時停止しても何も変わらない。
func TestCameraModesDoesNotBlameItselfForAnotherCamera(t *testing.T) {
	lister := &fakeModeLister{err: errors.New("uvc: ffmpeg listed no modes for \"Typo\"")}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	_, err := b.CameraModes(context.Background(), "Typo")
	if err == nil {
		t.Fatal("expected an error when the camera cannot be opened")
	}
	if strings.Contains(err.Error(), "pause capture") {
		t.Errorf("error = %q, want no advice about a camera the bridge is not holding", err)
	}
}

// 一時停止中はカメラを解放している。そこは実際に開けるので、憶えを取りに行く
// 唯一の機会になる。
func TestCameraModesLooksAgainWhilePaused(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)

	before := lister.count()
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Fatalf("CameraModes while paused: %v", err)
	}
	if got := lister.count() - before; got != 1 {
		t.Errorf("ffmpeg ran %d times while paused, want 1", got)
	}
}

// 憶えは呼び出し側に渡した後も、こちらのものであり続けなければならない。
func TestCameraModesDoesNotHandOutItsOwnMemory(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Fatalf("CameraModes: %v", err)
	}

	// ここから先は憶えから答える経路。渡したものを書き換えられても、次の答えは
	// 変わってはいけない。
	lister.modes, lister.err = nil, errors.New("uvc: ffmpeg listed no modes")
	got, err := b.CameraModes(context.Background(), "Bigeye")
	if err != nil {
		t.Fatalf("CameraModes: %v", err)
	}
	got[0].MinSize = "scribbled"

	again, err := b.CameraModes(context.Background(), "Bigeye")
	if err != nil {
		t.Fatalf("CameraModes: %v", err)
	}
	if again[0].MinSize != "640x480" {
		t.Errorf("remembered mode = %q, want the caller not to be able to change it", again[0].MinSize)
	}
}

// 動いているカメラは、代替名で訊かれても開きに行ってはいけない。
//
// capturing は名前しか見ないので、設定がフレンドリ名を持ち、画面が
// "@device_pnp_..." で訊けば、そこは素通りします。素性を知っているのは登録の
// 側なので、実際に開く手前でもう一度、素性で見なければなりません。
func TestCameraModesDoesNotReopenTheRunningCameraUnderItsOtherName(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}, nil
	}
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	// 顔ぶれを数えておく。素性はここから引く。
	b.Devices(context.Background())

	before := lister.count()
	if _, err := b.CameraModes(context.Background(), "@device_pnp_bigeye"); err == nil {
		t.Error("expected the camera the bridge is holding to be left alone under its other name")
	}
	if got := lister.count() - before; got != 0 {
		t.Errorf("ffmpeg ran %d times for the camera the bridge is holding, want 0", got)
	}

	// 別のカメラは今までどおり調べられる。
	if _, err := b.CameraModes(context.Background(), "Newcomer"); err != nil {
		t.Errorf("CameraModes for another camera: %v", err)
	}
}

// 調べている間に差し替わったカメラの答えは、憶えてはいけない。
//
// 列挙は 15 秒かかることがあります。その間に同じ名前の別機種へ差し替わると、
// 返ってきた答えは今そこにいるカメラのものではありません。今の素性を貼って
// 憶えると、後の照合も素通りして、二度と捨てられなくなります。
func TestCameraModesDoesNotRememberTheModesOfACameraThatWasSwappedOut(t *testing.T) {
	onWindowsCameraRules(t)

	running := make(chan struct{})
	release := make(chan struct{})
	var started sync.Once
	prev := listModes
	listModes = func(context.Context, string, string) ([]source.Mode, error) {
		started.Do(func() { close(running) })
		<-release
		return []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}, nil
	}
	t.Cleanup(func() { listModes = prev })

	alternative := "@device_pnp_bigeye"
	prevDevices := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{{Name: "Bigeye", Alternative: alternative}}, nil
	}
	t.Cleanup(func() { listDevices = prevDevices })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)
	b.Devices(context.Background())

	answered := make(chan error, 1)
	go func() {
		_, err := b.CameraModes(context.Background(), "Bigeye")
		answered <- err
	}()
	<-running

	// 調べている最中に、同じ名前の別機種へ差し替わる。
	alternative = "@device_pnp_newcomer"
	b.Devices(context.Background())

	close(release)
	// 訊いた人にも渡してはいけない。憶えないだけでは、画面が名前の変わらない
	// まま受け取って、今そこにいるカメラが持っていない候補を並べる。
	if err := <-answered; err == nil {
		t.Error("handed back the modes of the camera that was swapped out while they were being listed")
	}

	// 憶えてもいないこと。憶えていれば、掴んだ後もそれで答えてしまう。
	b.SetPaused(false)
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err == nil {
		t.Error("remembered the modes of the camera that was swapped out while they were being listed")
	}
}

// 走っている列挙は、その最中に顔ぶれを数え直しても見つけられなければならない。
//
// 登録の鍵を素性にすると、登録した後に Devices が素性を埋めただけで鍵が変わり、
// 走っているものを誰も引けなくなる。2 本目の ffmpeg が同じカメラへ向かう。
func TestCameraModesJoinsALookupThatStartedBeforeTheCamerasWereCounted(t *testing.T) {
	onWindowsCameraRules(t)

	var probes atomic.Int64
	running := make(chan struct{})
	release := make(chan struct{})
	var started sync.Once
	prev := listModes
	listModes = func(context.Context, string, string) ([]source.Mode, error) {
		probes.Add(1)
		started.Do(func() { close(running) })
		<-release
		return nil, nil
	}
	t.Cleanup(func() { listModes = prev })

	prevDevices := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}, nil
	}
	t.Cleanup(func() { listDevices = prevDevices })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.SetPaused(true)

	// 顔ぶれを数える前に始める。この時点で引ける素性は無い。
	first := make(chan error, 1)
	go func() {
		_, err := b.CameraModes(context.Background(), "Bigeye")
		first <- err
	}()
	<-running

	// 走っている最中に数える。ここで素性が付く。
	b.Devices(context.Background())

	second := make(chan error, 1)
	go func() {
		_, err := b.CameraModes(context.Background(), "Bigeye")
		second <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for b.listingWaitersForTest() < 1 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("the second call did not join the running enumeration; the camera was opened %d times", probes.Load())
		}
		time.Sleep(time.Millisecond)
	}
	close(release)

	if err := <-first; err != nil {
		t.Errorf("CameraModes: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("CameraModes: %v", err)
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("opened the camera %d times, want 1", got)
	}
}

// 同じ列挙を、キャプチャの起動も見つけられなければならない。見つけられなければ、
// 掴んだままの ffmpeg を残して開きに行く。
func TestStartingCaptureTakesTheCameraFromALookupThatPredatesTheCount(t *testing.T) {
	onWindowsCameraRules(t)

	running := make(chan struct{})
	release := make(chan struct{})
	var started sync.Once
	prev := listModes
	listModes = func(ctx context.Context, _, _ string) ([]source.Mode, error) {
		started.Do(func() { close(running) })
		// 諦めろと言われても、すぐには手放さない。すぐ返ると、起動が待ったのか
		// 素通りしたのかを見分けられない。
		<-release
		return nil, ctx.Err()
	}
	t.Cleanup(func() { listModes = prev })

	prevDevices := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}, nil
	}
	t.Cleanup(func() { listDevices = prevDevices })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	_ = b.Start(context.Background())
	t.Cleanup(b.Stop)
	b.SetPaused(true)

	go b.CameraModes(context.Background(), "Bigeye") //nolint:errcheck // 答えは見ない
	<-running

	// 走っている最中に数える。鍵が変わるのはここ。
	b.Devices(context.Background())

	resumed := make(chan error, 1)
	go func() { resumed <- b.SetPaused(false) }()

	early := false
	select {
	case err := <-resumed:
		early = true
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		t.Error("capture started without taking the camera from the lookup registered before the count")
	case <-time.After(50 * time.Millisecond):
		// 起動は列挙が手放すのを待っている。これが正しい。
	}

	close(release)
	if !early {
		if err := <-resumed; err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
}

// フレンドリ名が重複しているとき、その名前はどの個体でもあり得る。
//
// どれか 1 台に決めると、決めなかった側を「別のカメラ」と答えることになる。
// 設定が共有名を持ち、画面がもう一方の代替名で訊けば、動いているカメラへ列挙が
// 向かう。排他の判定で迷ったときは、衝突する側へ倒さなければならない。
func TestCameraModesTreatsASharedNameAsEveryCameraThatHasIt(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{
			{Name: "USB Camera", Alternative: "@device_pnp_one"},
			{Name: "USB Camera", Alternative: "@device_pnp_two"},
		}, nil
	}
	t.Cleanup(func() { listDevices = prev })

	// 設定は共有名を持っている。既存の設定も API もそれを使える。
	b := New(uvcConfig("USB Camera"), "", hub.New(), status.New(), discardLogger())
	b.Devices(context.Background())

	// どちらの個体を訊かれても、掴んでいる可能性がある。
	for _, name := range []string{"@device_pnp_one", "@device_pnp_two", "USB Camera"} {
		before := lister.count()
		if _, err := b.CameraModes(context.Background(), name); err == nil {
			t.Errorf("CameraModes(%q) went ahead while a camera of that name is being captured", name)
		}
		if got := lister.count() - before; got != 0 {
			t.Errorf("ffmpeg ran %d times for %q, want 0", got, name)
		}
	}

	// 名前を共有していないカメラは、今までどおり調べられる。
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Errorf("CameraModes for a camera with its own name: %v", err)
	}
}

// 憶えは、同じ 1 台を指す別名からも引けなければならない。
//
// 憶えるのは訊かれた名前だが、訊く名前は場面で変わる。一時停止中にフレンドリ名で
// 憶えたものを、キャプチャ中に "@device_pnp_..." で訊かれる形が実際に起こる。
// そこで引けないと、掴んでいて調べ直せないカメラについて、答えを持っているのに
// エラーを返すことになる。
func TestCameraModesAnswersUnderTheOtherNameOfTheSameCamera(t *testing.T) {
	want := []source.Mode{{Format: "mjpeg", MinSize: "640x480", MaxSize: "640x480", MinFPS: 30, MaxFPS: 30}}
	lister := &fakeModeLister{modes: want}
	lister.install(t)

	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}, nil
	}
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.Devices(context.Background())

	// 一時停止中にフレンドリ名で憶える。
	b.SetPaused(true)
	if _, err := b.CameraModes(context.Background(), "Bigeye"); err != nil {
		t.Fatalf("CameraModes while paused: %v", err)
	}
	b.SetPaused(false)

	// キャプチャ中に代替名で訊かれる。開きに行けない — その判定は素性で正しく
	// 効く — ので、憶えから答えられなければ何も出せない。
	got, err := b.CameraModes(context.Background(), "@device_pnp_bigeye")
	if err != nil {
		t.Fatalf("CameraModes under the other name: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("modes = %v, want the remembered %v", got, want)
	}
}

// 重なった要求は 1 本にまとめる。ただし**走っているものには相乗りさせない**。
//
// 走っている列挙は、後から来た要求より前に顔ぶれを見ている。差し替えに気づいて
// 数え直しに来た要求へその答えを渡すと、差し替え前の一覧を「数え直した結果」として
// 受け取ることになる。画面側の繰り越しは同じことをページの中でしているが、あちらが
// 知っているのはそのページのことだけで、別のタブや診断画面が始めた列挙には効かない。
//
// 予約は 1 本に共有させる。3 本重なっても払うのは列挙 2 回分。
func TestDevicesQueuesAScanForRequestsThatArriveMidListing(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		if calls.Add(1) == 1 {
			once.Do(func() { close(started) })
			<-release
			return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_old"}}, nil
		}
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_new"}}, nil
	}
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())

	first := make(chan server.Devices, 1)
	go func() { first <- b.Devices(context.Background()) }()
	<-started

	// 1 本目が顔ぶれを見た**後**に来た 2 本。差し替えはこの間に起きたかもしれない。
	const late = 2
	answers := make(chan server.Devices, late)
	for range late {
		go func() { answers <- b.Devices(context.Background()) }()
	}
	// 予約に並ぶのを待つ。待たずに解くと、1 本目が終わった後に来ることがあり、
	// 予約しているのかどうかを区別できない。
	waitFor(t, 2*time.Second, "the late requests to queue a scan", func() bool {
		return b.scanWaitersForTest() == late
	})

	close(release)

	if devices := <-first; len(devices.Cameras) != 1 || devices.Cameras[0].Alternative != "@device_pnp_old" {
		t.Errorf("the first request got %v, want the listing it started", devices.Cameras)
	}
	for range late {
		devices := <-answers
		if devices.CameraError != "" {
			t.Fatalf("Devices: %s", devices.CameraError)
		}
		if len(devices.Cameras) != 1 || devices.Cameras[0].Alternative != "@device_pnp_new" {
			t.Errorf("a request that arrived mid-listing got %v, want a listing that started after it asked", devices.Cameras)
		}
	}

	b.Stop()
	if got := calls.Load(); got != 2 {
		t.Errorf("ffmpeg ran %d times for %d requests, want 2 — the late ones share one queued scan", got, late+1)
	}
}

// 予約した要求の 1 本が去っても、走っている列挙も予約も止めてはいけない。
//
// 止めれば、同じ答えを待っている他の要求まで巻き添えになる。しかもこの列挙は
// 顔ぶれの照らし合わせも兼ねていて、その成果は待っている人だけのものではない。
func TestDevicesKeepsListingWhenOneRequestGivesUp(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	prev := listDevices
	listDevices = func(ctx context.Context, _ string) ([]source.Device, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_old"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())

	stayed := make(chan server.Devices, 1)
	go func() { stayed <- b.Devices(context.Background()) }()
	<-started

	// 2 本目は次の列挙を予約してから諦める。
	leaving, giveUp := context.WithCancel(context.Background())
	left := make(chan server.Devices, 1)
	go func() { left <- b.Devices(leaving) }()
	waitFor(t, 2*time.Second, "the second request to queue a scan", func() bool {
		return b.scanWaitersForTest() == 1
	})
	giveUp()

	if devices := <-left; devices.CameraError == "" {
		t.Error("the request that gave up reported no error, so it waited for something it no longer needed")
	}

	close(release)
	devices := <-stayed
	if devices.CameraError != "" {
		t.Fatalf("the request that stayed lost its answer: %s", devices.CameraError)
	}
	if len(devices.Cameras) != 1 {
		t.Errorf("cameras = %v, want the listing to have finished for the caller that stayed", devices.Cameras)
	}
	b.Stop()
}

// 顔ぶれを反映するのは、いつでも**最後に終わった列挙**でなければならない。
//
// 反映を登録の解除と同じ区間でやらないと、次の列挙が先に反映を済ませているところへ
// 古い顔ぶれを上書きできる。差し替えた直後にそれが起きると、素性が前のカメラへ
// 巻き戻り、動いているカメラを「別のカメラ」と答えるようになる。
func TestDevicesReflectsTheNewestListing(t *testing.T) {
	lister := &fakeModeLister{modes: []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}}
	lister.install(t)

	var calls atomic.Int64
	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		if calls.Add(1) == 1 {
			return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_old"}}, nil
		}
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_new"}}, nil
	}
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())

	b.Devices(context.Background())
	b.Devices(context.Background())

	// 新しい代替名は、動いているカメラと同じ 1 台を指していなければならない。
	// 巻き戻っていれば、それは知らない名前になり、開きに行ってしまう。
	before := lister.count()
	if _, err := b.CameraModes(context.Background(), "@device_pnp_new"); err == nil {
		t.Error("the newest alternative name is not tied to the running camera; an older listing rolled it back")
	}
	if got := lister.count() - before; got != 0 {
		t.Errorf("ffmpeg ran %d times for the camera the bridge is holding, want 0", got)
	}
}

// 曖昧な名前に、別の個体のモードを渡してはいけない。
//
// 排他の判定で使う「同じ可能性がある」は開きに行かないための保守側で、答えを
// 渡す理由にはならない。フレンドリ名を共有する 2 台があるとき、共有名で開かれる
// のは常に同じ 1 台なので、もう一方のモードを渡すと、そのカメラが持っていない
// 値を勧めることになる。
func TestCameraModesDoesNotAnswerASharedNameWithTheOtherCamerasModes(t *testing.T) {
	other := []source.Mode{{Format: "mjpeg", MinSize: "1280x720", MaxSize: "1280x720", MinFPS: 30, MaxFPS: 30}}
	lister := &fakeModeLister{modes: other}
	lister.install(t)

	prev := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{
			{Name: "USB Camera", Alternative: "@device_pnp_one"},
			{Name: "USB Camera", Alternative: "@device_pnp_two"},
		}, nil
	}
	t.Cleanup(func() { listDevices = prev })

	b := New(uvcConfig("USB Camera"), "", hub.New(), status.New(), discardLogger())
	b.Devices(context.Background())

	// 一時停止中に、2 台目を代替名で調べて憶える。
	b.SetPaused(true)
	if _, err := b.CameraModes(context.Background(), "@device_pnp_two"); err != nil {
		t.Fatalf("CameraModes for the second camera: %v", err)
	}
	b.SetPaused(false)

	// 共有名でのキャプチャ中に、共有名で訊かれる。憶えているのは、その名前が
	// 開く 1 台のものだとは言えない。
	got, err := b.CameraModes(context.Background(), "USB Camera")
	if err == nil {
		t.Errorf("answered the shared name with %v, which was learned from a camera it cannot tie to that name", got)
	}
}

// 曖昧な名前の列挙に、別の個体を訊いた要求を合流させてはいけない。
//
// 共有名で ffmpeg が開くのは、その名前を持つ 1 台だけ。その答えをもう一方の
// 代替名で訊いた画面へ渡すと、別の機種の解像度を「対応している」として勧める
// ことになる。かといって 2 本目を開けば、同じカメラだった場合に取り合う。
// どちらもしない — 断る。
func TestCameraModesDoesNotShareASharedNameLookupWithOneOfTheCameras(t *testing.T) {
	onWindowsCameraRules(t)

	var probes atomic.Int64
	running := make(chan struct{})
	release := make(chan struct{})
	var started sync.Once
	prev := listModes
	listModes = func(context.Context, string, string) ([]source.Mode, error) {
		probes.Add(1)
		started.Do(func() { close(running) })
		<-release
		return []source.Mode{{MinSize: "1280x720", MaxSize: "1280x720"}}, nil
	}
	t.Cleanup(func() { listModes = prev })

	prevDevices := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{
			{Name: "USB Camera", Alternative: "@device_pnp_one"},
			{Name: "USB Camera", Alternative: "@device_pnp_two"},
		}, nil
	}
	t.Cleanup(func() { listDevices = prevDevices })

	b := New(uvcConfig("USB Camera"), "", hub.New(), status.New(), discardLogger())
	b.Devices(context.Background())
	b.SetPaused(true)

	shared := make(chan error, 1)
	go func() {
		_, err := b.CameraModes(context.Background(), "USB Camera")
		shared <- err
	}()
	<-running

	// 期限を切って訊く。合流させる作りだと、この要求はその 1 本を待ってしまう —
	// 期限が切れたことで、断らずに待ったと分かる。
	ask, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	modes, err := b.CameraModes(ask, "@device_pnp_two")
	switch {
	case err == nil:
		t.Errorf("answered %v from a lookup that may have opened the other camera", modes)
	case errors.Is(err, context.DeadlineExceeded):
		t.Error("waited on a lookup that may have opened the other camera instead of refusing")
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("opened a camera %d times, want 1 — the second request must neither join nor open its own", got)
	}

	close(release)
	if err := <-shared; err != nil {
		t.Errorf("CameraModes for the shared name: %v", err)
	}
}

// 個体が一意に決まるなら、走っている列挙には合流しなければならない。断ると、
// 同じ 1 台を 2 度開くか、待てば得られた答えを捨てることになる。
func TestCameraModesJoinsTheLookupRunningUnderTheOtherNameOfTheSameCamera(t *testing.T) {
	onWindowsCameraRules(t)

	want := []source.Mode{{MinSize: "640x480", MaxSize: "640x480"}}
	var probes atomic.Int64
	running := make(chan struct{})
	release := make(chan struct{})
	var started sync.Once
	prev := listModes
	listModes = func(context.Context, string, string) ([]source.Mode, error) {
		probes.Add(1)
		started.Do(func() { close(running) })
		<-release
		return want, nil
	}
	t.Cleanup(func() { listModes = prev })

	prevDevices := listDevices
	listDevices = func(context.Context, string) ([]source.Device, error) {
		return []source.Device{{Name: "Bigeye", Alternative: "@device_pnp_bigeye"}}, nil
	}
	t.Cleanup(func() { listDevices = prevDevices })

	b := New(uvcConfig("Bigeye"), "", hub.New(), status.New(), discardLogger())
	b.Devices(context.Background())
	b.SetPaused(true)

	first := make(chan error, 1)
	go func() {
		_, err := b.CameraModes(context.Background(), "Bigeye")
		first <- err
	}()
	<-running

	joined := make(chan []source.Mode, 1)
	errs := make(chan error, 1)
	go func() {
		modes, err := b.CameraModes(context.Background(), "@device_pnp_bigeye")
		joined <- modes
		errs <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for b.listingWaitersForTest() < 1 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("the other name did not join the running lookup; the camera was opened %d times", probes.Load())
		}
		time.Sleep(time.Millisecond)
	}
	close(release)

	if err := <-first; err != nil {
		t.Errorf("CameraModes: %v", err)
	}
	if err := <-errs; err != nil {
		t.Errorf("CameraModes under the other name: %v", err)
	}
	if modes := <-joined; !slices.Equal(modes, want) {
		t.Errorf("modes = %v, want the shared %v", modes, want)
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("opened the camera %d times, want 1", got)
	}
}

// 札は「設定がどれか」を指す。中身から導くので、同じ設定なら同じ札になる。
//
// 数え上げにしないのは、プロセスをまたいでも意味を保つため。起動のたびに 1 から
// 数え直す札は、再起動を挟むと別の設定に同じ札が付く。それを条件にした変更は、
// 読んだものとは違う設定の上に、通ってよいものとして載る。
func TestTheSettingsTokenNamesTheSettingsThemselves(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	first := config.Token(b.Snapshot())

	changed := b.Snapshot()
	changed.Transform.Rotate = 180
	if _, _, err := b.Apply(ctx, changed, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if second := config.Token(b.Snapshot()); second == first {
		t.Errorf("the token did not move when the settings did (%s)", second)
	}

	// 同じ設定をそのまま送り直しても札は動かない。これは珍しい操作ではなく、
	// 死んだドライバを起こす手段でもあり、書けなかった保存のやり直しでもある。
	same := config.Token(b.Snapshot())
	if _, _, err := b.Apply(ctx, b.Snapshot(), nil); err != nil {
		t.Fatalf("Apply the same settings: %v", err)
	}
	if again := config.Token(b.Snapshot()); again != same {
		t.Errorf("token = %s after applying the same settings, want it to stay at %s", again, same)
	}

	// **再起動をまたいでも同じ意味を保つ。** 別のブリッジでも、設定が同じなら
	// 同じ札、違えば違う札でなければならない。
	fresh := New(b.Snapshot(), "", hub.New(), status.New(), discardLogger())
	if got := config.Token(fresh.Snapshot()); got != same {
		t.Errorf("a restart with the same settings issued %s, want %s", got, same)
	}
	other := b.Snapshot()
	other.Transform.FlipV = true
	restarted := New(other, "", hub.New(), status.New(), discardLogger())
	if got := config.Token(restarted.Snapshot()); got == same {
		t.Errorf("a restart with different settings issued the same token %s; a caller holding it could overwrite them", got)
	}
}

// 条件が食い違えば、何もせずに断る。判定は適用と同じロックの下でなければならない。
// 呼び出し側が先に札を読んで比べても、比べ終えてから適用するまでの間に別の変更が
// 入る。
func TestApplyRefusesAChangeBuiltOnSettingsThatMovedOn(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// 2 つのクライアントが同じ設定を読む。
	mine := b.Snapshot()
	theirs := mine
	read := config.Token(mine)

	// 相手が先に確定させる。
	theirs.Transform.Rotate = 90
	if _, _, err := b.Apply(ctx, theirs, []string{read}); err != nil {
		t.Fatalf("the first change: %v", err)
	}

	// こちらは、もう動作中ではない設定を土台にしている。
	mine.Transform.FlipH = true
	_, _, err := b.Apply(ctx, mine, []string{read})
	if !errors.Is(err, config.ErrRevisionMismatch) {
		t.Fatalf("Apply on a stale token = %v, want config.ErrRevisionMismatch", err)
	}
	if got := b.Snapshot(); got.Transform.Rotate != 90 || got.Transform.FlipH {
		t.Errorf("settings = %+v, want the first change kept and the second refused", got.Transform)
	}

	// 並べた札は、どれか 1 つが一致すれば通る。
	fresh := b.Snapshot()
	fresh.Transform.FlipH = true
	if _, _, err := b.Apply(ctx, fresh, []string{read, config.Token(b.Snapshot())}); err != nil {
		t.Fatalf("Apply offering the current token among others: %v", err)
	}
	if got := b.Snapshot(); got.Transform.Rotate != 90 || !got.Transform.FlipH {
		t.Errorf("settings = %+v, want both changes", got.Transform)
	}
}

// 巻き戻る変更は、他のクライアントが読んだ設定を古くしない。
func TestApplyLeavesTheTokenAloneWhenItRollsBack(t *testing.T) {
	upstream := mjpegUpstream(t, testJPEG(t, 16, 16))
	b := New(mjpegConfig(upstream.URL), "", hub.New(), status.New(), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	before := config.Token(b.Snapshot())

	// 1 フレームも返さないソース。検証に落ちて巻き戻る。
	if _, _, err := b.Apply(ctx, mjpegConfig(silentUpstream(t).URL), nil); err == nil {
		t.Fatal("expected the settings that cannot start a source to be refused")
	}
	if after := config.Token(b.Snapshot()); after != before {
		t.Errorf("token = %s after a change that rolled back, want it to stay at %s", after, before)
	}

	// 前に読んだ札は、まだ使える。
	fresh := b.Snapshot()
	fresh.Transform.Rotate = 270
	if _, _, err := b.Apply(ctx, fresh, []string{before}); err != nil {
		t.Errorf("a token read before a rolled-back change is no longer accepted: %v", err)
	}
}

// serialOutputConfig は、UVC で動きつつシリアル出力を持つ設定です。ソースを
// serial へ切り替えたときに何と突き合わせられるかを見るためのものです。
func serialOutputConfig(t *testing.T, sourcePort, outputPort string, outputOn bool) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Source.Type = config.SourceUVC
	cfg.Source.UVC.Device = "camera"
	cfg.Source.Serial.Port = sourcePort
	cfg.Output.Serial.Enabled = outputOn
	cfg.Output.Serial.Port = outputPort
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the fixture config does not validate: %v", err)
	}
	return cfg
}

// クラッシュの判定は「設定が何と書いてあるか」ではなく「いま何が動いているか」で
// しなければなりません。output.serial.* は起動時にしか読まれないので、設定画面から
// 書き換えて再起動を待っている間、両者は食い違います。
//
// 出力 COM7 で起動し、設定を COM8 に保存 (保留) してから、ソースを serial の
// COM7 へ移す。設定どうしは食い違わないので素通りしますが、動いている出力はまだ
// COM7 を握っているので、ソースは開けません。
func TestSwitchingToSerialClashesWithTheRunningOutputNotTheSavedOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	b := New(serialOutputConfig(t, "COM7", "COM7", true), path, hub.New(), status.New(), discardLogger())

	// 出力ポートだけを動かす。起動時にしか読まれない葉なので、保留になるだけで
	// 動いている出力は COM7 のまま。
	next := b.Snapshot()
	next.Output.Serial.Port = "COM8"
	_, deferred, err := b.Apply(context.Background(), next, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(deferred, "output.serial.port") {
		t.Fatalf("deferred = %v, want it to name output.serial.port so the change is known to be pending", deferred)
	}

	err = b.Switch(context.Background(), config.SourceSerial)
	if err == nil {
		t.Fatal("Switch to serial on COM7 was accepted while the running output still holds COM7")
	}
	if !strings.Contains(err.Error(), "running in this process") {
		t.Errorf("Switch failed with %v, want it to name the clash with the running output", err)
	}
}

// 逆向き。起動時に無効だった出力を有効化しても、それは保留なので、まだ誰も
// ポートを握っていません。そこで拒否すると、存在しない衝突を理由に切り替えを
// 断ることになります。
func TestSwitchingToSerialIgnoresAnOutputThatIsOnlyPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	b := New(serialOutputConfig(t, "COM7", "", false), path, hub.New(), status.New(), discardLogger())

	// ペアの反対側なので、いま動いている出力とも、保存される設定とも衝突しません。
	next := b.Snapshot()
	next.Output.Serial.Enabled = true
	next.Output.Serial.Port = "COM8"
	if _, _, err := b.Apply(context.Background(), next, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 切り替えそのものは、この環境に COM8 も COM7 も無いので失敗して構いません。
	// 確かめたいのは、**衝突を理由に**断られないことです。
	err := b.Switch(context.Background(), config.SourceSerial)
	if err != nil && strings.Contains(err.Error(), "same port") {
		t.Errorf("Switch was refused over a clash that does not exist: %v", err)
	}
}

// 動いている出力と衝突しなくても、**保存される設定**が衝突していれば断ります。
// 通してしまうと、設定は受理・保存されたのに次の起動で main がプロセスごと止め、
// 設定画面から保存できた設定で二度と立ち上がらなくなります。
func TestSavingAnOutputThatWouldClashAtTheNextStartIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	cfg := serialOutputConfig(t, "COM7", "", false)
	cfg.Source.Type = config.SourceSerial
	b := New(cfg, path, hub.New(), status.New(), discardLogger())

	// 起動時の出力は無効なので、いま COM7 を握っているものはありません。しかし
	// この設定を保存すると、次の起動では読む側と書く側が同じ COM7 になります。
	next := b.Snapshot()
	next.Output.Serial.Enabled = true
	next.Output.Serial.Port = "COM7"

	_, _, err := b.Apply(context.Background(), next, nil)
	if err == nil {
		t.Fatal("Apply accepted settings that would stop the next start")
	}
	if !strings.Contains(err.Error(), "next time") {
		t.Errorf("Apply failed with %v, want it to say the settings would break the next start", err)
	}

	// 断ったのだから、保存もされていてはいけません。
	if got := b.Snapshot().Output.Serial; got.Enabled || got.Port == "COM7" {
		t.Errorf("the refused settings were kept anyway: %+v", got)
	}
}

// 同じデバイスを指す 2 通りの書き方を、別物として見逃してはいけません。Windows の
// COM7 と \\.\COM7 は同じポートで、go.bug.st/serial はどちらでも開きます。
func TestTheDeviceNamespacePrefixDoesNotHideAClash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	cfg := serialOutputConfig(t, `\\.\COM7`, "COM7", true)
	cfg.Source.Type = config.SourceSerial
	b := New(cfg, path, hub.New(), status.New(), discardLogger())

	if err := b.Switch(context.Background(), config.SourceSerial); err == nil {
		t.Fatal(`Switch accepted \\.\COM7 against COM7, want them recognised as the same port`)
	} else if !strings.Contains(err.Error(), "same port") {
		t.Errorf("Switch failed with %v, want it to name the port clash", err)
	}
}

// 古い札で来た要求は、まずそのことを告げられなければなりません。先に衝突を見ると、
// 読み直せば消えるかもしれない衝突を理由に断ってしまい、しかも呼び出し側が受け取る
// のは 412 ではなく設定の誤りになります。「読み直してやり直す」という正しい手が
// 取れず、送っていない設定について直し方を考えることになります。
func TestAStaleRevisionIsReportedBeforeAPortClash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ptcambridge.toml")
	cfg := serialOutputConfig(t, "COM7", "", false)
	cfg.Source.Type = config.SourceSerial
	b := New(cfg, path, hub.New(), status.New(), discardLogger())

	// 衝突する設定を、古い札を条件にして送る。両方が当てはまる。
	clashing := b.Snapshot()
	clashing.Output.Serial.Enabled = true
	clashing.Output.Serial.Port = "COM7"

	_, _, err := b.Apply(context.Background(), clashing, []string{"a-token-from-some-older-read"})
	if err == nil {
		t.Fatal("Apply accepted a request built on a revision that is no longer current")
	}
	if !errors.Is(err, config.ErrRevisionMismatch) {
		t.Errorf("Apply failed with %v, want ErrRevisionMismatch so the caller knows to re-read and retry", err)
	}
}
