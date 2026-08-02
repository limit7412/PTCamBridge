// Package server publishes the bridged camera as the MJPEG-over-HTTP stream
// the PaperTracker client expects, plus the status and management endpoints.
package server

import (
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

// writeTimeout bounds a single frame write to one client.
//
// The response as a whole is endless, so the server has no WriteTimeout; that
// leaves a client which stops reading able to block the write forever once the
// socket buffer fills, and a blocked write reaches neither the watchdog nor
// the cancelled context. A frame that cannot be handed over in this long says
// the client is gone whether or not it has closed the connection.
const writeTimeout = 5 * time.Second

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
	Hub    *hub.Hub
	Status *status.Tracker
	// Encoder is the initial wire format; SetStreamOptions replaces it.
	Encoder core.MultipartEncoder
	Logger  *slog.Logger
	// Controller enables the management API. It is ignored unless
	// EnableAdmin is also set.
	Controller Controller
	// EnableAdmin serves /api/v1/*. The bridge turns this off whenever the
	// listener is not on loopback, because the API has no authentication.
	EnableAdmin bool
	// HoldOnSourceLoss keeps stream clients connected while the camera
	// reconnects rather than closing the response. It is the initial value;
	// SetStreamOptions replaces it.
	HoldOnSourceLoss bool
	Version          string
}

// streamOptions are the response settings a running server can swap out.
type streamOptions struct {
	encoder core.MultipartEncoder
	hold    bool
}

// Server serves the stream and status endpoints.
type Server struct {
	opts Options
	log  *slog.Logger

	// stream is replaced wholesale when the settings change, so a client that
	// is already connected keeps the wire format it started parsing.
	stream atomic.Pointer[streamOptions]
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
	s := &Server{opts: opts, log: opts.Logger}
	s.stream.Store(&streamOptions{encoder: opts.Encoder, hold: opts.HoldOnSourceLoss})
	return s, nil
}

// SetStreamOptions adopts a new wire format for the streams started from here
// on. The bridge calls it after a settings change so that adjusting the
// boundary or the extra headers -- the knobs that exist purely to match a
// PaperTracker release -- does not need a restart to take effect.
func (s *Server) SetStreamOptions(encoder core.MultipartEncoder, holdOnSourceLoss bool) {
	s.stream.Store(&streamOptions{encoder: encoder, hold: holdOnSourceLoss})
	s.log.Info("stream settings updated", "boundary", encoder.Boundary(), "hold_on_source_loss", holdOnSourceLoss)
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

	// Read the settings once: the boundary announced in the Content-Type has
	// to be the one every part of this response then uses.
	stream := s.stream.Load()

	header := w.Header()
	header.Set("Content-Type", stream.encoder.ContentType())
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

	// A deadline per frame is the only thing that unblocks a write to a client
	// that has stopped reading. Not every ResponseWriter supports one; where it
	// does not, the behaviour is what it was before.
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

	// Send whatever is current straight away so a reconnecting client sees an
	// image without waiting for the next capture. A frame older than the loss
	// timeout is withheld: the hub keeps the last image indefinitely, and
	// replaying it to every reconnect while the camera is down would feed the
	// tracker a stale mouth shape over and over.
	if latest, ok := s.opts.Hub.Latest(); ok && time.Since(latest.RecvedAt) <= sourceLossTimeout {
		buf = stream.encoder.AppendPart(buf[:0], latest.Data)
		if err := writeFrame(buf); err != nil {
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

// guardAdmin rejects management API requests that a web page could have caused
// the browser to send.
//
// Binding to loopback is not on its own a control: any page the user visits can
// reach 127.0.0.1, and a form-style POST needs no preflight to do it. Reaching
// this API is the whole authority it has, so unless a request proves it did not
// come from a foreign page it does not get to change the source.
func (s *Server) guardAdmin(w http.ResponseWriter, r *http.Request) bool {
	// A rebound DNS name resolves to loopback but still carries its own name
	// in Host, which is what makes the bind address meaningful again.
	if !isLoopbackHost(r.Host) {
		http.Error(w, "the management API only answers requests addressed to loopback", http.StatusForbidden)
		return false
	}
	// Browsers attach Origin to every cross-site request. Command line clients
	// send none at all, which is why an absent header is allowed through.
	if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
		http.Error(w, "cross-origin requests are not accepted", http.StatusForbidden)
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	// text/plain, form and multipart are the body types a page can post
	// without a preflight. Insisting on JSON is what forces the preflight,
	// which the Origin check above then fails.
	if !hasJSONBody(r) {
		http.Error(w, "Content-Type: application/json is required", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// isLoopbackHost reports whether an authority names this machine. The port is
// irrelevant and a missing one is fine.
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

// isLoopbackOrigin reports whether an Origin header value is a page served from
// this machine.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Host)
}

// hasJSONBody reports whether the request declares a JSON body.
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
		var cfg config.Config
		if err := decodeStrict(http.MaxBytesReader(w, r.Body, 1<<20), &cfg); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		cfg.Normalise()
		if err := cfg.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.opts.Controller.Apply(r.Context(), cfg); err != nil {
			http.Error(w, err.Error(), applyStatus(err))
			return
		}
		writeJSON(w, r, http.StatusOK, s.opts.Controller.Snapshot())

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
	// A body of {} decodes cleanly into an empty type, and empty is not a
	// missing value further down: Normalise reads it as "unset" and fills in
	// the default, so a request with no type at all would quietly move a
	// working serial or MJPEG source onto UVC.
	if body.Type == "" {
		http.Error(w, `"type" is required`, http.StatusBadRequest)
		return
	}
	if err := s.opts.Controller.Switch(r.Context(), body.Type); err != nil {
		http.Error(w, err.Error(), applyStatus(err))
		return
	}
	writeJSON(w, r, http.StatusOK, s.opts.Controller.Snapshot())
}

// applyStatus maps a settings failure onto a status code. A change that took
// effect but could not be written to disk is the one case where the request
// was fine and this side failed, so it is the only one that is not a 400.
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
	devices, err := s.opts.Controller.Devices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, http.StatusOK, devices)
}

// decodeStrict rejects a body carrying fields the target does not have, or
// anything at all after the first JSON value.
//
// The settings file already refuses unknown keys; without the same rule here a
// misspelled field over the API is silently dropped, the value it was meant to
// set stays at its zero value, and the caller gets a 200 for a change that did
// something other than what it asked for.
//
// The trailing check is the same argument one step out. A body of
// {"type":"uvc"}{"type":"mjpeg"} is not a request with a typo in it, it is two
// requests; decoding only the first and reporting success tells the caller the
// second one was honoured.
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
