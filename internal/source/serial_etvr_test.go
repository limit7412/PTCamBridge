package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// autoSerial は、列挙結果を固定した AutoPort のドライバを作る。ハードウェア無しで
// 候補の巡回を観察できるようにするため。
func autoSerial(t *testing.T, ports ...SerialPort) *Serial {
	t.Helper()
	s, err := NewSerial(SerialConfig{Port: AutoPort}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.listPorts = func() ([]SerialPort, error) { return ports, nil }
	return s
}

func resolve(t *testing.T, s *Serial) string {
	t.Helper()
	name, err := s.resolvePort()
	if err != nil {
		t.Fatalf("resolvePort: %v", err)
	}
	return name
}

// 既知のベンダー ID を持つボードが 2 つあり、そのうちカメラは片方だけ、ということが
// ある。再接続のたびに同じ先頭を返していると、誤った方を永遠に開き続け、本物の
// ボードには決して辿り着かない。
func TestSerialAutoPortMovesOnAfterAFailedCandidate(t *testing.T) {
	s := autoSerial(t,
		SerialPort{Name: "COM3", Vendor: "Silicon Labs CP210x", VID: "10C4"},
		SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"},
	)

	if got := resolve(t, s); got != "COM3" {
		t.Fatalf("first attempt = %q, want COM3", got)
	}
	if got := resolve(t, s); got != "COM7" {
		t.Errorf("second attempt = %q, want the other candidate", got)
	}
}

// すべての候補が一巡したら、諦めずに探索をやり直す。1 分前に抜かれていたボードが
// 戻っていることはある。
func TestSerialAutoPortRestartsTheRotationWhenExhausted(t *testing.T) {
	s := autoSerial(t,
		SerialPort{Name: "COM3", Vendor: "Silicon Labs CP210x", VID: "10C4"},
		SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"},
	)

	want := []string{"COM3", "COM7", "COM3", "COM7"}
	for i, expected := range want {
		if got := resolve(t, s); got != expected {
			t.Fatalf("attempt %d = %q, want %q", i+1, got, expected)
		}
	}
}

// フレームを届けたポートは、切れたときに最初に開き直す相手。ボードが移動したより
// ケーブルが引っ張られた可能性の方が高い。試行は 1 回だけなので、本当に居なくなった
// ボードが巡回を詰まらせることはない。
func TestSerialAutoPortPrefersThePortThatWorked(t *testing.T) {
	s := autoSerial(t,
		SerialPort{Name: "COM3", Vendor: "Silicon Labs CP210x", VID: "10C4"},
		SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"},
	)

	if got := resolve(t, s); got != "COM3" {
		t.Fatalf("first attempt = %q, want COM3", got)
	}
	// 次に開かれるのは COM7 で、フレームを出すのはそちら。
	if got := resolve(t, s); got != "COM7" {
		t.Fatalf("second attempt = %q, want COM7", got)
	}
	s.proven = "COM7"

	if got := resolve(t, s); got != "COM7" {
		t.Errorf("after a drop = %q, want the port that was working", got)
	}
	if got := resolve(t, s); got != "COM3" {
		t.Errorf("after the retry failed = %q, want the rotation to continue", got)
	}
}

// 動いていたが今は抜かれているポートが、探索を止めてはいけない。
func TestSerialAutoPortSkipsAProvenPortThatIsGone(t *testing.T) {
	s := autoSerial(t, SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"})
	s.proven = "COM4"

	if got := resolve(t, s); got != "COM7" {
		t.Errorf("resolvePort = %q, want the candidate that is still attached", got)
	}
}

// 見覚えのあるものが無く、他にあり得るものも無いなら、機械に 1 つしかないポートは
// 試す価値がある。見覚えの無いポートが複数あるなら、それは行う価値のある推測ではない。
func TestSerialAutoPortCandidates(t *testing.T) {
	tests := []struct {
		name        string
		ports       []SerialPort
		want        []string
		wantGuessed bool
	}{
		{
			name:  "the lone port is the fallback",
			ports: []SerialPort{{Name: "COM1"}},
			want:  []string{"COM1"},
			// カメラだと言える材料が何も無いので、その選択が推測であることを
			// 呼び出し側に伝える必要がある。
			wantGuessed: true,
		},
		{
			name:  "several unrecognised ports are not candidates",
			ports: []SerialPort{{Name: "COM1"}, {Name: "COM2"}},
			want:  nil,
		},
		{
			name:  "a recognised port wins over the fallback",
			ports: []SerialPort{{Name: "COM1"}, {Name: "COM2", Vendor: "FTDI"}},
			want:  []string{"COM2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, guessed := autoCandidates(tc.ports)
			if len(got) != len(tc.want) {
				t.Fatalf("autoCandidates = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("autoCandidates = %v, want %v", got, tc.want)
				}
			}
			if guessed != tc.wantGuessed {
				t.Errorf("guessed = %v, want %v", guessed, tc.wantGuessed)
			}
		})
	}
}

func TestSerialAutoPortReportsWhenNothingMatches(t *testing.T) {
	s := autoSerial(t, SerialPort{Name: "COM1"}, SerialPort{Name: "COM2"})
	if _, err := s.resolvePort(); err == nil {
		t.Fatal("expected an error when no port matches a known vendor ID")
	}
}

// 明示されたポートは書かれたとおりに使い、巡回は一切行わない。
func TestSerialExplicitPortIsUsedVerbatim(t *testing.T) {
	s, err := NewSerial(SerialConfig{Port: "COM9"}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.listPorts = func() ([]SerialPort, error) { return nil, errors.New("enumeration must not be reached") }

	for i := 0; i < 3; i++ {
		if got := resolve(t, s); got != "COM9" {
			t.Fatalf("attempt %d = %q, want COM9", i+1, got)
		}
	}
}

// fakePort は、決まったストリームをセッションに流したあと黙る。意味を成さないことを
// 喋るボードと、何も言わないボードは、ここからは同じに見える。
type fakePort struct {
	chunks [][]byte
	closed bool
}

func (p *fakePort) Read(b []byte) (int, error) {
	if len(p.chunks) == 0 {
		// 実際のドライバが報告するのと同じ形の読み取りタイムアウト。
		time.Sleep(time.Millisecond)
		return 0, nil
	}
	n := copy(b, p.chunks[0])
	p.chunks = p.chunks[1:]
	return n, nil
}

func (p *fakePort) SetReadTimeout(time.Duration) error { return nil }

func (p *fakePort) Close() error {
	p.closed = true
	return nil
}

// wiredSerial は、指定した chunk を返すポートを持つドライバを作る。
func wiredSerial(t *testing.T, log *slog.Logger, cfg SerialConfig, chunks ...[]byte) *Serial {
	t.Helper()
	if cfg.Port == "" {
		cfg.Port = "COM4"
	}
	s, err := NewSerial(cfg, log, nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.openPort = func(string, int) (serialPort, error) { return &fakePort{chunks: chunks}, nil }
	return s
}

// runUntilStall はセッションを 1 つ走らせ、それが終わった理由を返す。
func runUntilStall(t *testing.T, s *Serial) error {
	t.Helper()
	restore := serialStallTimeout
	serialStallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { serialStallTimeout = restore })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.session(ctx, make(chan core.Frame, 8))
}

// 無言のポートと、誰にも解析できないバイトで満ちたポートは正反対の対処を要するが、
// 停滞だけでは区別がつかない。区別するのはバイト数。
func TestSerialStallSaysHowMuchArrived(t *testing.T) {
	t.Run("nothing on the wire", func(t *testing.T) {
		s := wiredSerial(t, discardLogger(), SerialConfig{})
		err := runUntilStall(t, s)
		if err == nil {
			t.Fatal("session returned nil, want a stall")
		}
		if !strings.Contains(err.Error(), "0 bytes received") {
			t.Errorf("error = %v, want it to report that nothing arrived", err)
		}
	})

	t.Run("bytes but no packet", func(t *testing.T) {
		s := wiredSerial(t, discardLogger(), SerialConfig{}, bytes.Repeat([]byte{0x5A}, 64))
		err := runUntilStall(t, s)
		if err == nil {
			t.Fatal("session returned nil, want a stall")
		}
		if !strings.Contains(err.Error(), "64 bytes received") {
			t.Errorf("error = %v, want it to report the 64 bytes that arrived", err)
		}
	})
}

// ヘッダーの不一致に決着をつけるのはバイト列そのものなので、探していたヘッダーと
// 一緒にログまで届かなければならない。
func TestSerialStallLogsTheUnparsableStream(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 設定と 1 バイトだけ違う前置き。この設定が存在する理由そのものであり、
	// バイト数だけでは診断できない場合。
	stream := append([]byte{0xFF, 0xA0, 0xFF, 0xB1, 0x10, 0x00}, bytes.Repeat([]byte{0x42}, 32)...)
	s := wiredSerial(t, log, SerialConfig{}, stream)

	if err := runUntilStall(t, s); err == nil {
		t.Fatal("session returned nil, want a stall")
	}

	out := logged.String()
	for _, want := range []string{
		"ff a0 ff b1",     // what actually arrived
		"expected_header", // and what was being looked for
		"ff a0 ff a1",
		"baud",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not mention %q:\n%s", want, out)
		}
	}
	// ストリームの先頭だけ。届いたもの全部のダンプではない。
	if strings.Contains(out, strings.Repeat("42 ", 20)) {
		t.Errorf("log carries more of the stream than the preview:\n%s", out)
	}
}

// 再接続ループは数秒ごとに戻ってくる。毎回同じ警告を繰り返せば、何も足さないまま
// ログを埋め尽くす。
func TestSerialStallWarnsOncePerStream(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	junk := bytes.Repeat([]byte{0x5A}, 64)
	s := wiredSerial(t, log, SerialConfig{}, junk)
	for i := 0; i < 3; i++ {
		s.openPort = func(string, int) (serialPort, error) { return &fakePort{chunks: [][]byte{junk}}, nil }
		if err := runUntilStall(t, s); err == nil {
			t.Fatalf("session %d returned nil, want a stall", i+1)
		}
	}

	if warns := strings.Count(logged.String(), "level=WARN"); warns != 1 {
		t.Errorf("logged %d warnings for one unchanged stream, want 1:\n%s", warns, logged.String())
	}
	// レベルを上げた人のために、繰り返しは debug に残っている。
	if debugs := strings.Count(logged.String(), "level=DEBUG"); debugs != 2 {
		t.Errorf("logged %d debug lines for the repeats, want 2:\n%s", debugs, logged.String())
	}
}

// ストリームが変わったなら改めて言う価値がある。書き換えられたボードや、同じ COM
// 番号に現れた別のデバイスは別の診断だから。ただし際限なくではない。流れている
// ストリームを載せたポートは毎回違う地点で合流するので、開くたびにバイト列は異なり、
// そのたびに警告すれば、この記憶が防ごうとしている氾濫そのものになる。
func TestSerialStallWarnsAboutAChangedStreamButNotForever(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s := wiredSerial(t, log, SerialConfig{})
	for i := 0; i < maxWarnedStreams+2; i++ {
		filler := byte(0x40 + i)
		s.openPort = func(string, int) (serialPort, error) {
			return &fakePort{chunks: [][]byte{bytes.Repeat([]byte{filler}, 64)}}, nil
		}
		if err := runUntilStall(t, s); err == nil {
			t.Fatalf("session %d returned nil, want a stall", i+1)
		}
	}

	if warns := strings.Count(logged.String(), "level=WARN"); warns != maxWarnedStreams {
		t.Errorf("logged %d warnings for %d different streams, want the cap of %d:\n%s",
			warns, maxWarnedStreams+2, maxWarnedStreams, logged.String())
	}
}

// 動き始めたポートは、何が悪かったにせよそれが解消したということ。後でまた壊れたら
// それは新しい苦情であって、古い苦情の繰り返しではない。
func TestSerialStallWarnsAgainAfterThePortHasWorked(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	parser, err := core.NewETVRParser(nil, 0)
	if err != nil {
		t.Fatalf("NewETVRParser: %v", err)
	}
	packet, err := parser.EncodePacket(testJPEG(t))
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	junk := bytes.Repeat([]byte{0x5A}, 64)

	s := wiredSerial(t, log, SerialConfig{}, junk)
	// 解析できない、次に動くセッション、そして最初とまったく同じ形でまた解析できない。
	for _, chunks := range [][][]byte{{junk}, {packet}, {junk}} {
		s.openPort = func(string, int) (serialPort, error) { return &fakePort{chunks: chunks}, nil }
		if err := runUntilStall(t, s); err == nil {
			t.Fatal("session returned nil, want a stall")
		}
	}

	if warns := strings.Count(logged.String(), "level=WARN"); warns != 2 {
		t.Errorf("logged %d warnings, want 2 (the port worked in between):\n%s", warns, logged.String())
	}
}

// "auto" は候補の間を巡回するので、最後のストリームだけを覚えていると、既に報告
// したポートに巡回が戻ってくるたびにまた警告することになる。
func TestSerialStallWarnsOncePerPortAcrossTheRotation(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s, err := NewSerial(SerialConfig{Port: AutoPort}, log, nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.listPorts = func() ([]SerialPort, error) {
		return []SerialPort{{Name: "COM3", Vendor: "Espressif"}, {Name: "COM4", Vendor: "Espressif"}}, nil
	}
	// 各ポートがそれぞれ固有の、解析できないストリームを載せている。
	streams := map[string][]byte{
		"COM3": bytes.Repeat([]byte{0xAA}, 64),
		"COM4": bytes.Repeat([]byte{0xBB}, 64),
	}
	s.openPort = func(name string, _ int) (serialPort, error) {
		return &fakePort{chunks: [][]byte{streams[name]}}, nil
	}

	// A、B、そして一周して再び A。
	for i := 0; i < 3; i++ {
		if err := runUntilStall(t, s); err == nil {
			t.Fatalf("session %d returned nil, want a stall", i+1)
		}
	}

	if warns := strings.Count(logged.String(), "level=WARN"); warns != 2 {
		t.Errorf("logged %d warnings for two ports, want 2:\n%s", warns, logged.String())
	}
}

// 1 回の読み取りが、フレームと次のパケットの先頭を同時に運ぶことがある。フレームが
// 数を無条件に 0 に戻していると、その後パケットの途中で黙ったボードが「何も送って
// いない」と報告されることになり、誤った対処へ導く。
func TestSerialStallKeepsBytesThatFollowedTheLastFrame(t *testing.T) {
	parser, err := core.NewETVRParser(nil, 0)
	if err != nil {
		t.Fatalf("NewETVRParser: %v", err)
	}
	packet, err := parser.EncodePacket(testJPEG(t))
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	// 2 つ目のパケットの先頭。ペイロードは決して届かない。ヘッダーと長さフィールドだけ。
	partial := packet[:parser.HeaderLen()+2]

	s := wiredSerial(t, discardLogger(), SerialConfig{}, append(append([]byte{}, packet...), partial...))
	stallErr := runUntilStall(t, s)
	if stallErr == nil {
		t.Fatal("session returned nil, want a stall")
	}
	want := fmt.Sprintf("%d bytes received", len(partial))
	if !strings.Contains(stallErr.Error(), want) {
		t.Errorf("error = %v, want it to report the %s that followed the frame", stallErr, want)
	}
}

// フレームより後のバイトは、パーサーが保持したものではなく通り過ぎたものから数える。
// パーサーが保持するのはまだパケットの先頭になり得る分だけで、純然たるゴミに対して
// それはせいぜいヘッダー長 -1 バイト。設定が許すとおりヘッダーが 1 バイトなら、
// まったく残らない。
func TestSerialStallCountsUnparsableBytesAfterAFrame(t *testing.T) {
	for _, header := range [][]byte{{0xFF, 0xA0, 0xFF, 0xA1}, {0xFF}} {
		t.Run(hexPreview(header), func(t *testing.T) {
			parser, err := core.NewETVRParser(header, 0)
			if err != nil {
				t.Fatalf("NewETVRParser: %v", err)
			}
			packet, err := parser.EncodePacket(testJPEG(t))
			if err != nil {
				t.Fatalf("EncodePacket: %v", err)
			}
			// パーサーが何も読み取れないゴミであり、ヘッダーのバイトを 1 つも
			// 含まないので、候補として残る部分は無い。
			junk := bytes.Repeat([]byte{0x5A}, 64)

			s := wiredSerial(t, discardLogger(), SerialConfig{Header: header},
				append(append([]byte{}, packet...), junk...))
			stallErr := runUntilStall(t, s)
			if stallErr == nil {
				t.Fatal("session returned nil, want a stall")
			}
			want := fmt.Sprintf("%d bytes received", len(junk))
			if !strings.Contains(stallErr.Error(), want) {
				t.Errorf("error = %v, want it to report the %s that followed the frame", stallErr, want)
			}
		})
	}
}

// 動いていたボードが黙った場合は状況が違う。見せるべき「解析できないストリーム」が
// 存在しないので、何かを見せれば誤解を招く。
func TestSerialStallAfterWorkingSaysNothingAboutTheStream(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	parser, err := core.NewETVRParser(nil, 0)
	if err != nil {
		t.Fatalf("NewETVRParser: %v", err)
	}
	packet, err := parser.EncodePacket(testJPEG(t))
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}

	s := wiredSerial(t, log, SerialConfig{}, packet)
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("session returned nil, want a stall once the board went quiet")
	}
	if strings.Contains(logged.String(), "no packet matched") {
		t.Errorf("a board that worked was reported as unparsable:\n%s", logged.String())
	}
}

// 最後の頼みは、存在する唯一のポートが何であれそれを開く。それは VR ヘッドセット
// かもしれないしプリンタかもしれない。ログがそう言わなければ、続く失敗は壊れた
// カメラボードのように見え、読み手を誤った方向へ走らせる。
func TestSerialAutoFallbackSaysThePickIsAGuess(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	s, err := NewSerial(SerialConfig{Port: AutoPort}, log, nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.listPorts = func() ([]SerialPort, error) {
		return []SerialPort{{Name: "COM4", VID: "28DE", PID: "2102", Product: "Valve Controller"}}, nil
	}

	if got := resolve(t, s); got != "COM4" {
		t.Fatalf("resolvePort = %q, want COM4", got)
	}

	out := logged.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("the guess was not reported as a warning:\n%s", out)
	}
	for _, want := range []string{"28DE:2102", "Valve Controller", "may not be a camera"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not mention %q:\n%s", want, out)
		}
	}
}

