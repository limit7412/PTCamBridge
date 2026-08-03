package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/i18n"
	"github.com/limit7412/PTCamBridge/internal/status"
)

// uiRequest は、ループバック宛の要求を組み立てます。httptest の既定の Host は
// example.com で、管理 API の番人はそれを — 正しく — 撥ねます。
func uiRequest(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.Host = "127.0.0.1:18080"
	return req
}

// 画面は管理 API と同じ扱いでなければならない。認証を持たないまま設定ファイルの
// 場所とデバイス名を映すので、ループバックを離れたら消える。
func TestUIFollowsTheAdminSwitch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		admin bool
		want  int
	}{
		{"loopback", true, http.StatusOK},
		{"off loopback", false, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newTestServer(t, Options{EnableAdmin: tc.admin, Controller: &fakeController{}})
			for _, path := range []string{"/ui", "/ui/state"} {
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, uiRequest(http.MethodGet, path, nil))
				if rec.Code != tc.want {
					t.Errorf("GET %s = %d, want %d", path, rec.Code, tc.want)
				}
			}
		})
	}
}

// 画面はループバック宛でない要求には答えない。管理 API と同じ番人を通っている
// ことの確認。
func TestUIRejectsReboundHost(t *testing.T) {
	s, _, _ := newTestServer(t, Options{EnableAdmin: true, Controller: &fakeController{}})

	req := uiRequest(http.MethodGet, "/ui", nil)
	req.Host = "camera.example.com"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /ui with a rebound Host = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// 画面は、書き込みの手段をひとつも持たない。この PR の範囲は診断まで。
func TestUIIsReadOnly(t *testing.T) {
	s, _, _ := newTestServer(t, Options{EnableAdmin: true, Controller: &fakeController{}})

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		for _, path := range []string{"/ui", "/ui/state"} {
			req := uiRequest(method, path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want %d", method, path, rec.Code, http.StatusMethodNotAllowed)
			}
		}
	}
}

// 画面が持ち出せる情報 — 設定ファイルの場所、ログの場所 — は、渡されたときだけ
// 出る。空なら行そのものが無い。
func TestUIShowsThePlacesItWasGiven(t *testing.T) {
	s, _, _ := newTestServer(t, Options{
		EnableAdmin: true,
		Controller:  &fakeController{},
		ConfigPath:  `C:\Users\someone\ptcambridge.toml`,
		LogDir:      `C:\Users\someone\logs`,
		Version:     "1.2.3",
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, uiRequest(http.MethodGet, "/ui", nil))
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui = %d, want %d", rec.Code, http.StatusOK)
	}
	// HTML なので \ は素通りするが、エスケープの有無に賭けずに済むよう部分で見る。
	for _, want := range []string{"ptcambridge.toml", "logs", "1.2.3"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q", want)
		}
	}
	// 外部への参照が 1 つも無いことは、画面がこのプロセスの中で完結している
	// という主張そのもの。
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Error("the page does not declare a content security policy")
	}
}

// 骨格は選ばれた言語で描かれる。
func TestUISpeaksTheConfiguredLanguage(t *testing.T) {
	for lang, want := range map[i18n.Lang]string{
		i18n.Japanese: "診断",
		i18n.English:  "diagnostics",
	} {
		s, _, _ := newTestServer(t, Options{
			EnableAdmin: true,
			Controller:  &fakeController{},
			Printer:     i18n.NewPrinter(lang),
		})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, uiRequest(http.MethodGet, "/ui", nil))
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the %s page does not contain %q", lang, want)
		}
	}
}

// /ui/state は画面に出すとおりの文字列を返す。判断がすべて Go 側にあることの確認。
func TestUIStateIsAlreadyFormatted(t *testing.T) {
	s, frames, tracker := newTestServer(t, Options{EnableAdmin: true, Controller: &fakeController{}})
	tracker.Connected("uvc")
	frames.Publish(core.Frame{Data: testJPEG(t), RecvedAt: time.Now()})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, uiRequest(http.MethodGet, "/ui/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/state = %d, want %d", rec.Code, http.StatusOK)
	}

	var got uiState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Source != "uvc" {
		t.Errorf("source = %q, want %q", got.Source, "uvc")
	}
	if got.State == "" {
		t.Error("the state is empty")
	}
	if !got.HasFrame {
		t.Error("a frame was published but the state does not say so")
	}
	// ffmpeg を組み込んでいないので、行そのものが出てはいけない。導入されて
	// いないことと、そもそも取得の手段が無いことは別。
	if got.HasFFmpeg {
		t.Error("the state offers an ffmpeg line without a fetcher")
	}
}

