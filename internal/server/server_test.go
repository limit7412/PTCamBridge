package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/source"
	"github.com/limit7412/PTCamBridge/internal/status"
)

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func newTestServer(t *testing.T, opts Options) (*Server, *hub.Hub, *status.Tracker) {
	t.Helper()
	if opts.Hub == nil {
		opts.Hub = hub.New()
	}
	if opts.Status == nil {
		opts.Status = status.New()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Encoder.Boundary() == "" {
		enc, err := core.NewMultipartEncoder("", nil)
		if err != nil {
			t.Fatalf("NewMultipartEncoder: %v", err)
		}
		opts.Encoder = enc
	}
	if opts.Version == "" {
		opts.Version = "test"
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, opts.Hub, opts.Status
}

// publishUntilDone は、テストの間フレームを流し続ける。
func publishUntilDone(t *testing.T, h *hub.Hub, jpg []byte) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				h.Publish(core.Frame{Data: jpg})
			}
		}
	}()
}

// クライアントは boundary と Content-Length を探しながらソケットを読む。チャンクの
// 枠組みはその構造の間に 16 進の長さ行を挟むので、ここでは net/http のクライアントが
// それを隠してくれることに頼らず、線上の生バイトを読む。
func TestStreamIsNotChunkedOnTheWire(t *testing.T) {
	s, h, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	jpg := testJPEG(t)
	publishUntilDone(t, h, jpg)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	reader := bufio.NewReader(conn)
	var headers []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		headers = append(headers, line)
	}

	joined := strings.Join(headers, "\n")
	if !strings.HasPrefix(headers[0], "HTTP/1.1 200") {
		t.Fatalf("status line = %q, want 200", headers[0])
	}
	if strings.Contains(strings.ToLower(joined), "transfer-encoding: chunked") {
		t.Errorf("the response is chunked, which the client cannot parse:\n%s", joined)
	}
	if !strings.Contains(joined, "Content-Type: multipart/x-mixed-replace; boundary=ptcambridge") {
		t.Errorf("Content-Type missing or wrong:\n%s", joined)
	}

	// 本体は区切りで始まらなければならない。チャンク長の行ではなく。
	body := make([]byte, len("--ptcambridge\r\n"))
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "--ptcambridge\r\n" {
		t.Errorf("body starts with %q, want the boundary delimiter", body)
	}
}

// パートの正確な並びが、クライアントとの互換性の契約そのもの。
func TestStreamPartLayout(t *testing.T) {
	s, h, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	jpg := testJPEG(t)
	publishUntilDone(t, h, jpg)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "GET /stream HTTP/1.1\r\nHost: localhost\r\n\r\n")

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	want := fmt.Sprintf("--ptcambridge\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpg))
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read part header: %v", err)
	}
	if string(got) != want {
		t.Fatalf("part header:\ngot:  %q\nwant: %q", got, want)
	}

	payload := make([]byte, len(jpg)+2)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("read part body: %v", err)
	}
	if !bytes.Equal(payload[:len(jpg)], jpg) {
		t.Error("the frame on the wire does not match the published frame")
	}
	if string(payload[len(jpg):]) != "\r\n" {
		t.Errorf("part body ends with %q, want CRLF", payload[len(jpg):])
	}
}

// 途中から接続したクライアントは、次のキャプチャを待たずに今のフレームをすぐ
// 見られるべき。
func TestStreamSendsTheLatestFrameImmediately(t *testing.T) {
	s, h, _ := newTestServer(t, Options{HoldOnSourceLoss: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	jpg := testJPEG(t)
	h.Publish(core.Frame{Data: jpg})

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if !bytes.HasPrefix(buf[:n], []byte("--ptcambridge\r\n")) {
		t.Errorf("first bytes = %q, want a part delimiter", buf[:n])
	}
}

// 保持を無効にしている場合、止まったソースはクライアントを解放し、再接続できる
// ようにしなければならない。
func TestStreamClosesAfterSourceLoss(t *testing.T) {
	s, h, _ := newTestServer(t, Options{HoldOnSourceLoss: false})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	h.Publish(core.Frame{Data: testJPEG(t)})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	start := time.Now()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil && ctx.Err() == nil {
		t.Fatalf("read stream: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the stream stayed open after the source stopped")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the stream took %s to close, expected roughly the loss timeout", elapsed)
	}
}

func TestSnapshot(t *testing.T) {
	s, h, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/snapshot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("before any frame: status = %d, want 503", resp.StatusCode)
	}

	jpg := testJPEG(t)
	h.Publish(core.Frame{Data: jpg})

	resp, err = http.Get(ts.URL + "/snapshot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, jpg) {
		t.Error("the snapshot does not match the published frame")
	}
}

func TestHealthz(t *testing.T) {
	s, h, tracker := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	get := func() (int, Health) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		var body Health
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.StatusCode, body
	}

	code, body := get()
	if code != http.StatusServiceUnavailable || body.OK {
		t.Errorf("disconnected: status %d ok=%v, want 503 and not ok", code, body.OK)
	}

	tracker.Connected("uvc")
	h.Publish(core.Frame{Data: testJPEG(t)})

	code, body = get()
	if code != http.StatusOK || !body.OK {
		t.Errorf("connected: status %d ok=%v reason=%q, want 200 and ok", code, body.OK, body.Reason)
	}

	tracker.SetPaused(true)
	code, body = get()
	if code != http.StatusServiceUnavailable || body.OK {
		t.Errorf("paused: status %d ok=%v, want 503 and not ok", code, body.OK)
	}
}

