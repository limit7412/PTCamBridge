package source

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// 読み取りをまたいで持ち越される未完成のフレームは assembler が受け持つ。そうで
// なければドライバは chunk の境目でデータを失う。
func TestFrameAssemblerCarriesPartialFramesAcrossReads(t *testing.T) {
	a := newFrameAssembler(core.SplitJPEGStream, 0)
	jpg := testJPEG(t)
	stream := append(bytes.Clone(jpg), jpg...)

	var got [][]byte
	for off := 0; off < len(stream); off += 7 {
		end := min(off+7, len(stream))
		got = append(got, a.feed(stream[off:end])...)
	}

	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	for i, f := range got {
		if !bytes.Equal(f, jpg) {
			t.Errorf("frame %d does not match the original", i)
		}
	}
}

// ドライバは読み取りバッファを 1 つ使い回すので、assembler がそれを指すスライスを
// 返してはいけない。
func TestFrameAssemblerFramesSurviveBufferReuse(t *testing.T) {
	a := newFrameAssembler(core.SplitJPEGStream, 0)
	jpg := testJPEG(t)

	scratch := make([]byte, len(jpg))
	copy(scratch, jpg)
	frames := a.feed(scratch)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}

	for i := range scratch {
		scratch[i] = 0xEE
	}
	if !bytes.Equal(frames[0], jpg) {
		t.Error("the frame changed when the read buffer was overwritten")
	}
}

func TestFrameAssemblerReset(t *testing.T) {
	a := newFrameAssembler(core.SplitJPEGStream, 0)
	jpg := testJPEG(t)

	a.feed(jpg[:len(jpg)/2])
	a.reset()
	if frames := a.feed(jpg); len(frames) != 1 {
		t.Fatalf("after reset the assembler returned %d frames, want 1 clean frame", len(frames))
	}
}

func TestRunWithBackoffStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := runWithBackoff(ctx, discardLogger(), "test", nil, func(ctx context.Context) error {
		attempts++
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Errorf("runWithBackoff = %v, want nil on cancellation", err)
	}
	if attempts != 1 {
		t.Errorf("made %d attempts, want 1", attempts)
	}
}

// 設定の誤りは永遠にまったく同じ形で繰り返されるので、再試行しても空回りするだけ。
// ループは諦めてそれを報告しなければならない。
func TestRunWithBackoffGivesUpOnAFatalError(t *testing.T) {
	sentinel := errors.New("bad device name")
	attempts := 0

	err := runWithBackoff(context.Background(), discardLogger(), "test", nil, func(context.Context) error {
		attempts++
		return fatalf(sentinel)
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("runWithBackoff = %v, want the fatal error to surface", err)
	}
	if attempts != 1 {
		t.Errorf("made %d attempts, want 1: a fatal error must not be retried", attempts)
	}
}

func TestRunWithBackoffRetriesAndReportsDisconnects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	reporter := &countingReporter{}
	attempts := 0
	_ = runWithBackoff(ctx, discardLogger(), "test", reporter, func(context.Context) error {
		attempts++
		return errors.New("transient")
	})

	if attempts < 2 {
		t.Errorf("made %d attempts, want at least 2 within the retry window", attempts)
	}
	if reporter.disconnects.Load() < 2 {
		t.Errorf("reported %d disconnects, want one per failed attempt", reporter.disconnects.Load())
	}
}

func TestSendHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 読み手のいない非バッファチャネルは、キャンセルの分岐が無ければ永遠に
	// ブロックする。
	if err := send(ctx, make(chan core.Frame), []byte{0xFF, 0xD8, 0xFF, 0xD9}); err == nil {
		t.Fatal("send should fail once the context is cancelled")
	}
}

func TestParseDshowDevices(t *testing.T) {
	out := `[dshow @ 0000021] "HD Webcam" (video)
[dshow @ 0000021]   Alternative name "@device_pnp_\\?\usb#vid_1234"
[dshow @ 0000021] "Microphone (Realtek)" (audio)
[dshow @ 0000021] "Babble Cam" (video)
`
	devices := parseDshowDevices(out)
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want the 2 video ones", len(devices))
	}
	if devices[0].Name != "HD Webcam" {
		t.Errorf("devices[0].Name = %q", devices[0].Name)
	}
	if devices[0].Alternative == "" {
		t.Error("the alternative name was not attached to the first device")
	}
	if devices[1].Name != "Babble Cam" {
		t.Errorf("devices[1].Name = %q", devices[1].Name)
	}
	if devices[1].Alternative != "" {
		t.Error("the alternative name leaked onto the wrong device")
	}
}

