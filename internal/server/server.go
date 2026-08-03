// Package server は、中継したカメラを PaperTracker クライアントが期待する
// MJPEG-over-HTTP ストリームとして公開します。状態エンドポイントと管理
// エンドポイントも併せて提供します。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/i18n"
	"github.com/limit7412/PTCamBridge/internal/source"
	"github.com/limit7412/PTCamBridge/internal/status"
)

// sourceLossTimeout は、ソースを失われたとみなすまでにストリームがフレームを
// 出さずにいられる時間です。/healthz の判定にも使います。
const sourceLossTimeout = 2 * time.Second

// readHeaderTimeout は、リクエスト行とヘッダーに対する上限です。応答本体に上限を
// 設けていないのは意図的です。終わりのないストリームだからです。
const readHeaderTimeout = 10 * time.Second

// streamBufferSize は、クライアントごとのエンコードバッファをあらかじめ確保する
// 大きさです。
const streamBufferSize = 64 << 10

// writeTimeout は、1 クライアントへのフレーム 1 枚の書き込みに対する上限です。
//
// 応答全体は終わりがないのでサーバに WriteTimeout はありません。そのため、読むのを
// やめたクライアントは、ソケットバッファが埋まった時点で書き込みを永遠にブロック
// できてしまいます。ブロックした書き込みは、監視にもキャンセルされたコンテキストにも
// 届きません。これだけ待っても渡せないフレームは、接続が閉じられているかどうかに
// 関わらず、そのクライアントが居なくなったことを意味します。
const writeTimeout = 5 * time.Second

// Devices は、ソース選択のために管理 API が報告する内容です。
//
// 2 つのリストは独立に集め、それぞれが自分のエラーを持ちます。失敗の仕方が独立して
// いるからです。ffmpeg の無い機械はカメラを列挙できませんが、シリアルポートの列挙は
// 何の問題もなくできます。どちらかの失敗でリクエスト全体を失敗させると、ユーザーが
// 実際に持っているデバイスを隠すことになります。
//
// エラーの無い空のリストは「何も繋がっていない」を意味します。エラーを伴う空の
// リストは「誰にも分からない」を意味します。選択画面は、その違いを飲み込まずに
// 見せなければなりません。
type Devices struct {
	Cameras     []source.Device     `json:"cameras"`
	CameraError string              `json:"camera_error,omitempty"`
	SerialPorts []source.SerialPort `json:"serial_ports"`
	SerialError string              `json:"serial_error,omitempty"`
}

// Controller は、ソースがどう起動されるかを server パッケージに知らせないまま、
// 管理 API がブリッジを操作できるようにします。
type Controller interface {
	// Snapshot は、現在有効な設定を返します。
	Snapshot() config.Config
	// Apply は新しい設定を検証し、採用します。起動時にしか読まれない設定も
	// 受け入れますが、この起動の振る舞いは変わりません。その名前が返ります。
	Apply(ctx context.Context, cfg config.Config) ([]string, error)
	// Switch は、稼働中のソース種別を変更します。
	Switch(ctx context.Context, sourceType string) error
	// Devices は、今使えるカメラとシリアルポートを列挙します。問題が起きた場合は、
	// 1 つのエラーにまとめず、リストごとに報告します。
	Devices(ctx context.Context) Devices
}

// FFmpegFetcher は、管理 API が操作する ffmpeg ダウンロードの一部です。
//
// 型そのものではなくインターフェースにしているのは、サーバがダウンロードの仕組みを
// 何も知らずに済むようにするため、そして fetcher を組み込まないビルドでは単に
// エンドポイントが無くなるようにするためです。
type FFmpegFetcher interface {
	// State は、何が導入済みで何が進行中かを報告します。
	State() ffmpegfetch.State
	// Start はダウンロードを開始するか、開始できない理由を返します。ダウンロードが
	// 走り出した時点で戻ります。100 メガバイトは HTTP リクエストの中に収まらないので、
	// 呼び出し側は代わりに State を polling します。
	Start() error
}