func TestStats(t *testing.T) {
	s, h, tracker := newTestServer(t, Options{Version: "1.2.3"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	tracker.Connected("serial")
	h.Publish(core.Frame{Data: testJPEG(t)})

	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	var body Stats
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", body.Version)
	}
	if body.Frames.Published != 1 {
		t.Errorf("Published = %d, want 1", body.Frames.Published)
	}
	if body.Source.Source != "serial" || !body.Source.Connected {
		t.Errorf("Source = %+v, want a connected serial source", body.Source)
	}
}

// fakeController は、管理 API が何を要求したかを記録する。
type fakeController struct {
	cfg      config.Config
	switched string
	applied  bool
	// applyErr は Apply が返す値。失敗の対応付けを見るためのもの。
	applyErr error
	// devices は Devices が返す値。
	devices Devices
	// devicesCalls は列挙の回数を数える。Windows では 1 回につき子プロセスが要る。
	devicesCalls int
	// deferred は Apply が「保存したが動作中には効いていない」と報告する設定名。
	deferred []string
	// overridden は、起動時の指定が優先される設定名。
	overridden []string
	// modes と modesErr は CameraModes が返す値。modesFor は最後に訊かれた
	// カメラ名で、modesCalls はその回数。1 回につきカメラを開くので、呼ばれ方
	// そのものが確認したい振る舞い。
	modes      []source.Mode
	modesErr   error
	modesFor   string
	modesCalls int
	// askedRevision は、最後に要求された条件 (If-Match が並べた札)。
	askedRevision []string
	// next は、Apply の後に読まれる設定。別のクライアントの変更が割り込んだ
	// 状況を作るためのもの。
	next *config.Config
	// drifting と drifts は、Snapshot が Apply の見る設定と食い違う状況を作る。
	// 「ロックの外で読んでから、ロックの下で適用されるまでの間に動いた」を、
	// 次の drifts 回の読みで再現する。
	drifting *config.Config
	drifts   int
}

func (c *fakeController) CameraModes(_ context.Context, device string) ([]source.Mode, error) {
	c.modesCalls++
	c.modesFor = device
	return c.modes, c.modesErr
}

// Snapshot は、next が置かれている場合、Apply の後だけそちらを返す。「この PUT が
// ロックを離した直後に、別のクライアントの変更が入った」状況そのもの。
func (c *fakeController) Snapshot() config.Config {
	if c.drifts > 0 && c.drifting != nil {
		c.drifts--
		return *c.drifting
	}
	if c.applied && c.next != nil {
		return *c.next
	}
	return c.cfg
}

func (c *fakeController) Overridden() []string { return c.overridden }

func (c *fakeController) Apply(_ context.Context, cfg config.Config, ifAny []string) (config.Config, []string, error) {
	c.askedRevision = ifAny
	if len(ifAny) > 0 && !slices.Contains(ifAny, config.Token(c.cfg)) {
		return c.cfg, nil, fmt.Errorf("%w: it was built on %s", config.ErrRevisionMismatch, strings.Join(ifAny, ", "))
	}
	if c.applyErr != nil {
		return config.Config{}, nil, c.applyErr
	}
	c.cfg = cfg
	c.applied = true
	return c.cfg, c.deferred, nil
}

func (c *fakeController) Switch(_ context.Context, sourceType string) error {
	c.switched = sourceType
	c.cfg.Source.Type = sourceType
	return nil
}

func (c *fakeController) Devices(context.Context) Devices {
	c.devicesCalls++
	return c.devices
}

func TestManagementAPIIsAbsentUnlessEnabled(t *testing.T) {
	s, _, _ := newTestServer(t, Options{Controller: &fakeController{}, EnableAdmin: false})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// ルートが全部を受けるので、登録されていない管理用のパスはストリームを
	// 返さず 404 になる。固定しておく価値があるのはその挙動。
	resp, err := http.Get(ts.URL + "/api/v1/config")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the API is disabled", resp.StatusCode)
	}
}

