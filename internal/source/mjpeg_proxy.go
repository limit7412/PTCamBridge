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

	stall := time.AfterFunc(p.cfg.StallTimeout, cancel)
	defer stall.Stop()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.cfg.URL, nil)
	if err != nil {
		return fatalf(fmt.Errorf("mjpeg: build request: %w", err))
	}
	req.Header.Set("Accept", "multipart/x-mixed-replace, image/jpeg")

	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() == nil && reqCtx.Err() != nil {
			return fmt.Errorf("mjpeg: %s sent nothing for %s", p.cfg.URL, p.cfg.StallTimeout)
		}
		return fmt.Errorf("mjpeg: connect to %s: %w", p.cfg.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mjpeg: %s returned %s", p.cfg.URL, resp.Status)
	}

	split := core.SplitJPEGStream
	contentType := resp.Header.Get("Content-Type")
	if boundary, ok := core.BoundaryFromContentType(contentType); ok {
		p.log.Debug("upstream is multipart", "boundary", boundary, "url", p.cfg.URL)
		split = func(buf []byte, maxSize int) ([][]byte, []byte) {
			return core.SplitMultipart(buf, boundary, maxSize)
		}
	} else {
		// Either a bare concatenated JPEG stream or a multipart response with
		// no usable boundary parameter. Structural scanning handles both.
		p.log.Debug("upstream has no usable boundary, scanning for images", "content_type", contentType, "url", p.cfg.URL)
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
				return fmt.Errorf("mjpeg: %s went quiet for %s", p.cfg.URL, p.cfg.StallTimeout)
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("mjpeg: %s closed the stream", p.cfg.URL)
			}
			return fmt.Errorf("mjpeg: read from %s: %w", p.cfg.URL, err)
		}
	}
}