// Options は Server を設定します。
type Options struct {
	Hub    *hub.Hub
	Status *status.Tracker
	// Encoder は初期のワイヤ形式です。SetStreamOptions が差し替えます。
	Encoder core.MultipartEncoder
	Logger  *slog.Logger
	// Controller は管理 API を有効にします。EnableAdmin も立っていなければ
	// 無視されます。
	Controller Controller
	// EnableAdmin は /api/v1/* を提供します。listen 先がループバックでない場合、
	// ブリッジはこれを切ります。この API には認証が無いからです。
	EnableAdmin bool
	// FFmpeg は /api/v1/ffmpeg を有効にします。nil ならエンドポイントは無くなり、
	// 公式ビルドの無いプラットフォームはそうなります。
	FFmpeg FFmpegFetcher
	// HoldOnSourceLoss は、カメラの再接続中に応答を閉じず、ストリームの
	// クライアントを繋いだままにします。これは初期値で、SetStreamOptions が
	// 差し替えます。
	HoldOnSourceLoss bool
	Version          string
	// Printer は診断画面の文言を組み立てます。ゼロ値なら英語になります。
	// /stats と /api/v1/* はこれを使いません。あちらはプログラムが読むものです。
	Printer i18n.Printer
	// ConfigPath と LogDir は診断画面が場所として表示します。空なら出しません。
	// サーバがこれらを持つのは表示のためだけで、読み書きはしません。
	ConfigPath string
	LogDir     string
}

// streamOptions は、動作中のサーバが差し替えられる応答設定です。
type streamOptions struct {
	encoder core.MultipartEncoder
	hold    bool
}

// Server は、ストリームと状態のエンドポイントを提供します。
type Server struct {
	opts Options
	log  *slog.Logger

	// stream は設定変更時に丸ごと差し替えます。既に接続しているクライアントは、
	// 解析を始めたときのワイヤ形式を保ちます。
	stream atomic.Pointer[streamOptions]
}

// New はサーバを組み立てます。Hub・Status・Logger は必須です。
func New(opts Options) (*Server, error) {
	switch {
	case opts.Hub == nil:
		return nil, errors.New("server: hub is required")
	case opts.Status == nil:
		return nil, errors.New("server: status tracker is required")
	case opts.Logger == nil:
		return nil, errors.New("server: logger is required")
	}
	s := &Server{opts: opts, log: opts.Logger}
	s.stream.Store(&streamOptions{encoder: opts.Encoder, hold: opts.HoldOnSourceLoss})
	return s, nil
}

// SetStreamOptions は、これ以降に始まるストリームに新しいワイヤ形式を適用します。
// ブリッジは設定変更の後にこれを呼びます。boundary や追加ヘッダー — PaperTracker の
// リリースに合わせるためだけに存在するつまみ — の調整が、再起動なしで効くように
// するためです。
func (s *Server) SetStreamOptions(encoder core.MultipartEncoder, holdOnSourceLoss bool) {
	s.stream.Store(&streamOptions{encoder: encoder, hold: holdOnSourceLoss})
	s.log.Info("stream settings updated", "boundary", encoder.Boundary(), "hold_on_source_loss", holdOnSourceLoss)
}

// Handler は、経路を設定したハンドラを返します。
//
// ストリームを "/stream" だけでなく "/" でも提供するのは、PaperTracker クライアントが
// キャッシュした裸のアドレスをパス無しで要求するからです。この互換のための面は、
// まさにそのために存在します。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/stream", s.handleStream)
	mux.HandleFunc("/snapshot", s.handleSnapshot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)

	if s.opts.EnableAdmin && s.opts.Controller != nil {
		// 診断画面は管理 API と同じ扱いです。認証を持たないまま、設定ファイルの
		// 場所と繋がっているデバイスの名前を映すからです。
		mux.HandleFunc("/ui", s.handleUI)
		mux.HandleFunc("/ui/", s.handleUI)
		mux.HandleFunc("/ui/state", s.handleUIState)
		mux.HandleFunc("/ui/settings", s.handleUISettings)
		mux.HandleFunc("/api/v1/config", s.handleConfig)
		mux.HandleFunc("/api/v1/source", s.handleSourceSwitch)
		mux.HandleFunc("/api/v1/devices", s.handleDevices)
		if s.opts.FFmpeg != nil {
			mux.HandleFunc("/api/v1/ffmpeg", s.handleFFmpeg)
		}
	}
	return mux
}