func TestManagementAPI(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default()}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	t.Run("get config", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/config")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()

		var got config.Config
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Server.Listen != config.Default().Server.Listen {
			t.Errorf("Listen = %q, want the default", got.Server.Listen)
		}
	})

	t.Run("put config", func(t *testing.T) {
		updated := config.Default()
		updated.Source.Type = config.SourceMJPEG
		updated.Source.MJPEG.URL = "http://192.168.1.50/"
		body, _ := json.Marshal(updated)

		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d: %s", resp.StatusCode, out)
		}
		if !ctrl.applied || ctrl.cfg.Source.Type != config.SourceMJPEG {
			t.Errorf("controller did not receive the new settings: %+v", ctrl.cfg.Source)
		}
	})

	t.Run("put invalid config is rejected", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", strings.NewReader(`{"source":{"type":"nonsense"}}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("switch source", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(`{"type":"serial"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if ctrl.switched != "serial" {
			t.Errorf("switched to %q, want serial", ctrl.switched)
		}
	})

	t.Run("devices", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/devices")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
}

// ループバックで listen することはそれ自体では防御にならない。ユーザーが訪れた
// どのページも 127.0.0.1 に到達できるし、フォーム形式の POST は preflight 無しで
// そこへ届く。そうしたリクエストが、トラッカーからカメラを奪えてはいけない。
func TestManagementAPIRejectsRequestsAPageCouldSend(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		origin      string
		host        string
		want        int
	}{
		{
			// 抜け道。text/plain は単純リクエストなので preflight は一度も
			// 行われず、下の Origin の検査には出番が回ってこない。
			name:        "simple post with a plain text body",
			contentType: "text/plain;charset=UTF-8",
			want:        http.StatusUnsupportedMediaType,
		},
		{
			name:        "form post",
			contentType: "application/x-www-form-urlencoded",
			want:        http.StatusUnsupportedMediaType,
		},
		{
			// JSON を送るページは preflight を引き起こし、その preflight が
			// 失敗するのはこれが理由。
			name:        "json post from a foreign page",
			contentType: "application/json",
			origin:      "https://evil.example",
			want:        http.StatusForbidden,
		},
		{
			// DNS リバインディング。名前はループバックに解決されるが、Host には
			// その名前が載って届く。
			name:        "rebound host name",
			contentType: "application/json",
			host:        "evil.example",
			want:        http.StatusForbidden,
		},
		{
			name:        "json post from a local page",
			contentType: "application/json",
			origin:      "http://127.0.0.1:18080",
			want:        http.StatusOK,
		},
		{
			// curl とトレイは Origin をまったく送らない。
			name:        "json post with no origin",
			contentType: "application/json",
			want:        http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &fakeController{cfg: config.Default()}
			s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/source", strings.NewReader(`{"type":"serial"}`))
			req.Header.Set("Content-Type", tc.contentType)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.host != "" {
				req.Host = tc.host
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				out, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, out)
			}
			if switched := ctrl.switched != ""; switched != (tc.want == http.StatusOK) {
				t.Errorf("controller switched = %v, but the request returned %d", switched, resp.StatusCode)
			}
		})
	}
}

// GET は preflight を必要とせず、ページがサブリソースとして要求した場合は Origin も
// 載らない — <img src="http://127.0.0.1:18080/api/v1/devices">。ページは応答を
// 読めないが、応答はただではない。デバイスの列挙は ffmpeg を起動して待つので、URL を
// 次々に叩くページは、その機械でプロセスを起動し続けられる。Sec-Fetch-Site は
// リクエストの出所を述べるもので、どのページも偽装できず、ブラウザに送信をやめさせる
// こともできない。
func TestDeviceEnumerationRefusesACrossSiteGet(t *testing.T) {
	cases := []struct {
		name string
		site string
		want int
	}{
		{name: "a subresource on someone else's page", site: "cross-site", want: http.StatusForbidden},
		{name: "another port on this machine", site: "same-site", want: http.StatusForbidden},
		{name: "a page served by the bridge", site: "same-origin", want: http.StatusOK},
		{name: "the address typed in", site: "none", want: http.StatusOK},
		{name: "curl, which sends no such header", site: "", want: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &fakeController{cfg: config.Default()}
			s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/devices", nil)
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				out, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, out)
			}
			if listed := ctrl.devicesCalls > 0; listed != (tc.want == http.StatusOK) {
				t.Errorf("the controller was asked to enumerate = %v, but the request returned %d", listed, resp.StatusCode)
			}
		})
	}
}

// 反映はされたが書けなかった変更は、呼び出し側ではなくこちら側の失敗であり、
// 両者が同じ形で報告されてはいけない。
func TestManagementAPIReportsASaveFailureSeparately(t *testing.T) {
	ctrl := &fakeController{
		cfg:      config.Default(),
		applyErr: fmt.Errorf("%w to /nowhere: read-only file system", config.ErrNotSaved),
	}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body, _ := json.Marshal(config.Default())
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a persistence failure", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "could not be saved") {
		t.Errorf("body = %q, want it to say the settings were not saved", out)
	}
}

