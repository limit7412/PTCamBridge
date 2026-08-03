// Package ffmpegfetch downloads the ffmpeg binary the UVC source needs, from
// the people who built it, onto the machine that will run it.
//
// PaperBridge does not ship ffmpeg. It starts ffmpeg as a child process, so the
// two are separate programs and PaperBridge's own licence is unaffected either
// way -- but putting a copy in the release would make this project a
// redistributor of an LGPL binary, with the source-availability duties that
// carries, and would put a hundred-odd megabytes in front of every user
// including the ones bridging a serial board who never need it. Fetching on
// request keeps both away: the bytes go from the publisher to the user, and
// only when the user asks for them.
//
// Nothing here starts on its own. Every download begins with an explicit user
// action, which is also why the archive is described -- publisher, size,
// licence -- in terms a person can be shown before agreeing to it.
package ffmpegfetch

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
)

// Build describes one published archive precisely enough to verify it.
type Build struct {
	// URL is the archive, at the publisher's own download location. It is
	// never mirrored: a copy served from somewhere of ours would make this
	// project the distributor, which is the thing fetching exists to avoid.
	URL string `json:"url"`
	// SHA256 is the hex digest of the whole archive.
	SHA256 string `json:"sha256"`
	// Size is the archive length in bytes, so the user can be told what the
	// download costs before it starts and a wrong body can be cut off early.
	Size int64 `json:"size"`
	// Publisher names who produced the build, for the same reason.
	Publisher string `json:"publisher"`
	// License is what the fetched binary is covered by.
	License string `json:"license"`
	// Binary is the archive member holding the executable, matched by suffix
	// because the archive's top folder carries the version in its name.
	Binary string `json:"-"`
	// Notice is the archive member holding the licence text. It is installed
	// beside the executable so the copy on disk says what it is.
	Notice string `json:"-"`
}

// pinned is the build a fetch installs.
//
// A dated tag rather than the publisher's rolling "latest": that one is
// rebuilt daily under the same URL, so no digest pinned to it stays true for
// longer than a day, and a download that cannot be checked against a digest
// fixed in advance is not verified at all -- it only proves the bytes arrived
// intact from whoever answered.
//
// The LGPL variant rather than the GPL one, and the static build rather than
// the shared: the static archive is larger to download but installs as one
// self-contained file, with no set of DLLs to keep together, and a single file
// is something the user can move or delete without leaving a half-working
// installation behind.
var pinned = Build{
	URL:       "https://github.com/BtbN/FFmpeg-Builds/releases/download/autobuild-2026-08-02-13-17/ffmpeg-n8.1.2-34-g9b6c8969e0-win64-lgpl-8.1.zip",
	SHA256:    "1c17a2af80ca4f85e3e72a1137eb4645f8a88c2e2d754339e270b1f234f8d49c",
	Size:      145349145,
	Publisher: "BtbN/FFmpeg-Builds",
	License:   "LGPL v2.1 or later",
	Binary:    "bin/ffmpeg.exe",
	Notice:    "LICENSE.txt",
}

// Pinned returns the build a fetch installs.
func Pinned() Build { return pinned }

// Supported reports whether a build is published for this platform.
//
// Only Windows: it is the platform the PaperTracker client ships for, and the
// one where ffmpeg is not a package manager away.
func Supported() bool { return runtime.GOOS == "windows" }

// noticeName is what the licence text is installed as. It keeps the ffmpeg
// prefix so a user looking in the folder can tell whose licence it is.
const noticeName = "ffmpeg-LICENSE.txt"

// Dir is where a fetched ffmpeg is installed: a folder of our own under the
// settings directory, so a fetch never writes next to the executable (which
// may sit somewhere the user cannot write) and never touches PATH.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bin"), nil
}

// Path is where a fetched ffmpeg ends up.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, binaryName()), nil
}