// Listen はアドレスを bind します。bind を提供と分けているのは、呼び出し側が実際の
// アドレスを知れるようにするため (ポート 0 はここで解決されます)、そして bind の失敗を
// 「既に別の実体が動いている」と解釈できるようにするためです。
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// Serve は ctx がキャンセルされるまで動き、その後穏当に停止します。ストリームの
// クライアントは接続を永遠に開いたままにするので、停止処理は終わることのない応答を
// 待つのではなく、それらを閉じます。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
		}
		return <-errCh
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.handleStream(w, r)
}

// handleStream は、クライアントが接続している限り、フレームを
// multipart/x-mixed-replace として書き続けます。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// 設定は一度だけ読む。Content-Type で名乗った boundary は、この応答の
	// すべてのパートがその後使うものと同じでなければならない。
	stream := s.stream.Load()

	header := w.Header()
	header.Set("Content-Type", stream.encoder.ContentType())
	header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("Expires", "0")
	// identity エンコーディングを宣言することが、net/http が本体をチャンクで
	// 包むのを止めている。クライアントはこちらの boundary と Content-Length を
	// 探しながらソケットを読むので、multipart の枠組みの間にチャンク長の行が
	// 挟まればパーサーの同期が外れる。
	header.Set("Transfer-Encoding", "identity")
	header.Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if r.Method == http.MethodHead {
		return
	}

	frames, unsubscribe := s.opts.Hub.Subscribe()
	defer unsubscribe()

	// 読むのをやめたクライアントへの書き込みを解除できるのは、フレームごとの
	// 期限だけ。すべての ResponseWriter が対応しているわけではなく、対応して
	// いない場合の挙動は従来どおり。
	rc := http.NewResponseController(w)
	writeFrame := func(b []byte) error {
		if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		_, err := w.Write(b)
		return err
	}

	ctx := r.Context()
	buf := make([]byte, 0, streamBufferSize)
	lastFrame := time.Now()

	// 今あるものをすぐ送る。再接続したクライアントが、次のキャプチャを待たずに
	// 画像を見られるようにするため。ただし喪失タイムアウトより古いフレームは
	// 送らない。hub は最後の画像を無期限に保持するので、カメラが落ちている間の
	// 再接続すべてにそれを流すと、トラッカーに古い口の形を何度も食わせることになる。
	//
	// ここで送ったものの連番は控えておく。購読と最新フレームの読み取りは 2 段階で
	// あり、その間に配信されたフレームはこのクライアントのキューに入ると同時に
	// 最新にもなる。これが無いと、ストリームは同じ画像を 2 回続けて送ることで
	// 始まり、トラッカーは同じ口の形を 2 つの標本として見ることになる。
	var sent uint64
	if latest, ok := s.opts.Hub.Latest(); ok && time.Since(latest.RecvedAt) <= sourceLossTimeout {
		buf = stream.encoder.AppendPart(buf[:0], latest.Data)
		if err := writeFrame(buf); err != nil {
			return
		}
		flusher.Flush()
		sent = latest.Seq
	}

	// この ticker は、出力を止めたソースに気づくためだけにある。ストリームの
	// 間隔を刻んでいるのではない。
	watchdog := time.NewTicker(sourceLossTimeout / 2)
	defer watchdog.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case frame, open := <-frames:
			if !open {
				return
			}
			if frame.Seq != 0 && frame.Seq <= sent {
				// 購読の直後に上で送信済み。
				continue
			}
			buf = stream.encoder.AppendPart(buf[:0], frame.Data)
			if err := writeFrame(buf); err != nil {
				s.log.Debug("stream client went away", "remote", r.RemoteAddr, "error", err)
				return
			}
			flusher.Flush()
			lastFrame = time.Now()

		case <-watchdog.C:
			if stream.hold || time.Since(lastFrame) <= sourceLossTimeout {
				continue
			}
			// 閉じるとクライアントは再接続する。代わりに接続を開いたまま保つと、
			// 短い断絶からの復帰は速い。だからこの挙動は設定可能にしてある。
			s.log.Debug("closing stream after source loss", "remote", r.RemoteAddr)
			return
		}
	}
}

