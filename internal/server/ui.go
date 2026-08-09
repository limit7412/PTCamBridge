package server

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/i18n"
	"github.com/limit7412/PTCamBridge/internal/status"
)

// 診断画面。
//
// ブリッジが今どう見えているかを 1 枚にまとめて見せます。ソースの状態、フレームの
// 流れ、映像そのもの、繋がっているデバイス、ffmpeg の有無、そして設定ファイルと
// ログの場所です。書き込みは 1 つもありません。
//
// 既に /stats があるのになぜ、という問いへの答えは読み手の違いです。/stats は
// プログラムが読むもので、英語で、生の数値を返します。この画面が相手にしているのは
// 「映らない」と言っているユーザーであり、必要なのは JSON ではなく、自分の言語で
// 書かれた状態と、目で見える映像です。
//
// 画面はループバック待受のときだけ現れます。管理 API と同じ扱いにするのは、これが
// 認証を持たないまま設定ファイルの場所やデバイス名を映すからです。
//
// 面は 2 つに分かれます。/ui が骨格 (文言とレイアウト) で、/ui/state が中身です。
// 表示の判断 — 状態をどの言葉で呼ぶか、秒をどう並べるか、エラーを翻訳するか —
// はすべて Go 側にあり、ブラウザは受け取った文字列を置くだけです。そうしたのは、
// テストできる場所に判断を集めるためです。
//
//go:embed ui.html
var uiHTML string

// 設定画面。診断画面と対になります。あちらが「今どうなっているか」なら、こちらは
// 「どうしたいか」です。
//
// 書き込みは 1 本の道しかありません。この画面は保存の直前に /api/v1/config を
// GET し、ユーザーが触った項目だけをそこへ重ねて、設定全体を PUT で送り返します。
// 管理 API が設定の一部ではなく全体を受け取るからで、取り直した設定に重ねる形に
// しないと 2 つのものを壊します。画面に出していない項目 — シリアルのヘッダ定数、
// 追加ヘッダー、フレーム上限 — が既定値へ戻ることと、画面を開いた後にトレイや別の
// クライアントが変えたものを、こちらが見ていた古い値で押し戻すことです。
//
//go:embed ui_settings.html
var uiSettingsHTML string

// スタイルは 2 つの画面が共有します。テンプレートとして持つのは、埋め込んだ文字列を
// そのまま両方の <style> に流し込むためです。
//
//go:embed ui.css
var uiCSS string

var uiTemplates = func() *template.Template {
	set := template.Must(template.New("css").Parse(uiCSS))
	template.Must(set.New("ui").Parse(uiHTML))
	template.Must(set.New("settings").Parse(uiSettingsHTML))
	return set
}()

// uiPage は、骨格を組み立てるのに必要なものです。読み込み時に 1 度だけ描かれ、
// 以降変わりません。
type uiPage struct {
	Lang     string
	Title    string
	Text     map[string]string
	TextJSON template.JS
	Address  string
	Version  string
	Settings string
	LogDir   string
}

// uiState は、画面が polling で受け取る中身です。すべて表示するとおりの文字列で、
// 数値のまま出せるものだけが数値です。
type uiState struct {
	State      string `json:"state"`
	Source     string `json:"source"`
	Uptime     string `json:"uptime"`
	Reconnects uint64 `json:"reconnects"`
	LastError  string `json:"last_error"`
	InputFPS   string `json:"input_fps"`
	Clients    int    `json:"clients"`
	Published  uint64 `json:"published"`
	Dropped    uint64 `json:"dropped"`
	FrameSize  string `json:"frame_size"`
	LastFrame  string `json:"last_frame"`
	HasFrame   bool   `json:"has_frame"`
	FFmpeg     string `json:"ffmpeg"`
	HasFFmpeg  bool   `json:"has_ffmpeg"`
	// FFmpegPrompt は、取得を始める前にユーザーへ見せなければならない内容です。
	// 配布元、URL、サイズ、ライセンス。取得できる状態のときだけ入ります。
	FFmpegPrompt string `json:"ffmpeg_prompt,omitempty"`
	// Overridden は、起動時の指定が優先されるため、設定ファイルに何を書いても
	// 変わらない設定の名前です。設定画面がそう伝えるために読みます。
	Overridden []string `json:"overridden,omitempty"`
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ui" && r.URL.Path != "/ui/" {
		// guardAdmin より先に見ます。/ui/ で受けているのは配下すべてなので、
		// 綴りを間違えた URL に「拒否」と答えるのは、無いものを在ると言うのと
		// 同じことになります。
		http.NotFound(w, r)
		return
	}
	s.renderUI(w, r, "ui", s.printer().S(i18n.UITitle))
}