// guessSerial は、推測の経路をセッションごと動かすためのドライバ。回数を数えるのは
// 実際にポートを開けたときなので、resolvePort を単体で呼んでも何も進まない。
//
// opened には開こうとしたポート名が順に入る。「開かなくなった」ことを見るには、
// 開いた回数そのものを数えるしかない。
func guessSerial(t *testing.T, ports *[]SerialPort, chunks ...[]byte) (*Serial, *[]string) {
	t.Helper()
	s, err := NewSerial(SerialConfig{Port: AutoPort}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	var opened []string
	s.listPorts = func() ([]SerialPort, error) { return *ports, nil }
	s.openPort = func(name string, _ int) (serialPort, error) {
		opened = append(opened, name)
		return &fakePort{chunks: chunks}, nil
	}
	return s, &opened
}

// 推測は有限回で打ち切る。実機で踏んだのは Valve の VR 機器 (28DE:2102) で、
// ブリッジはそれを 7 秒おきに永久に開き直していた。他人の機器のポートを掴み続けるのは、
// ログを汚す以上のことをしている。
func TestSerialAutoGuessStopsOpeningAPortThatNeverDelivers(t *testing.T) {
	ports := []SerialPort{{Name: "COM4", VID: "28DE", PID: "2102", Product: "Valve Controller"}}
	s, opened := guessSerial(t, &ports)

	for i := 1; i <= maxGuessAttempts; i++ {
		if err := runUntilStall(t, s); err == nil {
			t.Fatalf("session %d returned nil, want a stall", i)
		}
	}
	if len(*opened) != maxGuessAttempts {
		t.Fatalf("opened %v, want exactly %d attempts", *opened, maxGuessAttempts)
	}

	err := runUntilStall(t, s)
	if err == nil {
		t.Fatal("the session kept using the guess after the attempts ran out")
	}
	// 肝心なのは、もうポートを開かないこと。エラーを返すだけで開き続けるなら
	// 何も直っていない。
	if len(*opened) != maxGuessAttempts {
		t.Errorf("opened %v, want the port left alone after %d attempts", *opened, maxGuessAttempts)
	}
	// 対処は「no port matched a known camera board」のときと同じ — ポートを明示する。
	// 画面がその案内を出せるよう、同じ sentinel でなければならない。
	if !errors.Is(err, ErrNoSerialPort) {
		t.Errorf("error = %v, want it to be ErrNoSerialPort so the UI can offer the same remedy", err)
	}
	// 何を諦めたのかが分からなければ、ユーザーはどのポートを疑えばよいのか分からない。
	for _, want := range []string{"COM4", "28DE:2102", "Valve Controller"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// 開けなかった試行は数えない。別のプロセスがポートを掴んでいる間に持ち点を使い切ると、
// 解放された頃には一度も読めていないのに打ち切られる。
func TestSerialAutoGuessDoesNotSpendAttemptsItCouldNotOpen(t *testing.T) {
	ports := []SerialPort{{Name: "COM4", VID: "1234", PID: "5678", Product: "New Board"}}
	s, opened := guessSerial(t, &ports)

	busy := true
	fake := s.openPort
	s.openPort = func(name string, baud int) (serialPort, error) {
		if busy {
			return nil, errors.New("access denied")
		}
		return fake(name, baud)
	}

	// ポートが塞がっている間に、上限より多く試す。
	for i := 0; i < maxGuessAttempts+2; i++ {
		if err := runUntilStall(t, s); err == nil {
			t.Fatalf("attempt %d returned nil, want the open to fail", i+1)
		}
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v, want none: every open failed", *opened)
	}

	// ポートが解放された。持ち点は 1 度も使っていないので、まだ上限まで読める。
	busy = false
	for i := 1; i <= maxGuessAttempts; i++ {
		if err := runUntilStall(t, s); err == nil {
			t.Fatalf("session %d returned nil, want a stall", i)
		}
	}
	if len(*opened) != maxGuessAttempts {
		t.Errorf("opened %v, want %d reads once the port was free", *opened, maxGuessAttempts)
	}
}

// ポート名は使い回される。/dev/ttyUSB0 も COM 番号も、抜き差しで別のデバイスに
// 付け直される。前のデバイスで使い切った持ち点を引き継ぐと、同じ名前で現れた
// 別のボードが一度も開かれないまま拒まれる。
func TestSerialAutoGuessStartsOverWhenTheDeviceBehindTheNameChanges(t *testing.T) {
	ports := []SerialPort{{Name: "COM4", VID: "28DE", PID: "2102", Product: "Valve Controller"}}
	s, opened := guessSerial(t, &ports)

	for i := 1; i <= maxGuessAttempts; i++ {
		runUntilStall(t, s)
	}
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("the guess was not exhausted")
	}

	// 同じ名前に別のデバイスが現れた。
	ports = []SerialPort{{Name: "COM4", VID: "1234", PID: "5678", Product: "New Board"}}
	before := len(*opened)
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("session returned nil, want a stall")
	}
	if len(*opened) != before+1 {
		t.Errorf("opened %v, want the new device to get its own attempt", *opened)
	}
}

// 打ち切るのは「開くこと」だけで、「待つこと」ではない。後からボードを挿した
// ユーザーが、アプリを再起動せずに拾われなければならない。
func TestSerialAutoGuessStillFindsABoardPluggedInLater(t *testing.T) {
	valve := SerialPort{Name: "COM4", VID: "28DE", PID: "2102", Product: "Valve Controller"}
	board := SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"}

	ports := []SerialPort{valve}
	s, opened := guessSerial(t, &ports)

	for i := 1; i <= maxGuessAttempts; i++ {
		runUntilStall(t, s)
	}
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("the guess was not exhausted")
	}

	// ボードを挿した。次の再試行がそれを見つける。
	ports = []SerialPort{valve, board}
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("session returned nil, want a stall on the new board")
	}
	if got := (*opened)[len(*opened)-1]; got != "COM7" {
		t.Errorf("opened %q, want the board that was plugged in", got)
	}
}