// handleSnapshot は、直近のフレームをそのままの JPEG として返します。MJPEG を
// 扱えるクライアントが無くても調査できるようにするためです。
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	frame, ok := s.opts.Hub.Latest()
	if !ok {
		http.Error(w, "no frame captured yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(frame.Size()))
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(frame.Data)
}

// Health は /healthz の応答本体です。
type Health struct {
	OK     bool            `json:"ok"`
	Reason string          `json:"reason,omitempty"`
	Source status.Snapshot `json:"source"`
}

// handleHealth は、フレームが届いている間は 200 を、そうでなければ 503 を返します。
// 監視プロセスやトレイが「動いている」と「機能している」を区別できるようにするため
// です。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snapshot := s.opts.Status.Snapshot()
	stats := s.opts.Hub.Stats()

	health := Health{OK: true, Source: snapshot}
	switch {
	case snapshot.Paused:
		health.OK, health.Reason = false, "capture is paused"
	case !snapshot.Connected:
		health.OK, health.Reason = false, "source is not connected"
	case stats.LastFrameAt.IsZero():
		health.OK, health.Reason = false, "no frame captured yet"
	case time.Since(stats.LastFrameAt) > sourceLossTimeout:
		health.OK, health.Reason = false, fmt.Sprintf("no frame for %s", time.Since(stats.LastFrameAt).Round(time.Millisecond))
	}

	code := http.StatusOK
	if !health.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, r, code, health)
}

