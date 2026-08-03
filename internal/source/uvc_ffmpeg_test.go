package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// fakeFFmpeg は ffmpeg の代役を書き出す。diag を標準エラー出力に印字して非ゼロで
// 終了する。デバイスを開けなかったときの ffmpeg の振る舞いそのもの。
func fakeFFmpeg(t *testing.T, diag string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in driver is a shell script")
	}
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\necho " + strconv.Quote(diag) + " >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the stand-in ffmpeg: %v", err)
	}
	return path
}

// まだ挿さっていないだけのカメラと、永遠に存在しないカメラは同じことを報告してくる。
// だからドライバはどちらの場合も再試行を続ける。サインイン後に繋がれたカメラも拾える
// 必要があるから。ソースが機能しているかを判断するのはブリッジの仕事で、フレームを
// 待つことでそれを行う。
func TestUVCKeepsRetryingAnUnopenableDevice(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "not-plugged-in-yet",
		FFmpegPath: fakeFFmpeg(t, `[dshow @ 000001] Could not find video device with name "not-plugged-in-yet"`),
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- u.Run(ctx, make(chan core.Frame, 4)) }()

	// 最初の試行、1 秒のバックオフ、2 回目の試行に足りる長さ。
	select {
	case err := <-done:
		t.Fatalf("Run gave up on a device that may still appear: %v", err)
	case <-time.After(2500 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

// ffmpeg が無い場合は話が違う。後からカメラが現れても直らないし、ドライバには
// 再試行する対象が無い。
func TestUVCTreatsAMissingFFmpegAsFatal(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "camera",
		FFmpegPath: filepath.Join(t.TempDir(), "no-such-ffmpeg"),
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- u.Run(ctx, make(chan core.Frame, 4)) }()

	select {
	case err := <-done:
		var fatal *FatalError
		if !errors.As(err, &fatal) {
			t.Fatalf("Run returned %v, want a FatalError", err)
		}
	case <-ctx.Done():
		t.Fatal("Run kept retrying a missing ffmpeg binary")
	}
}

// 詰まったカメラは ffmpeg を終了させず、動いたまま黙らせる。その標準出力に対する
// ブロッキング読み取りは決して返らない。停滞タイムアウトが無ければ再接続ループには
// 到達せず、ブリッジは再起動されるまで死んだままになる。
func TestUVCRecoversFromAnFFmpegThatGoesSilent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in driver is a shell script")
	}
	// 何も出さず、終了もしない。固まった DirectShow フィルタのように。
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatalf("write the stand-in ffmpeg: %v", err)
	}

	u, err := NewUVC(UVCConfig{
		Device:       "camera",
		FFmpegPath:   path,
		StallTimeout: 300 * time.Millisecond,
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- u.Run(ctx, make(chan core.Frame, 4)) }()

	// 停滞タイムアウトが発火し、再試行が始まるのに足りる長さ。
	select {
	case err := <-done:
		t.Fatalf("Run returned instead of retrying: %v", err)
	case <-time.After(2 * time.Second):
	}

	// 本当の証拠はこれ。キャンセルすれば速やかに返る。停滞タイムアウトが無ければ
	// 読み取りは何も書かない子プロセスの上でブロックしたままで、Run はプロセスが
	// 殺されるまでそこに居座る。
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop on cancellation; the read is still blocked")
	}
}

// そのまま流す設定は恒久的に降ろされるので、デバイスが実際に開いたうえで MJPEG を
// 拒否した場合に限らなければならない。まだ挿さっていないだけのカメラも同じ失敗を
// するので、それで固定してしまうと以降すべてのフレームがデコードと再エンコードを
// 払うことになる。
func TestUVCKeepsPassthroughWhenTheDeviceNeverOpened(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "not-plugged-in-yet",
		FFmpegPath: fakeFFmpeg(t, `[dshow @ 000001] Could not find video device with name "not-plugged-in-yet"`),
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_ = u.Run(ctx, make(chan core.Frame, 4))

	if !u.copyCodec {
		t.Error("passthrough was disabled by a device that never opened")
	}
}

