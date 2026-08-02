// Package server publishes the bridged camera as the MJPEG-over-HTTP stream
// the PaperTracker client expects, plus the status and management endpoints.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/source"
	"github.com/limit7412/PTCamBridge/internal/status"
)

// sourceLossTimeout is how long a stream may go without a frame before the
// source counts as lost. It also gates /healthz.
const sourceLossTimeout = 2 * time.Second

// readHeaderTimeout bounds the request line and headers. The response body is
// deliberately unbounded: it is an endless stream.
const readHeaderTimeout = 10 * time.Second

// streamBufferSize preallocates the per-client encode buffer.
const streamBufferSize = 64 << 10

// Devices is what the management API reports for the source pickers.
type Devices struct {
	Cameras     []source.Device     `json:"cameras"`
	SerialPorts []source.SerialPort `json:"serial_ports"`
}

// Controller lets the management API drive the bridge without the server
// package knowing how sources are started.
type Controller interface {
	// Snapshot returns the settings currently in effect.
	Snapshot() config.Config
	// Apply validates and adopts new settings.
	Apply(ctx context.Context, cfg config.Config) error
	// Switch changes the active source type.
	Switch(ctx context.Context, sourceType string) error
	// Devices lists the cameras and serial ports available right now.
	Devices(ctx context.Context) (Devices, error)
}

// Options configures a Server.
type Options struct {
	Hub     *hub.Hub
	Status  *status.Tracker
	Encoder core.MultipartEncoder
	Logger  *slog.Logger
	// Controller enables the management API. It is ignored unless
	// EnableAdmin is also set.
	Controller Controller
	// EnableAdmin serves /api/v1/*. The bridge turns this off whenever the
	// listener is not on loopback, because the API has no authentication.
	EnableAdmin bool
	// HoldOnSourceLoss keeps stream clients connected while the camera
	// reconnects rather than closing the response.
	HoldOnSourceLoss bool
	Version          string
}

// Server serves the stream and status endpoints.
type Server struct {
	opts Options
	log  *slog.Logger
}

// New builds a server. Hub, Status and Logger must be set.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Hub == nil:
		return nil, errors.New("server: hub is required")
	case opts.Status == nil:
		return nil, errors.New("server: status tracker is required")
	case opts.Logger == nil:
		return nil, errors.New("server: logger is required")
	}
	return &Server{opts: opts, log: opts.Logger}, nil
}

// Handler returns the routed handler.
//
// The stream is served from "/" as well as "/stream" because the PaperTracker
// client requests the bare cached address with no path. That is the whole
// point of the compatibility surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/stream", s.handleStream)
	mux.HandleFunc("/snapshot", s.handleSnapshot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)

	if s.opts.EnableAdmin && s.opts.Controller != nil {
		mux.HandleFunc("/api/v1/config", s.handleConfig)
		mux.HandleFunc("/api/v1/source", s.handleSourceSwitch)
		mux.HandleFunc("/api/v1/devices", s.handleDevices)
	}
	return mux
}

// Listen binds the address. Binding is separate from serving so the caller can
// learn the real address (a port of 0 resolves here) and can treat a bind
// failure as another instance already running.
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// Serve runs until ctx is cancelled, then shuts down gracefully. Stream
// clients hold their connections open forever, so shutdown closes them rather
// than waiting for responses that will never finish.
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

// handleStream writes frames as multipart/x-mixed-replace for as long as the
// client stays connected.
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

	header := w.Header()
	header.Set("Content-Type", s.opts.Encoder.ContentType())
	header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("Expires", "0")
	// Declaring identity encoding is what stops net/http from framing the
	// body as chunked. The client reads the socket looking for our boundary
	// and for Content-Length, so chunk size lines interleaved with the
	// multipart framing would desynchronise its parser.
	header.Set("Transfer-Encoding", "identity")
	header.Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if r.Method == http.MethodHead {
		return
	}

	frames, unsubscribe := s.opts.Hub.Subscribe()
	defer unsubscribe()

	ctx := r.Context()
	buf := make([]byte, 0, streamBufferSize)
	lastFrame := time.Now()

	// Send whatever is current straight away so a reconnecting client sees an
	// image without waiting for the next capture.
	if latest, ok := s.opts.Hub.Latest(); ok {
		buf = s.opts.Encoder.AppendPart(buf[:0], latest.Data)
		if _, err := w.Write(buf); err != nil {
			return
		}
		flusher.Flush()
	}

	// The ticker only exists to notice a source that stopped producing; it
	// does not pace the stream.
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
			buf = s.opts.Encoder.AppendPart(buf[:0], frame.Data)
			if _, err := w.Write(buf); err != nil {
				s.log.Debug("stream client went away", "remote", r.RemoteAddr, "error", err)
				return
			}
			flusher.Flush()
			lastFrame = time.Now()

		case <-watchdog.C:
			if s.opts.HoldOnSourceLoss || time.Since(lastFrame) <= sourceLossTimeout {
				continue
			}
			// Closing prompts the client to reconnect. Holding the connection
			// open instead recovers faster from a brief dropout, which is why
			// the behaviour is configurable.
			s.log.Debug("closing stream after source loss", "remote", r.RemoteAddr)
			return
		}
	}
}

// handleSnapshot returns the most recent frame as a plain JPEG, for debugging
// without an MJPEG-capable client.
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

// Health is the /healthz body.
type Health struct {
	OK     bool            `json:"ok"`
	Reason string          `json:"reason,omitempty"`
	Source status.Snapshot `json:"source"`
}

// handleHealth reports 200 while frames are arriving and 503 otherwise, so a
// supervisor or the tray can tell "running" from "working".
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

// Stats is the /stats body.
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

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, r, http.StatusOK, s.opts.Controller.Snapshot())

	case http.MethodPut:
		var cfg config.Config
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&cfg); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		cfg.Normalise()
		if err := cfg.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.opts.Controller.Apply(r.Context(), cfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, http.StatusOK, s.opts.Controller.Snapshot())

	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSourceSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.opts.Controller.Switch(r.Context(), body.Type); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, r, http.StatusOK, s.opts.Controller.Snapshot())
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	devices, err := s.opts.Controller.Devices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, http.StatusOK, devices)
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