func TestUVCArgs(t *testing.T) {
	u := &UVC{cfg: UVCConfig{Device: "USB Camera", Size: "240x240", Framerate: 30}, log: discardLogger()}

	passthrough := u.args(true)
	if !containsPair(passthrough, "-c:v", "copy") {
		t.Errorf("passthrough args do not copy the stream: %v", passthrough)
	}
	if !containsPair(passthrough, "-f", "mjpeg") || passthrough[len(passthrough)-1] != "pipe:1" {
		t.Errorf("passthrough args do not write MJPEG to stdout: %v", passthrough)
	}
	if !containsPair(passthrough, "-video_size", "240x240") || !containsPair(passthrough, "-framerate", "30") {
		t.Errorf("capture format was not requested: %v", passthrough)
	}

	reencode := u.args(false)
	if !containsPair(reencode, "-c:v", "mjpeg") || !containsPair(reencode, "-q:v", "4") {
		t.Errorf("fallback args do not re-encode: %v", reencode)
	}
	if containsPair(reencode, "-c:v", "copy") {
		t.Errorf("fallback args still copy the stream: %v", reencode)
	}
}

// 既定はモードを決めないので、こちらが何も足さないことが要になる。-video_size や
// -framerate が付いていると、その組み合わせを持っていないカメラは開かない。
func TestUVCArgsLeaveTheModeToTheCamera(t *testing.T) {
	u := &UVC{cfg: UVCConfig{Device: "USB Camera"}, log: discardLogger()}

	for _, args := range [][]string{u.args(true), u.args(false)} {
		if slices.Contains(args, "-video_size") {
			t.Errorf("args ask for a resolution the camera may not have: %v", args)
		}
		if slices.Contains(args, "-framerate") {
			t.Errorf("args ask for a frame rate the camera may not have: %v", args)
		}
		if !containsPair(args, "-i", "video=USB Camera") && !containsPair(args, "-i", "USB Camera") {
			t.Errorf("args do not open the device: %v", args)
		}
	}
}

func TestUVCRejectsAnEmptyDevice(t *testing.T) {
	if _, err := NewUVC(UVCConfig{Device: "  "}, discardLogger(), nil); err == nil {
		t.Fatal("expected an empty device name to be rejected")
	}
}