// フォールバックは検証中の推測であって、判決ではない。再エンコードは同じデバイスに
// 別の出力形式を要求する。それが動くなら問題は本当に形式の側にあったし、それでも
// 何も出ないならデバイスの側だったということなので、そのまま流す方を復帰させる。
// そうしないと、サインイン時にたまたま使用中だっただけのカメラのせいで、プロセスが
// 生きている限りすべてのフレームがデコードと再エンコードを払うことになる。しかも
// その根拠は、こちらが認識できるとは期待できない標準エラー出力の文字列だ。
func TestUVCCodecChoice(t *testing.T) {
	const busy = "[dshow @ 000001] I/O error"
	const missing = `[dshow @ 000001] Could not find video device with name "camera"`
	failed := errors.New("ffmpeg exited")

	t.Run("a device that never opened keeps passthrough", func(t *testing.T) {
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}
		u.chooseCodec(0, missing, failed)
		if !u.copyCodec {
			t.Error("passthrough was disabled by a device that never opened")
		}
	})

	t.Run("an unrecognised failure is probed and then undone", func(t *testing.T) {
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}

		u.chooseCodec(0, busy, failed)
		if u.copyCodec {
			t.Fatal("re-encoding was never tried")
		}
		u.chooseCodec(0, busy, failed)
		if !u.copyCodec {
			t.Error("re-encoding produced nothing either, so passthrough should be back")
		}
	})

	t.Run("re-encoding that works is confirmed before it settles", func(t *testing.T) {
		const noMJPEG = "Selected video codec mjpeg is not supported by the device"
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}

		u.chooseCodec(0, noMJPEG, failed)
		if u.copyCodec {
			t.Fatal("re-encoding was never tried")
		}
		// 再エンコードでフレームが出たことが言うのは「カメラは動く」であって
		// 「MJPEG を持たない」ではない。その前のそのまま流す試みは、使用中の瞬間に
		// 当たっただけかもしれない。動くと分かったデバイスに対して、そのまま流す方に
		// もう 1 回機会を与える。
		u.chooseCodec(12, "", nil)
		if !u.copyCodec {
			t.Fatal("passthrough was written off on a single comparison")
		}
		if u.reencodeReal {
			t.Fatal("one working re-encode settled it")
		}

		// また失敗した。これで 2 つの結果は、同じ状態にある同じデバイスについての
		// ものになった。
		u.chooseCodec(0, noMJPEG, failed)
		if u.copyCodec || !u.reencodeReal {
			t.Fatal("a second passthrough failure did not settle it")
		}
		// 後でカメラが抜かれても、その結論を覆してはいけない。
		u.chooseCodec(0, busy, failed)
		if u.copyCodec {
			t.Error("passthrough came back after re-encoding had been proven necessary")
		}
		u.chooseCodec(12, "", nil)
		if u.copyCodec {
			t.Error("a working re-encode reopened the question")
		}
	})

	// 確認の手順が存在する理由そのものの場合。そのまま流したときカメラは使用中で、
	// 再エンコードのときは空いていたので、比較は何も証明していない。再試行では
	// そのまま流せて、それが維持される。
	t.Run("a device that was merely busy keeps passthrough", func(t *testing.T) {
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}

		u.chooseCodec(0, busy, failed)
		u.chooseCodec(12, "", nil) // re-encoding, on a camera that has recovered
		if !u.copyCodec {
			t.Fatal("passthrough was not tried again")
		}
		u.chooseCodec(12, "", nil) // and passthrough works
		if !u.copyCodec || u.reencodeReal {
			t.Error("a working passthrough was given up")
		}
		// 後の切断が、以前の推測を蘇らせてはいけない。
		u.chooseCodec(0, busy, failed)
		u.chooseCodec(12, "", nil)
		if !u.copyCodec {
			t.Error("passthrough was written off on the strength of an old failure")
		}
	})

	t.Run("passthrough that works is left alone", func(t *testing.T) {
		u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger(), copyCodec: true}
		u.chooseCodec(12, "", nil)
		if !u.copyCodec || u.reencodeReal {
			t.Error("a working passthrough was changed")
		}
	})
}