// boundary は PaperTracker のリリースに合わせるためのつまみなので、その変更は
// 再起動なしで線上まで届かなければならない。
func TestSetStreamOptionsAppliesToNewStreams(t *testing.T) {
	s, frames, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	frames.Publish(core.Frame{Data: testJPEG(t)})

	updated, err := core.NewMultipartEncoder("othermark", nil)
	if err != nil {
		t.Fatalf("NewMultipartEncoder: %v", err)
	}
	s.SetStreamOptions(updated, false)

	resp, err := http.Get(ts.URL + "/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	boundary, ok := core.BoundaryFromContentType(resp.Header.Get("Content-Type"))
	if !ok || boundary != "othermark" {
		t.Fatalf("boundary = %q (ok=%v), want othermark", boundary, ok)
	}

	buf := make([]byte, 64)
	n, _ := io.ReadFull(resp.Body, buf)
	if !bytes.Contains(buf[:n], []byte("--othermark")) {
		t.Errorf("first part = %q, want it delimited by the new boundary", buf[:n])
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	s, _, _ := newTestServer(t, Options{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// 読むのをやめたクライアントが、他のクライアントを遅くしてはいけない。
func TestManyStreamClients(t *testing.T) {
	s, h, _ := newTestServer(t, Options{HoldOnSourceLoss: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	publishUntilDone(t, h, testJPEG(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		defer resp.Body.Close()

		if i == 0 {
			// これはテストの間ずっと読まないままにしておく。
			continue
		}
		buf := make([]byte, 64)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatalf("client %d read: %v", i, err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.Subscribers() == 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Subscribers() = %d, want 3", h.Subscribers())
}

// hub は最後の画像を無期限に保持する。カメラが落ちている間の再接続すべてにそれを
// 流すと、トラッカーに同じ古い口の形を何度も食わせることになるので、喪失タイム
// アウトより古いフレームは送らない。
func TestStreamWithholdsAStaleOpeningFrame(t *testing.T) {
	frames := hub.New()
	s, _, _ := newTestServer(t, Options{Hub: frames})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	frames.Publish(core.Frame{
		Data:     testJPEG(t),
		RecvedAt: time.Now().Add(-time.Minute),
	})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	read := make(chan int, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := io.ReadFull(resp.Body, buf)
		read <- n
	}()

	select {
	case n := <-read:
		t.Errorf("the stream opened with %d bytes, want the stale frame withheld", n)
	case <-time.After(500 * time.Millisecond):
	}
}

// 届いたばかりのフレームは、再接続したクライアントが次のキャプチャを待たずに
// すぐ見るべきものであることに変わりはない。
func TestStreamSendsAFreshOpeningFrame(t *testing.T) {
	frames := hub.New()
	s, _, _ := newTestServer(t, Options{Hub: frames})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	frames.Publish(core.Frame{Data: testJPEG(t), RecvedAt: time.Now()})

	resp, err := http.Get(ts.URL + "/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 32)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read the opening part: %v", err)
	}
	if !bytes.Contains(buf, []byte("--"+core.DefaultBoundary)) {
		t.Errorf("opening bytes = %q, want the current frame", buf)
	}
}

// 失敗した列挙は「何も繋がっていない機械」ではないし、どちらだったかをユーザーに
// 伝えられるのは呼び出し側だけ。2 つのリストは独立に失敗するので、片方の失敗が
// もう片方の結果を道連れにしてはいけない。ffmpeg の無い機械にもシリアルポートは
// あるし、どちらも見せない選択画面は、両方について間違っていることになる。
func TestDevicesReportsAPartialListWithItsError(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default(), devices: Devices{
		CameraError: "ffmpeg is not executable",
		SerialPorts: []source.SerialPort{{Name: "COM5", Vendor: "Espressif"}},
	}}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/devices")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: half an answer is still an answer", resp.StatusCode)
	}
	var got Devices
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.CameraError, "not executable") {
		t.Errorf("camera_error = %q, want the enumeration error", got.CameraError)
	}
	if len(got.SerialPorts) != 1 || got.SerialPorts[0].Name != "COM5" {
		t.Errorf("serial_ports = %+v, want the list that did enumerate", got.SerialPorts)
	}
	if got.SerialError != "" {
		t.Errorf("serial_error = %q, want it empty: that enumeration worked", got.SerialError)
	}
}

// 設定ファイルは未知のキーを拒否するので、API もそれに揃わなければならない。
// さもないと綴りを誤ったフィールドは捨てられ、それが設定するはずだった値はゼロ値の
// まま残り、呼び出し側は「別のことをした変更」に対して 200 を受け取る。
func TestManagementAPIRejectsUnknownFields(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default()}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	t.Run("config", func(t *testing.T) {
		body := `{"server":{"hold_on_sorce_loss":true}}`
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for a misspelled field", resp.StatusCode)
		}
		if ctrl.applied {
			t.Error("the controller was handed a config built from a rejected body")
		}
	})

	t.Run("source", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(`{"tpye":"serial"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for a misspelled field", resp.StatusCode)
		}
		if ctrl.switched != "" {
			t.Errorf("the controller switched to %q from a rejected body", ctrl.switched)
		}
	})

	// 構造体が知っている本体は、これまでどおり受け入れられる。
	t.Run("well formed", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(`{"type":"serial"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
}

// 1 つの本体に 2 つの JSON 値があるのは、打ち間違いのある 1 つではなく 2 つの
// リクエスト。最初の 1 つをデコードしてそこで止まることは、呼び出し側が要求したのに
// 得られなかった変更について成功を報告することになる。
func TestManagementAPIRejectsTrailingContent(t *testing.T) {
	bodies := map[string]string{
		"a second value":   `{"type":"uvc"}{"type":"mjpeg"}`,
		"trailing junk":    `{"type":"uvc"} oops`,
		"a trailing array": `{"type":"uvc"}[1]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			ctrl := &fakeController{cfg: config.Default()}
			s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if ctrl.switched != "" {
				t.Errorf("the controller switched to %q from a rejected body", ctrl.switched)
			}
		})
	}
}

// 値の後ろの空白は単なる整形であって、2 つ目のリクエストではない。
func TestManagementAPIAcceptsTrailingWhitespace(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default()}
	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader("{\"type\":\"serial\"}\n\n"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// type が無ければ空文字列にデコードされるが、その先で空は「無い」を意味しない。
// Normalise はそれを未設定と読んで uvc を埋めるので、type を含まないリクエストは
// 拒否されるのではなく、動いているソースを動かしてしまう。
func TestManagementAPIRejectsAnEmptySourceType(t *testing.T) {
	for _, body := range []string{`{}`, `{"type":""}`, `{"type":"   "}`} {
		t.Run(body, func(t *testing.T) {
			ctrl := &fakeController{cfg: config.Default()}
			s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			resp, err := http.Post(ts.URL+"/api/v1/source", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if ctrl.switched != "" {
				t.Errorf("the controller switched to %q with no type given", ctrl.switched)
			}
		})
	}
}

// 購読と最新フレームの読み取りは 2 段階。その間に配信されたフレームは、新しい
// クライアントのキューに入ると同時に最新にもなるので、ストリームは同じ画像を 2 回
// 送って始まることになる。トラッカーにとっては、1 つの口の形が 2 つの標本になる。
func TestStreamDoesNotResendTheFrameItOpenedWith(t *testing.T) {
	s, h, _ := newTestServer(t, Options{HoldOnSourceLoss: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	first := testJPEG(t)
	h.Publish(core.Frame{Data: first})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	readPart := func() []byte {
		t.Helper()
		var length int
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read part header: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: ")); err == nil && strings.HasPrefix(line, "Content-Length:") {
				length = n
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Fatalf("read part body: %v", err)
		}
		// 各パートは次の区切りの前に、自身の CRLF で終わる。
		trailer := make([]byte, 2)
		if _, err := io.ReadFull(reader, trailer); err != nil {
			t.Fatalf("read part trailer: %v", err)
		}
		return body
	}

	// ストリームが最初に送るフレーム。クライアントのキューに既に入っていたもの。
	if got := readPart(); !bytes.Equal(got, first) {
		t.Fatal("the stream did not open with the current frame")
	}

	// 見分けのつく 2 枚目。最初のものが再送されていれば、この読み取りは代わりに
	// それをもう一度返すことになる。
	second := append(bytes.Clone(first), 0x00)
	h.Publish(core.Frame{Data: second})
	if got := readPart(); !bytes.Equal(got, second) {
		t.Error("the frame the stream opened with was sent a second time")
	}
}

// 上の読み飛ばしが対象としている重なり。クライアントの接続中に配信されたフレームは、
// hub の最新であると同時に、そのクライアントのキューの先頭でもある。競合そのものは
// テストから作り出せない — 配信が Subscribe と Latest の間、1 つのハンドラ内の
// 2 つの呼び出しの間に落ちる必要がある — が、それが生む状況は示せるし、両者を
// 見分けるのは連番だ。
func TestHubQueuesTheFrameThatIsAlsoTheLatest(t *testing.T) {
	h := hub.New()
	frames, cancel := h.Subscribe()
	defer cancel()

	h.Publish(core.Frame{Data: testJPEG(t)})

	latest, ok := h.Latest()
	if !ok {
		t.Fatal("no latest frame after publishing")
	}
	select {
	case queued := <-frames:
		if queued.Seq != latest.Seq {
			t.Fatalf("queued frame %d, latest %d: want the same frame in both", queued.Seq, latest.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the frame never reached the subscriber")
	}
}

// fakeFetcher は ffmpeg のダウンロードの代役。
type fakeFetcher struct {
	state  ffmpegfetch.State
	starts int
	err    error
}

func (f *fakeFetcher) State() ffmpegfetch.State { return f.state }

func (f *fakeFetcher) Start() error {
	f.starts++
	if f.err != nil {
		return f.err
	}
	f.state.Downloading = true
	return nil
}

func TestFFmpegEndpointIsAbsentWithoutAFetcher(t *testing.T) {
	s, _, _ := newTestServer(t, Options{Controller: &fakeController{}, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/ffmpeg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 with no fetcher wired in", resp.StatusCode)
	}
}

func TestFFmpegEndpointReportsAndStarts(t *testing.T) {
	fetcher := &fakeFetcher{state: ffmpegfetch.State{
		Total:     145349145,
		Supported: true,
		Source:    ffmpegfetch.Build{URL: "https://example.invalid/ffmpeg.zip", Publisher: "test"},
	}}
	s, _, _ := newTestServer(t, Options{Controller: &fakeController{}, EnableAdmin: true, FFmpeg: fetcher})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	t.Run("GET reports the state", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/ffmpeg")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var state ffmpegfetch.State
		if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if state.Source.URL != "https://example.invalid/ffmpeg.zip" {
			t.Errorf("source URL = %q, want the pinned archive", state.Source.URL)
		}
		if fetcher.starts != 0 {
			t.Errorf("a GET started %d downloads, want none", fetcher.starts)
		}
	})

	t.Run("POST starts one", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/ffmpeg", "application/json", nil)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		// OK ではなく Accepted。ダウンロードはリクエストより数分長く生き続ける。
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("status = %d, want 202", resp.StatusCode)
		}
		if fetcher.starts != 1 {
			t.Errorf("starts = %d, want 1", fetcher.starts)
		}
	})
}

// 実行中にもう一度頼むのは同じ要求であって、2 つ目のダウンロードではない。
func TestFFmpegEndpointAcceptsARepeatedRequestWhileBusy(t *testing.T) {
	fetcher := &fakeFetcher{err: ffmpegfetch.ErrBusy}
	s, _, _ := newTestServer(t, Options{Controller: &fakeController{}, EnableAdmin: true, FFmpeg: fetcher})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/ffmpeg", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202 when a download is already running", resp.StatusCode)
	}
}

// 既に動いている ffmpeg の上に、100 メガバイトを取り直したりはしない。
func TestFFmpegEndpointDoesNotRefetchAnInstalledCopy(t *testing.T) {
	fetcher := &fakeFetcher{state: ffmpegfetch.State{Installed: true, Path: `C:\ffmpeg.exe`}}
	s, _, _ := newTestServer(t, Options{Controller: &fakeController{}, EnableAdmin: true, FFmpeg: fetcher})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/ffmpeg", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for an install that is already there", resp.StatusCode)
	}
	if fetcher.starts != 0 {
		t.Errorf("starts = %d, want none over an installed copy", fetcher.starts)
	}
}

// ダウンロードのエンドポイントも他と同じ番人の後ろにある。ページが機械に 100
// メガバイトを引かせられてはいけない。
func TestFFmpegEndpointRefusesACrossSiteRequest(t *testing.T) {
	fetcher := &fakeFetcher{}
	s, _, _ := newTestServer(t, Options{Controller: &fakeController{}, EnableAdmin: true, FFmpeg: fetcher})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ffmpeg", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if fetcher.starts != 0 {
		t.Errorf("a cross-site request started %d downloads", fetcher.starts)
	}
}

// ui が存在する前に書かれたクライアントはそれを送れないし、実際に送ってくる設定を
// 変えるのはそのクライアントの領分ではない。名前を挙げなかったフィールドを既定値へ
// 落とすと、起動時にしか変更できない設定への変更要求に見えてしまい、リクエスト全体が
// 拒否される。触れてもいないもののせいで、そのクライアントは管理 API から締め出される。
func TestConfigPutKeepsSettingsTheRequestNeverNamed(t *testing.T) {
	running := config.Default()
	running.UI.Language = "ja"
	ctrl := &fakeController{cfg: running}

	s, _, _ := newTestServer(t, Options{Controller: ctrl, EnableAdmin: true})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	put := func(t *testing.T, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	// 古いスキーマ。知っているものはすべて含み、ui は含まない。
	old := config.Default()
	old.UI = config.UI{}
	body, _ := json.Marshal(old)
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	delete(fields, "ui")
	withoutUI, _ := json.Marshal(fields)

	resp := put(t, string(withoutUI))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want the request accepted: %s", resp.StatusCode, out)
	}
	if got := ctrl.cfg.UI.Language; got != "ja" {
		t.Errorf("language = %q, want the running ja carried through untouched", got)
	}

	// 名前を挙げればちゃんと変わる。引き継ぎが本物の編集を隠すことはない。
	named := config.Default()
	named.UI.Language = "en"
	namedBody, _ := json.Marshal(named)

	resp = put(t, string(namedBody))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, out)
	}
	if got := ctrl.cfg.UI.Language; got != "en" {
		t.Errorf("language = %q, want the explicit en", got)
	}
}

// 設定は全体で 1 つの値として受け渡されるので、呼び出し側は「読んで、変えたい葉を
// 重ねて、書く」という往復をする。その 2 つの要求の間に別のクライアントが変更を
// 確定させても、条件を付けなければ誰にも分からない。後から書いた側が黙って消す。
func TestConfigOffersAVersionAndHonoursIt(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default()}
	s, _, _ := newTestServer(t, Options{EnableAdmin: true, Controller: ctrl})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	current := config.Token(ctrl.cfg)

	putConfig := func(t *testing.T, cfg config.Config, ifMatch string) *http.Response {
		t.Helper()
		body, _ := json.Marshal(cfg)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	put := func(t *testing.T, ifMatch string) *http.Response {
		t.Helper()
		return putConfig(t, ctrl.cfg, ifMatch)
	}

	t.Run("the version comes back with the settings", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/config")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		if got, want := resp.Header.Get("ETag"), `"`+current+`"`; got != want {
			t.Errorf("ETag = %q, want %q", got, want)
		}
	})

	t.Run("a stale version is refused", func(t *testing.T) {
		ctrl.applied = false
		resp := put(t, `"not-the-settings-that-are-here"`)
		if resp.StatusCode != http.StatusPreconditionFailed {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 412: %s", resp.StatusCode, out)
		}
		if ctrl.applied {
			t.Error("the settings were applied even though the caller was working from an older version")
		}
	})

	t.Run("the current version is accepted and the response carries the new one", func(t *testing.T) {
		ctrl.applied = false
		// 送るのは今と違う設定。応答の札は、その届いた設定のものでなければ
		// ならない。後から入った別の変更の札を付けて返すと、それを土台にした
		// 次の変更が、間の変更を消せてしまう。
		changed := config.Default()
		changed.Transform.Rotate = 180
		resp := putConfig(t, changed, `"`+current+`"`)
		if resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d: %s", resp.StatusCode, out)
		}
		if !ctrl.applied {
			t.Error("the settings were not applied")
		}
		if got, want := resp.Header.Get("ETag"), `"`+config.Token(changed)+`"`; got != want {
			t.Errorf("ETag = %q, want %q — the tag must belong to the settings in the response body", got, want)
		}
		current = config.Token(ctrl.cfg)
	})

	// 応答の本体と札は、同じ瞬間のものでなければならない。適用が終わってから
	// 札を訊き直すと、その隙間に入った別の変更の札を、こちらの本体に付けて
	// 返すことになる。それを土台にした次の変更は条件を通り、間の変更を消す。
	t.Run("the tag belongs to the body even when another change lands right after", func(t *testing.T) {
		interleaved := config.Default()
		interleaved.Transform.FlipV = true
		ctrl.applied, ctrl.next = false, &interleaved
		t.Cleanup(func() { ctrl.next = nil })

		mine := config.Default()
		mine.Transform.Rotate = 90
		resp := putConfig(t, mine, "")
		if resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d: %s", resp.StatusCode, out)
		}
		if got, want := resp.Header.Get("ETag"), `"`+config.Token(mine)+`"`; got != want {
			t.Errorf("ETag = %q, want %q — it names the change that landed after this one, so building on it would erase that change", got, want)
		}
		current = config.Token(ctrl.cfg)
	})

	// 並べられた札は、どれか 1 つが一致すれば通る。1 つに絞ると、使えたはずの
	// 候補があるのに断ることになる。
	t.Run("any one of several versions is enough", func(t *testing.T) {
		ctrl.applied = false
		if resp := put(t, `"long-gone", "`+current+`"`); resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
		}
		if !ctrl.applied {
			t.Error("a caller that offered the current version among others was refused")
		}
	})

	// 並べ方も 1 つとは限らない。同じ名前のヘッダーを何行かに分けて送るのも、
	// 1 行にカンマで並べるのと同じ意味 (RFC 9110)。行を 1 本しか読まないと、
	// 2 行目に書かれた今の札に気づかず断ることになる。
	t.Run("versions spread over several lines are all read", func(t *testing.T) {
		ctrl.applied = false
		body, _ := json.Marshal(ctrl.cfg)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Add("If-Match", `"long-gone"`)
		req.Header.Add("If-Match", `"`+current+`"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
		}
		if !ctrl.applied {
			t.Error("a caller that spread its versions over several header lines was refused")
		}
	})

	// 弱い札は候補にしない。If-Match は強い比較を求める (RFC 9110)。弱い札が
	// 言っているのは「見た目は同じ」であって「同じもの」ではないので、中身が
	// 揃っていても、上書きしてよいかの判断には使えない。
	t.Run("a weak version is not a candidate", func(t *testing.T) {
		ctrl.applied = false
		resp := put(t, `W/"`+current+`"`)
		if resp.StatusCode != http.StatusPreconditionFailed {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 412: %s", resp.StatusCode, out)
		}
		if ctrl.applied {
			t.Error("the settings were applied on a weak validator, which If-Match must not match")
		}
	})

	// 弱い札を落としても、条件が無かったことにはしない。落として素通しにすると、
	// 競合を防いだつもりの要求が、防がないまま通る。
	t.Run("a weak version does not open the way for a strong one", func(t *testing.T) {
		ctrl.applied = false
		if resp := put(t, `W/"long-gone", "`+current+`"`); resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
		}
		if !ctrl.applied {
			t.Error("a caller that offered the current version alongside a weak one was refused")
		}
	})

	t.Run("no condition is still accepted", func(t *testing.T) {
		ctrl.applied, ctrl.askedRevision = false, []string{"stale"}
		if resp := put(t, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if !ctrl.applied {
			t.Error("a caller that asked for no condition was refused")
		}
		if len(ctrl.askedRevision) != 0 {
			t.Errorf("asked for %v, want no condition", ctrl.askedRevision)
		}
	})

	// 条件を付けていないのは、ヘッダーそのものが無いときだけ。付いていて中身が
	// 空なのは、札を渡し損ねた要求。空を「条件なし」に読み替えると、防いだつもりの
	// 上書きがそのまま通る。
	t.Run("an empty condition is not the same as no condition", func(t *testing.T) {
		ctrl.applied = false
		body, _ := json.Marshal(ctrl.cfg)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header["If-Match"] = []string{""}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPreconditionFailed {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 412: %s", resp.StatusCode, out)
		}
		if ctrl.applied {
			t.Error("the settings were applied on an If-Match that carried no version at all")
		}
	})

	t.Run("* is accepted", func(t *testing.T) {
		ctrl.applied = false
		if resp := put(t, "*"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if !ctrl.applied {
			t.Error("If-Match: * was refused")
		}
	})

	// 省略された項目はこちらが現在の設定から補う。だからその設定の上で適用されな
	// ければ、呼び出し側が送ってもいない項目が、別の設定の値で復活する。
	t.Run("settings left out of the request are filled in from the settings the change lands on", func(t *testing.T) {
		ctrl.applied = false
		// 読みが 1 回だけ食い違う。ui だけが違う設定を返す。
		drifted := ctrl.cfg
		drifted.UI.Language = "ja"
		ctrl.drifting, ctrl.drifts = &drifted, 1
		t.Cleanup(func() { ctrl.drifting, ctrl.drifts = nil, 0 })
		was := ctrl.cfg.UI.Language

		// ui を省いた本体。古いスキーマのクライアントがこう送る。
		full, _ := json.Marshal(ctrl.cfg)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(full, &fields); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		delete(fields, "ui")
		body, _ := json.Marshal(fields)

		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
		}
		if got := ctrl.cfg.UI.Language; got != was {
			t.Errorf("ui.language = %q, want %q — the request never carried a ui, so it must come from the settings it landed on, not from a reading that had already moved on", got, was)
		}
		if len(ctrl.askedRevision) != 1 || ctrl.askedRevision[0] != config.Token(ctrl.cfg) {
			t.Errorf("asked for %v, want the version of the settings the omitted parts were taken from", ctrl.askedRevision)
		}
		current = config.Token(ctrl.cfg)
	})

	// 落ち着かなければ断る。黙って別の設定の値を復活させるより、断るほうがよい。
	t.Run("settings that keep moving while the gaps are filled are refused", func(t *testing.T) {
		ctrl.applied = false
		drifted := ctrl.cfg
		drifted.UI.Language = "ja"
		ctrl.drifting, ctrl.drifts = &drifted, 99
		t.Cleanup(func() { ctrl.drifting, ctrl.drifts = nil, 0 })

		full, _ := json.Marshal(ctrl.cfg)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(full, &fields); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		delete(fields, "ui")
		body, _ := json.Marshal(fields)

		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/config", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPreconditionFailed {
			out, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 412: %s", resp.StatusCode, out)
		}
		if ctrl.applied {
			t.Error("the settings were applied with a ui taken from a reading that never matched the settings underneath")
		}
	})

	// 読めない条件は落とさない。落として「条件なし」に変えると、競合を防いだ
	// つもりの要求が、防がないまま通る。どの札とも一致しないものとして断る。
	t.Run("an unreadable condition still refuses", func(t *testing.T) {
		ctrl.applied = false
		resp := put(t, `bare-word-without-quotes`)
		if resp.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("status = %d, want 412", resp.StatusCode)
		}
		if ctrl.applied {
			t.Error("the settings were applied even though the condition could not be read")
		}
	})
}