func TestNewSerialDefaults(t *testing.T) {
	s, err := NewSerial(SerialConfig{}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	if s.cfg.Baud != DefaultSerialBaud {
		t.Errorf("Baud = %d, want %d", s.cfg.Baud, DefaultSerialBaud)
	}
	if s.cfg.Port != AutoPort {
		t.Errorf("Port = %q, want %q", s.cfg.Port, AutoPort)
	}
	if s.Name() != "serial" {
		t.Errorf("Name() = %q, want serial", s.Name())
	}
}

func TestSerialSplitPacketsUsesTheConfiguredHeader(t *testing.T) {
	s, err := NewSerial(SerialConfig{Header: []byte{0xAB, 0xCD}}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	jpg := testJPEG(t)
	packet, err := s.parser.EncodePacket(jpg)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}

	frames, rest := s.splitPackets(packet, 0)
	if len(frames) != 1 || !bytes.Equal(frames[0], jpg) {
		t.Fatalf("got %d frames, want the encoded image back", len(frames))
	}
	if len(rest) != 0 {
		t.Errorf("got %d leftover bytes, want none", len(rest))
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// ffmpeg が実際に書く形。カメラを繋がずに確認できるのはここだけなので、行の形は
// 記録として残しておく。
const dshowListOptionsOutput = `[dshow @ 000001d0a1] DirectShow video device options (from video devices)
[dshow @ 000001d0a1]  Pin "Capture" (alternative pin name "0")
[dshow @ 000001d0a1]   vcodec=mjpeg  min s=1280x720 fps=5 max s=1280x720 fps=30
[dshow @ 000001d0a1]   vcodec=mjpeg  min s=640x480 fps=5 max s=640x480 fps=30
[dshow @ 000001d0a1]   pixel_format=yuyv422  min s=640x480 fps=5 max s=640x480 fps=30
[dshow @ 000001d0a1]   pixel_format=yuyv422  min s=640x480 fps=5 max s=640x480 fps=30
[dshow @ 000001d0a1]   pixel_format=yuyv422  min s=160x120 fps=5 max s=1280x720 fps=29.97
[dshow @ 000001d0a1] Immediate exit requested
`

func TestParseDshowModes(t *testing.T) {
	modes := parseDshowModes(dshowListOptionsOutput)

	// 同じ行が 2 度出ている (ピンが複数あるカメラで起きる) ので、5 行から 4 つ。
	if len(modes) != 4 {
		t.Fatalf("got %d modes, want 4 after folding the duplicate: %v", len(modes), modes)
	}
	// 並べ替えない。カメラが先に挙げるものには意味がある。
	if modes[0].Format != "mjpeg" || modes[0].MinSize != "1280x720" {
		t.Errorf("modes[0] = %+v, want the first line ffmpeg printed", modes[0])
	}
	if modes[0].MaxFPS != 30 {
		t.Errorf("modes[0].MaxFPS = %v, want 30", modes[0].MaxFPS)
	}
	// 範囲で答えるカメラもある。片方に丸めると使える値を隠すことになる。
	last := modes[len(modes)-1]
	if last.MinSize != "160x120" || last.MaxSize != "1280x720" {
		t.Errorf("the ranged mode was flattened: %+v", last)
	}
	if last.MaxFPS != 29.97 {
		t.Errorf("last.MaxFPS = %v, want the fractional rate kept", last.MaxFPS)
	}
}

// この文字列は、カメラが開かなかった人がそのまま設定ファイルへ書き写すもの。
func TestModeReadsLikeSomethingYouCanConfigure(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		want string
	}{
		{Mode{Format: "mjpeg", MinSize: "640x480", MaxSize: "640x480", MinFPS: 30, MaxFPS: 30}, "640x480 @ 30fps (mjpeg)"},
		{Mode{Format: "mjpeg", MinSize: "640x480", MaxSize: "640x480", MinFPS: 5, MaxFPS: 30}, "640x480 @ 5-30fps (mjpeg)"},
		{Mode{Format: "yuyv422", MinSize: "160x120", MaxSize: "1280x720", MinFPS: 5, MaxFPS: 29.97}, "160x120-1280x720 @ 5-29.97fps (yuyv422)"},
		{Mode{MinSize: "240x240", MaxSize: "240x240"}, "240x240"},
	} {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("Mode.String() = %q, want %q", got, tc.want)
		}
	}
}

// 関係のない出力からモードを拾ってはいけない。デバイス一覧と混ざると、存在しない
// 解像度を勧めることになる。
func TestParseDshowModesIgnoresEverythingElse(t *testing.T) {
	if modes := parseDshowModes(`[dshow @ 021] "HD Webcam" (video)
[dshow @ 021]   Alternative name "@device_pnp_\\?\usb#vid_1234"
Could not set video options
`); len(modes) != 0 {
		t.Errorf("parseDshowModes = %v, want nothing from output that has no modes", modes)
	}
}

// "Could not set video options" は何が悪かったかを言うが、代わりに何を書けばよいかは
// 言わない。設定ファイルを開いた人も、ログを読んだ人も、そこで止まる。
func TestRefusedModeFailureCarriesWhatTheCameraOffers(t *testing.T) {
	u := &UVC{
		cfg: UVCConfig{Device: "USB Camera", Size: "240x240", Framerate: 30},
		log: discardLogger(),
		// 調べ済みということにする。ListModes は ffmpeg とカメラを要るので、
		// ここで見たいのは「調べた結果をどう伝えるか」の方。
		modesAsked: true,
		modes: []Mode{
			{Format: "mjpeg", MinSize: "1280x720", MaxSize: "1280x720", MinFPS: 5, MaxFPS: 30},
			{Format: "yuyv422", MinSize: "640x480", MaxSize: "640x480", MinFPS: 5, MaxFPS: 30},
		},
	}

	diag := "Could not set video options\nError opening input: I/O error"
	err := u.explainRefusedMode(context.Background(), diag, errors.New("uvc: ffmpeg closed its output"))
	if err == nil {
		t.Fatal("explainRefusedMode swallowed the failure")
	}
	// 元の失敗は残す。添えるだけで、置き換えてはいけない。
	if !strings.Contains(err.Error(), "ffmpeg closed its output") {
		t.Errorf("error = %v, want the original failure kept", err)
	}
	for _, want := range []string{"USB Camera", "1280x720", "640x480", "mjpeg"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// モードの話でない失敗に、モードの一覧を足してはいけない。カメラが見つからない人に
// 解像度を並べても、探す場所を誤らせるだけ。
func TestOtherFailuresAreNotDressedUpAsModeProblems(t *testing.T) {
	modes := []Mode{{Format: "mjpeg", MinSize: "640x480", MaxSize: "640x480", MaxFPS: 30}}

	for _, tc := range []struct {
		name string
		cfg  UVCConfig
		diag string
	}{
		{
			name: "the device was not found at all",
			cfg:  UVCConfig{Device: "USB Camera", Size: "240x240"},
			diag: "Could not find video device with name [USB Camera]",
		},
		{
			// 何も要求していないのに拒まれたのなら、それはモードの話ではない。
			// 一覧を添えると「空を指定したのが悪い」と読ませてしまう。
			name: "nothing was asked for in the first place",
			cfg:  UVCConfig{Device: "USB Camera"},
			diag: "Could not set video options",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &UVC{cfg: tc.cfg, log: discardLogger(), modesAsked: true, modes: modes}
			original := errors.New("uvc: ffmpeg closed its output")
			err := u.explainRefusedMode(context.Background(), tc.diag, original)
			if err.Error() != original.Error() {
				t.Errorf("error = %v, want the failure left alone", err)
			}
		})
	}
}

// 成功を失敗に変えてはいけない。
func TestExplainRefusedModeLeavesSuccessAlone(t *testing.T) {
	u := &UVC{cfg: UVCConfig{Device: "USB Camera", Size: "240x240"}, log: discardLogger()}
	if err := u.explainRefusedMode(context.Background(), "Could not set video options", nil); err != nil {
		t.Errorf("explainRefusedMode = %v, want nil when the attempt did not fail", err)
	}
}