// 状態の呼び分けは、トレイの 1 行表示と同じ判定でなければならない。2 つの画面が
// 同じ瞬間に違うことを言うのは、何も表示しないより悪い。
func TestUIStateNamesTheSituation(t *testing.T) {
	p := i18n.NewPrinter(i18n.English)
	now := time.Now()

	for _, tc := range []struct {
		name     string
		snapshot status.Snapshot
		want     i18n.Key
	}{
		{"no source", status.Snapshot{}, i18n.UIStateNoSource},
		{"paused", status.Snapshot{Source: "uvc", Paused: true}, i18n.UIStatePaused},
		{"connecting", status.Snapshot{Source: "uvc"}, i18n.UIStateConnecting},
		{"reconnecting", status.Snapshot{Source: "uvc", LastError: "device is gone"}, i18n.UIStateReconnecting},
		{"running", status.Snapshot{Source: "uvc", Connected: true}, i18n.UIStateRunning},
		// 一時停止は接続断より強い。ユーザーが自分で止めたものを障害として
		// 見せてはいけない。
		{"paused wins", status.Snapshot{Source: "uvc", Paused: true, LastError: "device is gone"}, i18n.UIStatePaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := newUIState(p, tc.snapshot, hub.Stats{}, ffmpegView{}, now)
			if want := p.S(tc.want); got.State != want {
				t.Errorf("state = %q, want %q", got.State, want)
			}
		})
	}
}

// ユーザーが対処できる失敗はユーザーの言語で、それ以外はドライバの文言のまま出る。
// ログに載っているものと同じ文字列を見せることが、書き写して検索できるという意味。
func TestUIStateTranslatesOnlyWhatItCan(t *testing.T) {
	p := i18n.NewPrinter(i18n.Japanese)
	now := time.Now()

	named := newUIState(p, status.Snapshot{
		Source:       "uvc",
		LastError:    "ffmpeg was not found",
		LastErrorKey: string(i18n.ErrNoFFmpeg),
	}, hub.Stats{}, ffmpegView{}, now)
	if want := p.S(i18n.ErrNoFFmpeg); named.LastError != want {
		t.Errorf("last error = %q, want the translation %q", named.LastError, want)
	}

	raw := newUIState(p, status.Snapshot{
		Source:    "serial",
		LastError: "read from COM4: access denied",
	}, hub.Stats{}, ffmpegView{}, now)
	if raw.LastError != "read from COM4: access denied" {
		t.Errorf("last error = %q, want the driver's own words", raw.LastError)
	}

	quiet := newUIState(p, status.Snapshot{Source: "uvc", Connected: true}, hub.Stats{}, ffmpegView{}, now)
	if want := p.S(i18n.UINone); quiet.LastError != want {
		t.Errorf("last error = %q, want %q", quiet.LastError, want)
	}
}

// ffmpeg の 1 行は、UVC カメラが映らないという問い合わせで最初に見る場所。
func TestDescribeFFmpeg(t *testing.T) {
	p := i18n.NewPrinter(i18n.English)

	for _, tc := range []struct {
		name  string
		state ffmpegfetch.State
		want  string
	}{
		{"installed", ffmpegfetch.State{Installed: true, Supported: true, Path: `C:\bin\ffmpeg.exe`}, `C:\bin\ffmpeg.exe`},
		{"downloading", ffmpegfetch.State{Supported: true, Downloading: true, Received: 50, Total: 200}, "25%"},
		// 進捗の分母が未知でも、割り算で落ちてはいけない。
		{"downloading, size unknown", ffmpegfetch.State{Supported: true, Downloading: true}, "0%"},
		{"missing", ffmpegfetch.State{Supported: true}, p.S(i18n.UIFFmpegMissing)},
		{"failed", ffmpegfetch.State{Supported: true, LastError: "checksum mismatch"}, "checksum mismatch"},
		{"unsupported", ffmpegfetch.State{}, p.S(i18n.UIFFmpegUnsupported)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeFFmpeg(p, tc.state); !strings.Contains(got, tc.want) {
				t.Errorf("describeFFmpeg = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// 時間は言葉を使わずに書く。この画面でユーザーの言語に触れずに済む唯一の形。
func TestFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0:00"},
		{9 * time.Second, "0:09"},
		{90 * time.Second, "1:30"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1:02:03"},
		{49 * time.Hour, "2d 01:00:00"},
		// 時計が戻ることはある。負の経過時間を出すよりゼロを出す。
		{-time.Second, "0:00"},
	} {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0, "0 B"},
		{-1, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KB"},
		{3 * 1024 * 1024, "3.0 MB"},
	} {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