// その裏側。開いたものの MJPEG を出せなかったデバイスは再エンコードを試さなければ
// ならない。さもないと一度も動かない。
func TestUVCFallsBackWhenTheDeviceRejectsMJPEG(t *testing.T) {
	u, err := NewUVC(UVCConfig{
		Device:     "camera",
		FFmpegPath: fakeFFmpeg(t, "Selected video codec mjpeg is not supported by the device"),
	}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = u.Run(ctx, make(chan core.Frame, 4))

	if u.copyCodec {
		t.Error("passthrough was kept after the device rejected MJPEG")
	}
}

// ブリッジが自分で取得したコピーは、ユーザーが管理する 3 つの後、最後に見る場所。
// そうでなければ、特定の ffmpeg を意図して指しているインストールが、それを黙って
// 使わなくなる。
func TestUVCPrefersTheUsersFFmpegOverTheFetchedOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup needs an executable bit")
	}

	settings := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", settings)
	t.Setenv("APPDATA", settings)
	fetched := writeExecutable(t, filepath.Join(settings, "PTCamBridge", "bin"), ffmpegBinaryName())

	// 他に何も無い場合。取得したコピーが見つかる。
	t.Setenv("PATH", t.TempDir())
	u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger()}
	if got, err := u.ffmpegPath(); err != nil || got != fetched {
		t.Fatalf("ffmpegPath() = %q, %v; want the fetched copy %q", got, err, fetched)
	}

	// PATH 上のものはそれより優先される。
	onPath := t.TempDir()
	writeExecutable(t, onPath, ffmpegBinaryName())
	t.Setenv("PATH", onPath)
	got, err := u.ffmpegPath()
	if err != nil {
		t.Fatalf("ffmpegPath: %v", err)
	}
	if got == fetched {
		t.Errorf("ffmpegPath() = the fetched copy, want the one on PATH")
	}

	// そして設定による上書きはすべてに優先する。
	configured := writeExecutable(t, t.TempDir(), "my-ffmpeg")
	u.cfg.FFmpegPath = configured
	if got, err := u.ffmpegPath(); err != nil || got != configured {
		t.Errorf("ffmpegPath() = %q, %v; want the configured %q", got, err, configured)
	}
}

// どこにも無い場合、その失敗はユーザーに何ができるかを述べなければならない。
func TestUVCSaysHowToGetFFmpegWhenThereIsNone(t *testing.T) {
	settings := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", settings)
	t.Setenv("APPDATA", settings)
	t.Setenv("PATH", t.TempDir())

	u := &UVC{cfg: UVCConfig{Device: "camera"}, log: discardLogger()}
	_, err := u.ffmpegPath()
	if !errors.Is(err, ErrNoFFmpeg) {
		t.Fatalf("ffmpegPath() error = %v, want ErrNoFFmpeg", err)
	}
	for _, want := range []string{"tray", "ffmpeg_path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// ffmpeg が無い状態は再試行可能なままでなければならない。ブリッジが動いている間に
// トレイから取得できるし、ドライバが完全に諦めてしまえば、取得が終わってもユーザーが
// 再起動するまでカメラは死んだまま。それはこのダウンロード機能が支えるはずの流れ
// そのものだ。
func TestUVCKeepsRetryingWhenThereIsNoFFmpegYet(t *testing.T) {
	settings := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", settings)
	t.Setenv("APPDATA", settings)
	t.Setenv("PATH", t.TempDir())

	u, err := NewUVC(UVCConfig{Device: "camera"}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewUVC: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	runErr := u.Run(ctx, make(chan core.Frame, 4))

	var fatal *FatalError
	if errors.As(runErr, &fatal) {
		t.Fatalf("Run gave up with %v, want it to keep retrying until ffmpeg appears", runErr)
	}
	if runErr != nil {
		t.Errorf("Run = %v, want nil once the context expires", runErr)
	}
}

func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// 新しいユーザーがこのプログラムから最初に目にするものである可能性が最も高い。
// 症状を挙げるだけでは足りない。対処が同じ行に無ければならない。設定ファイルには
// 真似できるものが無く、推測してやれるカメラ名も無いのだから。
func TestUVCSaysHowToNameACameraWhenNoneIsConfigured(t *testing.T) {
	_, err := NewUVC(UVCConfig{}, discardLogger(), nil)
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("NewUVC error = %v, want ErrNoDevice", err)
	}
	for _, want := range []string{"-list-devices", "source.uvc", "device", "PTCAMBRIDGE_UVC_DEVICE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