// 既知の VID に載っていないだけの本物のボードは実在する。1 枚でもフレームを出したら
// それは推測ではないので、次に切れたときも開き直す。
func TestSerialAutoGuessThatDeliveredIsNotCountedAgainstItself(t *testing.T) {
	ports := []SerialPort{{Name: "COM4", VID: "1234", PID: "5678", Product: "New Board"}}

	probe, err := NewSerial(SerialConfig{}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	packet, err := probe.parser.EncodePacket(testJPEG(t))
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}

	// 毎回 1 枚届けてから黙るポート。届いた時点で推測ではなくなる。
	s, opened := guessSerial(t, &ports, packet)

	for i := 0; i < maxGuessAttempts+3; i++ {
		if err := runUntilStall(t, s); err == nil {
			t.Fatalf("session %d returned nil, want a stall after the frame", i+1)
		}
	}
	if len(*opened) != maxGuessAttempts+3 {
		t.Errorf("opened %v, want every session to reach the port: it delivers", *opened)
	}
}

// 既知のボードは推測ではないので、騒ぎ立ててはいけない。
func TestSerialAutoRecognisedPortIsNotWarnedAbout(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	s, err := NewSerial(SerialConfig{Port: AutoPort}, log, nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.listPorts = func() ([]SerialPort, error) {
		return []SerialPort{{Name: "COM4", Vendor: "Espressif", VID: "303A"}}, nil
	}

	resolve(t, s)
	if strings.Contains(logged.String(), "level=WARN") {
		t.Errorf("a recognised board was reported as a guess:\n%s", logged.String())
	}
}
