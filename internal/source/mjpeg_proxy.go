package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// Defaults for the upstream MJPEG connection.
const (
	defaultConnectTimeout = 5 * time.Second
	defaultStallTimeout   = 5 * time.Second
)

// MJPEGConfig configures the upstream MJPEG proxy driver.
type MJPEGConfig struct {
	// URL is the upstream stream, for example http://192.168.1.50/.
	URL string
	// ConnectTimeout bounds the handshake and the wait for response headers.
	// The stream body itself is unbounded.
	ConnectTimeout time.Duration
	// StallTimeout is how long the stream may go quiet before it counts as
	// disconnected.
	StallTimeout time.Duration
	// MaxFrameSize bounds a single JPEG; zero selects the core default.
	MaxFrameSize int
}

// MJPEGProxy re-serves an existing MJPEG-over-HTTP stream.
//
// Frames are decomposed to bare JPEGs and re-framed by our own encoder rather
// than passed through byte for byte. The upstream's boundary string, header
// set and Content-Length habits vary by firmware, and the PaperTracker client
// is strict about all three.
type MJPEGProxy struct {
	cfg      MJPEGConfig
	client   *http.Client
	log      *slog.Logger
	reporter Reporter
	// safeURL is the upstream with any password masked. Every message that
	// names the stream uses it, because a camera behind basic auth would
	// otherwise write its credentials into the log on each reconnect.
	safeURL string
}

// NewMJPEGProxy builds the driver and validates the upstream URL up front.
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
	// url.Parse is happy with "http:///stream". The transport is not, and its
	// "no Host in request URL" would arrive as an ordinary error that the
	// reconnect loop then retries forever.
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
	transport.ResponseHeaderTimeout = cfg.ConnectTimeout
	// A camera serves one stream per connection; pooling gains nothing and
	// keeps a dead socket around.
	transport.DisableKeepAlives = true

	return &MJPEGProxy{
		cfg:      cfg,
		client:   &http.Client{Transport: transport},
		log:      log,
		reporter: reporter,
		safeURL:  u.Redacted(),
	}, nil
}

// Name implements Source.
func (p *MJPEGProxy) Name() string { return "mjpeg" }

// Run implements Source.
func (p *MJPEGProxy) Run(ctx context.Context, out chan<- core.Frame) error {
	return runWithBackoff(ctx, p.log, p.Name(), p.reporter, func(ctx context.Context) error {
		return p.session(ctx, out)
	})
}

// session holds one upstream connection open and forwards its frames.
func (p *MJPEGProxy) session(ctx context.Context, out chan<- core.Frame) error {
	// A stalled stream is not a closed one, so the read is unblocked by
	// cancelling the request rather than by a read deadline.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.cfg.URL, nil)
	if err != nil {
		return fatalf(fmt.Errorf("mjpeg: build request: %w", err))
	}
	req.Header.Set("Accept", "multipart/x-mixed-replace, image/jpeg")

	// Getting this far is the transport's job, bounded by its own
	// ResponseHeaderTimeout. The stall timer must not start until the
	// response is in hand: started any earlier it spends its budget on DNS,
	// TCP, TLS and the header wait, and a camera that takes a moment to
	// answer would have the body cancelled out from under it.
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
			// The server answered and refused us: a wrong path, or credentials
			// it will not accept. Unlike a refused connection there is no
			// version of this that a retry fixes, so it is reported as fatal
			// and Apply gets to roll back to the source that was working.
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
		// Either a bare concatenated JPEG stream or a multipart response with
		// no usable boundary parameter. Structural scanning handles both.
		p.log.Debug("upstream has no usable boundary, scanning for images", "content_type", contentType, "url", p.safeURL)
	}

	assembler := newFrameAssembler(split, p.cfg.MaxFrameSize)
	buf := make([]byte, readChunk)
	var count uint64

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(p.cfg.StallTimeout)
			for _, f := range assembler.feed(buf[:n]) {
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

// permanentHTTPStatus reports whether a status means the request itself is
// wrong, rather than the server being briefly unable to serve it.
//
// 4xx is the client's fault by definition, with the exceptions below: those
// three ask the caller to come back later, which is exactly what the reconnect
// loop does.
func permanentHTTPStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return code >= 400 && code < 500
}