// Installed returns the path to a previously fetched ffmpeg, and whether there
// is one.
func Installed() (string, bool) {
	path, err := Path()
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

// ErrBusy means a download is already running. A second one would fetch the
// same bytes to the same place.
var ErrBusy = errors.New("ffmpegfetch: a download is already in progress")

// State is what the tray and the management API show about the download.
type State struct {
	// Installed is whether a fetched ffmpeg is on disk right now.
	Installed bool `json:"installed"`
	// Path is where it is, when it is.
	Path string `json:"path,omitempty"`
	// Downloading is whether a fetch is running.
	Downloading bool `json:"downloading"`
	// Received and Total track the current download in bytes.
	Received int64 `json:"received_bytes"`
	Total    int64 `json:"total_bytes"`
	// LastError is why the last attempt failed, empty if it did not.
	LastError string `json:"last_error,omitempty"`
	// Source describes what would be, or was, downloaded.
	Source Build `json:"source"`
	// Supported is whether a build is published for this platform at all.
	Supported bool `json:"supported"`
}

// Options configures a Manager.
type Options struct {
	// Lifetime bounds background downloads. It is the application's context,
	// not a request's: a download runs for minutes and outlives the click or
	// the HTTP call that asked for it, while stopping the bridge has to stop
	// it. Nil means downloads are only stopped by finishing.
	Lifetime context.Context
	// Dir overrides the install location. Empty uses Dir().
	Dir string
	// Build overrides the archive to fetch. The zero value uses Pinned().
	Build Build
	// Client overrides the HTTP client. Downloads are large and slow, so the
	// default has no overall timeout; cancellation comes from the context.
	Client *http.Client
	Log    *slog.Logger
}

// Manager owns the one download that may be in flight, and the state the tray
// and the API read.
//
// One at a time, and one place holding the answer: both entry points ask for
// the same file in the same location, so a second request while the first runs
// is not another job to do.
type Manager struct {
	build Build
	// isPinned records that the build came from Pinned() rather than from the
	// caller. Only that one is tied to a platform: a caller naming its own
	// archive knows what it is asking for.
	isPinned bool
	dir      string
	client   *http.Client
	log      *slog.Logger
	// lifetime is held rather than passed in because the work it bounds is not
	// a call: Start hands the download to a goroutine and returns, so there is
	// no call left to carry a context by the time the bytes are moving.
	lifetime context.Context

	mu          sync.Mutex
	downloading bool
	received    int64
	lastError   string
}

// New builds a Manager. It does not touch the disk or the network.
func New(opts Options) *Manager {
	m := &Manager{
		build:    opts.Build,
		dir:      opts.Dir,
		client:   opts.Client,
		log:      opts.Log,
		lifetime: opts.Lifetime,
	}
	if m.lifetime == nil {
		m.lifetime = context.Background()
	}
	if m.build.URL == "" {
		m.build = Pinned()
		m.isPinned = true
	}
	if m.client == nil {
		// No client timeout: this is a hundred-odd megabytes over whatever
		// connection the user has, and a deadline that fits a normal request
		// would abandon a download that is working. The transport timeouts
		// below still cover a connection that stops talking, and the context
		// covers the user changing their mind.
		m.client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}}
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	return m
}

// Build is what this manager would download.
func (m *Manager) Build() Build { return m.build }

// State reports what the UI should show.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := State{
		Downloading: m.downloading,
		Received:    m.received,
		Total:       m.build.Size,
		LastError:   m.lastError,
		Source:      m.build,
		Supported:   Supported(),
	}
	if path, err := m.path(); err == nil {
		if _, statErr := os.Stat(path); statErr == nil {
			state.Installed = true
			state.Path = path
		}
	}
	return state
}

// Start runs a fetch in the background and returns once it is under way.
//
// Callers watch State to see how it goes: the download is measured in minutes,
// which is longer than a menu click or an HTTP request should be held open for.
func (m *Manager) Start() error {
	if err := m.begin(); err != nil {
		return err
	}
	go func() {
		path, err := m.download(m.lifetime)
		m.finish(err)
		if err != nil {
			m.log.Error("could not fetch ffmpeg", "error", err)
			return
		}
		m.log.Info("ffmpeg fetched", "path", path, "source", m.build.URL)
	}()
	return nil
}

// Fetch downloads and installs, returning the path to the binary. It is what
// Start runs, exposed for callers that want to wait.
func (m *Manager) Fetch(ctx context.Context) (string, error) {
	if err := m.begin(); err != nil {
		return "", err
	}
	path, err := m.download(ctx)
	m.finish(err)
	return path, err
}

func (m *Manager) begin() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downloading {
		return ErrBusy
	}
	if m.isPinned && !Supported() {
		return fmt.Errorf("ffmpegfetch: no published build for %s; install ffmpeg yourself and set source.uvc.ffmpeg_path", runtime.GOOS)
	}
	m.downloading = true
	m.received = 0
	m.lastError = ""
	return nil
}

func (m *Manager) finish(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downloading = false
	if err != nil {
		m.lastError = err.Error()
	}
}

func (m *Manager) progress(received int64) {
	m.mu.Lock()
	m.received = received
	m.mu.Unlock()
}

func (m *Manager) path() (string, error) {
	if m.dir != "" {
		return filepath.Join(m.dir, binaryName()), nil
	}
	return Path()
}

func (m *Manager) installDir() (string, error) {
	if m.dir != "" {
		return m.dir, nil
	}
	return Dir()
}