func (s *Server) handleUISettings(w http.ResponseWriter, r *http.Request) {
	s.renderUI(w, r, "settings", s.printer().S(i18n.UISettingsTitle))
}

// renderUI は、名前付きのテンプレートを骨格として描きます。
func (s *Server) renderUI(w http.ResponseWriter, r *http.Request, name, title string) {
	if !s.guardAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	p := s.printer()
	text := uiText(p)
	// json.Marshal は既定で <、>、& を < などに変える。script の中に置く
	// 文字列としてはこれで足り、閉じタグに見えるものを含んでいても壊れない。
	encoded, err := json.Marshal(text)
	if err != nil {
		http.Error(w, "encode page text: "+err.Error(), http.StatusInternalServerError)
		return
	}

	page := uiPage{
		Lang:     string(p.Lang()),
		Title:    title,
		Text:     text,
		TextJSON: template.JS(encoded),
		Address:  r.Host,
		Version:  s.opts.Version,
		Settings: s.opts.ConfigPath,
		LogDir:   s.opts.LogDir,
	}
	// 描き始めた後に失敗すると応答が半分になるので、いったん貯めてから出します。
	var body bytes.Buffer
	if err := uiTemplates.ExecuteTemplate(&body, name, page); err != nil {
		http.Error(w, "render page: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// 画面はこのプロセスの中だけで完結します。外部への参照が 1 つも無いことを
	// 宣言しておくのは、ここが認証を持たないまま設定ファイルの場所やデバイス名を
	// 映すからです。
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

func (s *Server) handleUIState(w http.ResponseWriter, r *http.Request) {
	if !s.guardAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ffmpeg ffmpegView
	if s.opts.FFmpeg != nil {
		ffmpeg = ffmpegView{state: s.opts.FFmpeg.State(), present: true}
	}
	state := newUIState(
		s.printer(),
		s.opts.Status.Snapshot(),
		s.opts.Hub.Stats(),
		ffmpeg,
		time.Now(),
	)
	if s.opts.Controller != nil {
		state.Overridden = s.opts.Controller.Overridden()
	}
	writeJSON(w, r, http.StatusOK, state)
}

// ffmpegView は、fetcher が組み込まれていない場合と、組み込まれていて何も
// 導入されていない場合を、画面が取り違えないようにするためのものです。前者は
// 行そのものが出ません。
type ffmpegView struct {
	state   ffmpegfetch.State
	present bool
}

// describeFFmpeg は、ffmpeg について今言えることを 1 行にします。
//
// 導入済みならその場所を添えます。UVC カメラが映らないという問い合わせで最初に
// 確かめるのがここで、次に確かめるのが「どの ffmpeg を使っているのか」だからです。
func describeFFmpeg(p i18n.Printer, state ffmpegfetch.State) string {
	switch {
	case state.Downloading:
		percent := 0
		if state.Total > 0 {
			percent = int(state.Received * 100 / state.Total)
		}
		return p.F(i18n.UIFFmpegDownloading, percent)
	case state.Installed:
		if state.Path != "" {
			return p.S(i18n.UIFFmpegInstalled) + " — " + state.Path
		}
		return p.S(i18n.UIFFmpegInstalled)
	case !state.Supported:
		return p.S(i18n.UIFFmpegUnsupported)
	case state.LastError != "":
		// 直前の試行が残した文言は機械のものなので、そのまま出します。
		return p.S(i18n.UIFFmpegMissing) + " — " + state.LastError
	default:
		return p.S(i18n.UIFFmpegMissing)
	}
}

// newUIState は、状態と統計を画面に出す形へ変換します。
//
// 表示の判断がここに集まっているのは、ブラウザの JavaScript には単体テストの
// 手段が無く、こちらにはあるからです。
func newUIState(p i18n.Printer, snapshot status.Snapshot, frames hub.Stats, ffmpeg ffmpegView, now time.Time) uiState {
	source := snapshot.Source
	if source == "" {
		source = p.S(i18n.UIStateNoSource)
	}

	// トレイの 1 行表示と同じ判定です。2 つの画面が同じ瞬間に違うことを言うのは、
	// どちらを信じればよいのか分からないという点で、何も表示しないより悪いことです。
	var state string
	switch {
	case snapshot.Source == "":
		state = p.S(i18n.UIStateNoSource)
	case snapshot.Paused:
		state = p.S(i18n.UIStatePaused)
	case !snapshot.Connected && snapshot.LastError != "":
		state = p.S(i18n.UIStateReconnecting)
	case !snapshot.Connected:
		state = p.S(i18n.UIStateConnecting)
	default:
		state = p.S(i18n.UIStateRunning)
	}

	out := uiState{
		State:      state,
		Source:     source,
		Uptime:     formatDuration(time.Duration(snapshot.UptimeSeconds * float64(time.Second))),
		Reconnects: snapshot.Reconnects,
		LastError:  p.Reported(snapshot.LastErrorKey, snapshot.LastError),
		InputFPS:   fmt.Sprintf("%.1f", frames.InputFPS),
		Clients:    frames.Subscribers,
		Published:  frames.Published,
		Dropped:    frames.Dropped,
		FrameSize:  formatBytes(frames.LastFrameSize),
		LastFrame:  p.S(i18n.UINone),
		HasFrame:   !frames.LastFrameAt.IsZero(),
	}
	if out.LastError == "" {
		out.LastError = p.S(i18n.UINone)
	}
	if out.HasFrame {
		out.LastFrame = p.F(i18n.UIAgo, formatDuration(now.Sub(frames.LastFrameAt)))
	}
	if ffmpeg.present {
		out.HasFFmpeg = true
		out.FFmpeg = describeFFmpeg(p, ffmpeg.state)
		// 同意を求める文面は、これから取得できるときにだけ意味があります。
		// 導入済みや取得中に出すと、押していないボタンの説明になります。
		if ffmpeg.state.Supported && !ffmpeg.state.Installed && !ffmpeg.state.Downloading {
			out.FFmpegPrompt = ffmpegfetch.Prompt(p, ffmpeg.state.Source)
		}
	}
	return out
}

// formatDuration は、経過時間を言葉を使わずに書きます。
//
// 「3 分」ではなく "03:12" なのは、これがユーザーの言語に触れずに済む唯一の形だから
// です。この画面で時間の刻みが出る場所は 2 つあり、片方は稼働時間、もう片方は
// 最後にフレームが届いてからの間です。後者は 1 秒ごとに動きます。
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d.Seconds())
	days := total / 86400
	hours := (total % 86400) / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %02d:%02d:%02d", days, hours, minutes, seconds)
	case hours > 0:
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	default:
		return fmt.Sprintf("%d:%02d", minutes, seconds)
	}
}

