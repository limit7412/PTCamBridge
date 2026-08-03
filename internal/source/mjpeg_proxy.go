package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// 上流 MJPEG 接続の既定値。
const (
	defaultConnectTimeout = 5 * time.Second
	defaultStallTimeout   = 5 * time.Second
)

// MJPEGConfig は、上流 MJPEG を中継するドライバの設定です。
type MJPEGConfig struct {
	// URL は上流のストリームです。たとえば http://192.168.1.50/ です。
	URL string
	// ConnectTimeout は、接続に至る全段階 — DNS、TCP、TLS、応答ヘッダーの待ち —
	// に対する上限です。
	// ストリーム本体そのものに上限はありません。
	ConnectTimeout time.Duration
	// StallTimeout は、切断とみなすまでにストリームがフレームを出さずにいられる
	// 時間です。
	StallTimeout time.Duration
	// MaxFrameSize は JPEG 1 枚の上限です。0 なら core の既定値を使います。
	MaxFrameSize int
}

// MJPEGProxy は、既存の MJPEG-over-HTTP ストリームを配信し直します。
//
// フレームはバイト単位でそのまま流すのではなく、裸の JPEG に分解して自前の
// エンコーダで framing し直します。上流の boundary 文字列、ヘッダー構成、
// Content-Length の付け方はファームウェアによって異なり、PaperTracker
// クライアントはその 3 つすべてに厳格だからです。
type MJPEGProxy struct {
	cfg      MJPEGConfig
	client   *http.Client
	log      *slog.Logger
	reporter Reporter
	// safeURL は、パスワードを伏せた上流の URL です。ストリームに言及する
	// メッセージはすべてこれを使います。そうしないと Basic 認証の内側にある
	// カメラは、再接続のたびに資格情報をログに書き込むことになります。
	safeURL string
}

// NewMJPEGProxy はドライバを組み立て、上流 URL を先に検証します。
func NewMJPEGProxy(cfg MJPEGConfig, log *slog.Logger, reporter Reporter) (*MJPEGProxy, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("mjpeg: no upstream URL configured")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mjpeg: parse URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("mjpeg: unsupported URL scheme %q", u.Scheme)
	}
	// url.Parse は "http:///stream" を通してしまう。transport は通さず、その
	// "no Host in request URL" は普通のエラーとしてやって来るので、再接続ループが
	// 永遠に再試行することになる。
	if u.Host == "" {
		return nil, fmt.Errorf("mjpeg: URL %q has no host", cfg.URL)
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = defaultConnectTimeout
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = defaultStallTimeout
	}
	if reporter == nil {
		reporter = NopReporter{}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	// ヘッダーの待ちだけでなく、接続に至る全段階に効かせる。
	// ResponseHeaderTimeout だけでは DNS・TCP・TLS が transport のはるかに長い
	// 既定値のまま残るので、到達できないカメラは、このフィールドが約束する
	// タイムアウトをはるかに超えて再接続の試行を握り続ける。
	transport.DialContext = (&net.Dialer{
		Timeout:   cfg.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = cfg.ConnectTimeout
	transport.ResponseHeaderTimeout = cfg.ConnectTimeout
	// カメラは 1 接続につき 1 ストリームしか出さない。プールしても得るものは無く、
	// 死んだソケットを抱え込むだけ。
	transport.DisableKeepAlives = true

	return &MJPEGProxy{
		cfg:      cfg,
		client:   &http.Client{Transport: transport},
		log:      log,
		reporter: reporter,
		safeURL:  u.Redacted(),
	}, nil
}

// Name は Source を実装します。
func (p *MJPEGProxy) Name() string { return "mjpeg" }

// Run は Source を実装します。
func (p *MJPEGProxy) Run(ctx context.Context, out chan<- core.Frame) error {
	return runWithBackoff(ctx, p.log, p.Name(), p.reporter, func(ctx context.Context) error {
		return p.session(ctx, out)
	})
}

// session は上流への接続を 1 本開いたまま保ち、そのフレームを転送します。
func (p *MJPEGProxy) session(ctx context.Context, out chan<- core.Frame) error {
	// 止まったストリームは閉じたストリームではない。だから読み取りの解除は
	// 読み取りデッドラインではなく、リクエストのキャンセルで行う。
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.cfg.URL, nil)
	if err != nil {
		return fatalf(fmt.Errorf("mjpeg: build request: %w", err))
	}
	req.Header.Set("Accept", "multipart/x-mixed-replace, image/jpeg")

	// ここまで到達させるのは transport の仕事で、上限は transport 自身の
	// ResponseHeaderTimeout。停滞タイマーは応答を手にするまで開始してはならない。
	// それより早く始めると、持ち時間を DNS・TCP・TLS とヘッダー待ちに使ってしまい、
	// 応答に少し時間のかかるカメラは、本体を足元からキャンセルされることになる。
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("mjpeg: connect to %s: %w", p.safeURL, err)
	}
	defer resp.Body.Close()

	stall := time.AfterFunc(p.cfg.StallTimeout, cancel)
	defer stall.Stop()

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("mjpeg: %s returned %s", p.safeURL, resp.Status)
		if permanentHTTPStatus(resp.StatusCode) {
			// サーバは応答したうえで拒否した。パスが違うか、受け付けられない
			// 資格情報か。接続拒否と違い、これは再試行で直る類のものではない。
			// だから致命的として報告し、Apply が動いていたソースへ巻き戻せる
			// ようにする。
			return fatalf(err)
		}
		return err
	}

	split := core.SplitJPEGStream
	contentType := resp.Header.Get("Content-Type")
	if boundary, ok := core.BoundaryFromContentType(contentType); ok {
		p.log.Debug("upstream is multipart", "boundary", boundary, "url", p.safeURL)
		split = func(buf []byte, maxSize int) ([][]byte, []byte) {
			return core.SplitMultipart(buf, boundary, maxSize)
		}
	} else {
		// 裸の JPEG を連結したストリームか、使える boundary パラメータの無い
		// multipart 応答のどちらか。構造走査はその両方を扱える。
		p.log.Debug("upstream has no usable boundary, scanning for images", "content_type", contentType, "url", p.safeURL)
	}

	assembler := newFrameAssembler(split, p.cfg.MaxFrameSize)
	buf := make([]byte, readChunk)
	var count uint64

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			// バイトではなくフレームで測る。壊れたパートを少しずつ送り続ける
			// 上流はソケットを忙しくさせるだけで画像を一枚も生まない。到着だけで
			// タイマーを戻していると、/healthz がソースを失ったと報告している間も
			// そのセッションを永遠に開き続けることになる。
			for _, f := range assembler.feed(buf[:n]) {
				stall.Reset(p.cfg.StallTimeout)
				if count == 0 {
					p.reporter.Connected(p.Name())
				}
				count++
				if sendErr := send(ctx, out, f); sendErr != nil {
					return nil
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if reqCtx.Err() != nil {
				return fmt.Errorf("mjpeg: %s went quiet for %s", p.safeURL, p.cfg.StallTimeout)
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("mjpeg: %s closed the stream", p.safeURL)
			}
			return fmt.Errorf("mjpeg: read from %s: %w", p.safeURL, err)
		}
	}
}

// permanentHTTPStatus は、そのステータスが「リクエスト自体が誤っている」ことを
// 意味するのか、「サーバが一時的に応じられない」だけなのかを返します。
//
// 4xx は定義上クライアント側の誤りですが、下記の例外があります。その 3 つは
// 「後で来い」と言っているのであり、それはまさに再接続ループがすることです。
func permanentHTTPStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return code >= 400 && code < 500
}