// download fetches the archive, checks it against the pinned digest and
// installs what is needed out of it.
//
// The archive is written to a file rather than held in memory: it is larger
// than this process is meant to occupy, and reading a zip needs to seek.
func (m *Manager) download(ctx context.Context) (string, error) {
	dir, err := m.installDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("ffmpegfetch: create %s: %w", dir, err)
	}

	archive, err := m.fetchArchive(ctx, dir)
	if err != nil {
		return "", err
	}
	defer func() {
		archive.Close()
		os.Remove(archive.Name())
	}()

	return m.install(archive, dir)
}

// fetchArchive downloads to a temporary file next to the install location and
// verifies the digest. The returned file is positioned at the start.
func (m *Manager) fetchArchive(ctx context.Context, dir string) (*os.File, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.build.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("ffmpegfetch: build the request: %w", err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ffmpegfetch: download %s: %w", m.build.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ffmpegfetch: download %s: %s", m.build.URL, resp.Status)
	}

	tmp, err := os.CreateTemp(dir, "ffmpeg-download-*.zip")
	if err != nil {
		return nil, fmt.Errorf("ffmpegfetch: create a temporary file in %s: %w", dir, err)
	}
	// Removed on every path but the successful one, where the caller takes
	// over: a hundred megabytes left behind by a failure is not a small mess.
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	digest := sha256.New()
	// One byte past the expected length, so a body that is too long is caught
	// rather than silently truncated to a size that then fails the digest with
	// a less useful message.
	body := io.LimitReader(resp.Body, m.build.Size+1)
	written, err := io.Copy(io.MultiWriter(tmp, digest), &progressReader{r: body, report: m.progress})
	if err != nil {
		return nil, fmt.Errorf("ffmpegfetch: download %s: %w", m.build.URL, err)
	}
	if written != m.build.Size {
		return nil, fmt.Errorf("ffmpegfetch: %s is %d bytes, expected %d", m.build.URL, written, m.build.Size)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, m.build.SHA256) {
		return nil, fmt.Errorf("ffmpegfetch: %s has digest %s, expected %s", m.build.URL, got, m.build.SHA256)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("ffmpegfetch: rewind %s: %w", tmp.Name(), err)
	}

	keep = true
	return tmp, nil
}

// install extracts the binary and the licence text.
//
// The licence lands first and the binary last, because the binary is what
// everything else tests for: finishing in that order means an ffmpeg that is
// found is an ffmpeg whose licence text is already sitting beside it.
func (m *Manager) install(archive *os.File, dir string) (string, error) {
	info, err := archive.Stat()
	if err != nil {
		return "", fmt.Errorf("ffmpegfetch: stat the archive: %w", err)
	}
	zr, err := zip.NewReader(archive, info.Size())
	if err != nil {
		return "", fmt.Errorf("ffmpegfetch: read the archive: %w", err)
	}

	binary, err := findEntry(zr, m.build.Binary)
	if err != nil {
		return "", err
	}
	notice, err := findEntry(zr, m.build.Notice)
	if err != nil {
		return "", err
	}

	if err := extract(notice, filepath.Join(dir, noticeName), 0o644); err != nil {
		return "", err
	}
	path := filepath.Join(dir, binaryName())
	if err := extract(binary, path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// findEntry locates an archive member by its trailing path. The archive's top
// folder is named after the build, so the version would otherwise have to be
// repeated in the member paths and kept in step with the URL.
func findEntry(zr *zip.Reader, suffix string) (*zip.File, error) {
	if suffix == "" {
		return nil, errors.New("ffmpegfetch: no archive member named")
	}
	want := "/" + path.Clean(suffix)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// Slash-separated by the zip format itself, whatever the platform.
		if name := path.Clean(f.Name); name == path.Clean(suffix) || strings.HasSuffix(name, want) {
			return f, nil
		}
	}
	return nil, fmt.Errorf("ffmpegfetch: the archive has no %s", suffix)
}

// extract writes one member into place, replacing whatever was there.
//
// Through a temporary file and a rename, so a fetch that dies part way cannot
// leave a truncated ffmpeg.exe behind: that file would be found by the source
// driver and fail as something other than "not installed".
func extract(f *zip.File, dest string, mode os.FileMode) error {
	src, err := f.Open()
	if err != nil {
		return fmt.Errorf("ffmpegfetch: read %s from the archive: %w", f.Name, err)
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*")
	if err != nil {
		return fmt.Errorf("ffmpegfetch: create a temporary file for %s: %w", dest, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("ffmpegfetch: write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ffmpegfetch: close %s: %w", tmp.Name(), err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("ffmpegfetch: chmod %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("ffmpegfetch: rename onto %s: %w", dest, err)
	}
	return nil
}

// progressReader reports how much has arrived so the tray can show something
// during a download measured in minutes.
type progressReader struct {
	r      io.Reader
	report func(int64)
	total  int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.total += int64(n)
		p.report(p.total)
	}
	return n, err
}