// formatBytes は、フレームの大きさを読みやすい単位で書きます。単位は翻訳しません。
// KB と MB はどちらの言語でもそう書くからです。
func formatBytes(n int) string {
	switch {
	case n <= 0:
		return "0 B"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// printer は、この画面が話す言語です。設定の言語を変えるとブリッジは再起動するので、
// 起動時に受け取った 1 つで足ります。
func (s *Server) printer() i18n.Printer { return s.opts.Printer }

// uiText は、骨格に埋め込む文言です。map にしているのは、同じものを HTML と
// JavaScript の両方が使うからです。片方だけ翻訳された画面を作らずに済みます。
func uiText(p i18n.Printer) map[string]string {
	keys := map[string]i18n.Key{
		"title":       i18n.UITitle,
		"status":      i18n.UIStatus,
		"preview":     i18n.UIPreview,
		"frames":      i18n.UIFrames,
		"devices":     i18n.UIDevices,
		"ffmpeg":      i18n.UIFFmpeg,
		"places":      i18n.UIPlaces,
		"unreachable": i18n.UIUnreachable,

		"source":      i18n.UIFieldSource,
		"state":       i18n.UIFieldState,
		"uptime":      i18n.UIFieldUptime,
		"reconnects":  i18n.UIFieldReconnects,
		"lastError":   i18n.UIFieldLastError,
		"inputFPS":    i18n.UIFieldInputFPS,
		"clients":     i18n.UIFieldClients,
		"published":   i18n.UIFieldPublished,
		"dropped":     i18n.UIFieldDropped,
		"frameSize":   i18n.UIFieldFrameSize,
		"lastFrame":   i18n.UIFieldLastFrame,
		"address":     i18n.UIFieldAddress,
		"settings":    i18n.UIFieldSettings,
		"logs":        i18n.UIFieldLogs,
		"version":     i18n.UIFieldVersion,
		"cameras":     i18n.UIFieldCameras,
		"serialPorts": i18n.UIFieldSerialPorts,

		"none":        i18n.UINone,
		"waiting":     i18n.UIWaitingForFrame,
		"previewNote": i18n.UIPreviewNote,

		"navStatus":   i18n.UINavStatus,
		"navSettings": i18n.UINavSettings,

		"transform":    i18n.UITransform,
		"stream":       i18n.UISettingsStream,
		"papertracker": i18n.UIPaperTracker,
		"display":      i18n.UISettingsDisplay,
		"log":          i18n.UISettingsLog,

		"sourceUVC":    i18n.MenuSourceUVC,
		"sourceSerial": i18n.MenuSourceSerial,
		"sourceMJPEG":  i18n.MenuSourceMJPEG,

		"device":     i18n.UIFieldDevice,
		"size":       i18n.UIFieldSize,
		"framerate":  i18n.UIFieldFramerate,
		"ffmpegPath": i18n.UIFieldFFmpegPath,
		"port":       i18n.UIFieldPort,
		"baud":       i18n.UIFieldBaud,
		"url":        i18n.UIFieldURL,
		"rotate":     i18n.UIFieldRotate,
		"flipH":      i18n.UIFieldFlipH,
		"flipV":      i18n.UIFieldFlipV,
		"cropSquare": i18n.UIFieldCropSquare,
		"quality":    i18n.UIFieldQuality,
		"hold":       i18n.UIFieldHold,
		"boundary":   i18n.UIFieldBoundary,
		"listen":     i18n.UIFieldListen,
		"installDir": i18n.UIFieldInstallDir,
		"writeCache": i18n.UIFieldWriteCache,
		"language":   i18n.UIFieldLanguage,
		"logLevel":   i18n.UIFieldLogLevel,
		"logDir":     i18n.UIFieldLogDir,

		"langAuto":       i18n.UILangAuto,
		"qualityHint":    i18n.UIQualityHint,
		"framerateHint":  i18n.UIFramerateHint,
		"sizeHint":       i18n.UISizeHint,
		"modesFound":     i18n.UIModesFound,
		"modesLooking":   i18n.UIModesLooking,
		"modesNoSize":    i18n.UIModesNoSize,
		"modesNoRate":    i18n.UIModesNoRate,
		"modesUnknown":   i18n.UIModesUnknown,
		"writeCacheHint": i18n.UIWriteCacheHint,
		"restartBadge":   i18n.UIRestartBadge,
		"restartNote":    i18n.UIRestartNote,
		"overriddenNote": i18n.UIOverriddenNote,
		"save":           i18n.UISave,
		"saving":         i18n.UISaving,
		"saved":          i18n.UISaved,
		"savedPending":   i18n.UISavedPending,
		"saveFailed":     i18n.UISaveFailed,
		"conflict":       i18n.UIConflict,
		"conflictElse":   i18n.UIConflictElse,
		"conflictAsk":    i18n.UIConflictAsk,
		"conflictKept":   i18n.UIConflictKept,
		"getFFmpeg":      i18n.UIGetFFmpeg,
	}
	text := make(map[string]string, len(keys))
	for name, key := range keys {
		text[name] = p.S(key)
	}
	return text
}