// Stats は /stats の応答本体です。
type Stats struct {
	Version string          `json:"version"`
	Frames  hub.Stats       `json:"frames"`
	Source  status.Snapshot `json:"source"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, Stats{
		Version: s.opts.Version,
		Frames:  s.opts.Hub.Stats(),
		Source:  s.opts.Status.Snapshot(),
	})
}

// guardAdmin は、Web ページがブラウザに送らせた可能性のある管理 API リクエストを
// 拒否します。
//
// ループバックへの bind はそれ自体では防御になりません。ユーザーが訪れたどのページも
// 127.0.0.1 に到達できますし、フォーム形式の POST はそのために preflight を必要と
// しません。この API に到達できること自体がその権限のすべてなので、外部のページから
// 来たものではないと示せないリクエストに、ソースを変更させるわけにはいきません。
func (s *Server) guardAdmin(w http.ResponseWriter, r *http.Request) bool {
	// 再バインドされた DNS 名はループバックに解決されるが、Host には自身の名前が
	// 載ったまま。それが bind アドレスに再び意味を与えている。
	if !isLoopbackHost(r.Host) {
		http.Error(w, "the management API only answers requests addressed to loopback", http.StatusForbidden)
		return false
	}
	// ブラウザはクロスサイトのリクエストすべてに Origin を付ける。コマンドライン
	// のクライアントはまったく送らないので、ヘッダーが無い場合は通す。
	if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
		http.Error(w, "cross-origin requests are not accepted", http.StatusForbidden)
		return false
	}
	// GET に対して Origin だけでは足りない。ページはこの URL をサブリソースとして
	// 要求できる — <img src>、<script src> — が、ブラウザはそれらに Origin を
	// 付けないので、立ちはだかるのは Host の検査だけになる。応答は読めないものの、
	// GET /devices はただではない。Windows では ffmpeg を起動して最大 15 秒待つので、
	// URL を次々に叩くページは、その機械でプロセスを生み出し続けられる。
	//
	// Sec-Fetch-Site はリクエストの出所を述べるもので、ページからは設定できない。
	// 送ってくるブラウザにはそれを守らせる。送ってこないものはブラウザではないし、
	// どのページもブラウザにそれを送るのをやめさせられない。
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "the management API does not answer requests made by another site", http.StatusForbidden)
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	// text/plain、フォーム、multipart は、ページが preflight 無しに POST できる
	// 本体の種類。JSON を要求することが preflight を強制し、それを上の Origin の
	// 検査が失敗させる。
	if !hasJSONBody(r) {
		http.Error(w, "Content-Type: application/json is required", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// isLoopbackHost は、authority がこの機械を指しているかを返します。ポートは
// 関係なく、無くても構いません。
func isLoopbackHost(authority string) bool {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = authority
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// isLoopbackOrigin は、Origin ヘッダーの値が、この機械から提供されたページを
// 指しているかを返します。
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Host)
}

// hasJSONBody は、リクエストが JSON の本体を宣言しているかを返します。
func hasJSONBody(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !s.guardAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, r, http.StatusOK, s.opts.Controller.Snapshot())

	case http.MethodPut:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		// 受け取る型は応答と同じものです。設定として知らない項目は今までどおり
		// 撥ねますが、pending_restart だけは通します。これはこちらが応答に載せて
		// いるもので、受け取ったものをそのまま送り返す呼び出し側 — この画面が
		// まさにそうしかけました — が、自分では付けていない項目のせいで 400 を
		// 受け取るのは、往復として筋が通りません。値は読みません。何が次の起動を
		// 待っているかを決めるのは要求ではなく、ブリッジだからです。
		var received appliedConfig
		if err := decodeStrict(bytes.NewReader(body), &received); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		cfg := received.Config
		carryUnmentioned(&cfg, body, s.opts.Controller.Snapshot())
		cfg.Normalise()
		if err := cfg.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		deferred, err := s.opts.Controller.Apply(r.Context(), cfg)
		if err != nil {
			http.Error(w, err.Error(), applyStatus(err))
			return
		}
		writeJSON(w, r, http.StatusOK, appliedConfig{
			Config:         s.opts.Controller.Snapshot(),
			PendingRestart: deferred,
		})

	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSourceSwitch(w http.ResponseWriter, r *http.Request) {
	if !s.guardAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Type string `json:"type"`
	}
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, 1<<16), &body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// {} という本体は空の type として問題なくデコードされるが、その先で空は
	// 「値が無い」を意味しない。Normalise はそれを「未設定」と読んで既定値を
	// 埋めるので、type をまったく含まないリクエストが、動いている serial や
	// MJPEG のソースを黙って UVC へ移してしまう。空白を落とすのは Normalise も
	// そうするから。"   " も同じ既定値に行き着く。
	if strings.TrimSpace(body.Type) == "" {
		http.Error(w, `"type" is required`, http.StatusBadRequest)
		return
	}
	if err := s.opts.Controller.Switch(r.Context(), body.Type); err != nil {
		http.Error(w, err.Error(), applyStatus(err))
		return
	}
	writeJSON(w, r, http.StatusOK, s.opts.Controller.Snapshot())
}

// applyStatus は、設定の失敗をステータスコードに対応付けます。反映はされたが
// ディスクに書けなかった変更だけが「リクエストは正しく、こちら側が失敗した」場合
// なので、400 にならないのはそれだけです。
// appliedConfig は PUT /api/v1/config の応答です。
//
// 埋め込みなので、設定の各項目は今までどおり最上位に並びます。増えるのは 1 つ、
// 保存はされたが、効くのは次の起動からという設定の名前です。値そのものは要求した
// とおりに返ります。名前がなければ、設定を変えたのに何も変わらない理由を呼び出し側が
// 自分で突き止めるしかありません。
//
// 何も保留にならなければ項目ごと出ません。空の配列は、読み手に「何かが保留になった」と
// 一瞬考えさせるからです。
type appliedConfig struct {
	config.Config
	// PendingRestart は、保存されたが次の起動まで効かない設定の TOML キーです。
	PendingRestart []string `json:"pending_restart,omitempty"`
}

func applyStatus(err error) int {
	if errors.Is(err, config.ErrNotSaved) {
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if !s.guardAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// エラーは返さない。半分の答えでも答えではあるし、どちらが失敗したかは
	// 本体の中で報告している。
	writeJSON(w, r, http.StatusOK, s.opts.Controller.Devices(r.Context()))
}

// handleFFmpeg は、ffmpeg が取得済みかどうかを報告し、取得を開始します。
//
// PUT ではなく POST で、本体はありません。これはリソースに値を設定するのではなく、
// 仕事に実行を頼むものだからです。本体が運ぶことになるはずのもの — どのアーカイブを、
// どこから — は、リクエストに選ばせないためにこそコード側で固定してあります。
func (s *Server) handleFFmpeg(w http.ResponseWriter, r *http.Request) {
	if !s.guardAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, r, http.StatusOK, s.opts.FFmpeg.State())

	case http.MethodPost:
		// 既に導入済みのダウンロードは繰り返さない。同じ 100 メガバイトをもう一度
		// 取ってくることは 2 回目のクリックの意味ではないし、動いている ffmpeg の
		// 置き換えは、迷い込んだリクエストでやることではない。
		state := s.opts.FFmpeg.State()
		if state.Installed {
			writeJSON(w, r, http.StatusOK, state)
			return
		}
		if err := s.opts.FFmpeg.Start(); err != nil {
			// 「実行中」はリクエストの失敗ではない。頼まれたことは起きている。
			// それ以外は、この機械ができないと言っているということ。
			if errors.Is(err, ffmpegfetch.ErrBusy) {
				writeJSON(w, r, http.StatusAccepted, s.opts.FFmpeg.State())
				return
			}
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, r, http.StatusAccepted, s.opts.FFmpeg.State())

	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// carryUnmentioned は、リクエストが名前を挙げなかった設定を現在値のまま保ちます。
// 対象は、その設定が存在する前に書かれたクライアントには送りようがないものです。
//
// PUT は設定全体を置き換えるので、本体から漏れたフィールドは Normalise を経て
// 既定値として戻ってきます。この API が想定する呼び出し側 — 設定を読み、1 つ変え、
// 全部送り返す — に対してはそれで正しく、古いスキーマに対して書かれた呼び出し側に
// 対しては誤りです。そちらは、書かれた当時に存在しなかったフィールドを送れません。
//
// ほとんどの設定ではこの違いは表に出ません。停止中にしか変更できない設定では表に
// 出ます。既定値へ落ちた結果が動作中の値と食い違うので、リクエスト全体が拒否され、
// そのクライアントは、自分が触れてもいない設定のせいで管理 API から締め出されます。
//
// 対象が ui だけなのは、この API に対して何かが書かれ得るようになって以降に追加された
// 設定がそれだけだからです。今後追加するものも、ここに属します。
func carryUnmentioned(cfg *config.Config, body []byte, current config.Config) {
	var mentioned struct {
		UI *json.RawMessage `json:"ui"`
	}
	if err := json.Unmarshal(body, &mentioned); err != nil {
		// デコードできない本体がここに届くことはない。厳格なデコードが先に走っている。
		return
	}
	if mentioned.UI == nil {
		cfg.UI = current.UI
	}
}

// decodeStrict は、対象が持たないフィールドを含む本体と、最初の JSON 値より後ろに
// 何かが続く本体を拒否します。
//
// 設定ファイルは既に未知のキーを拒否しています。ここに同じ規則が無ければ、API 越しに
// 綴りを誤ったフィールドは黙って捨てられ、それが設定するはずだった値はゼロ値のまま
// 残り、呼び出し側は「頼んだのとは違うことをした変更」に対して 200 を受け取ります。
//
// 末尾の検査は同じ主張を一段外側に広げたものです。{"type":"uvc"}{"type":"mjpeg"} と
// いう本体は、打ち間違いのあるリクエスト 1 つではなくリクエスト 2 つです。最初の
// 1 つだけをデコードして成功と報告することは、2 つ目も聞き入れたと呼び出し側に
// 告げることになります。
func decodeStrict(r io.Reader, target any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("unexpected content after the JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, r *http.Request, code int, body any) {
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data = append(data, '\n')

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
