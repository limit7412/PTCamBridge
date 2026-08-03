package ffmpegfetch

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests never reach the network. Every one of them serves the archive it
// expects from an httptest server, which is also the only way to exercise a
// download that is wrong in a specific way.

const (
	binaryBody = "not really ffmpeg, but the bytes that get installed"
	noticeBody = "GNU LESSER GENERAL PUBLIC LICENSE Version 2.1"
)

// buildArchive returns a zip laid out the way the published one is: everything
// under a single folder named after the version.
func buildArchive(t *testing.T, members map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range members {
		w, err := zw.Create("ffmpeg-n8.1.2-34-gdeadbeef-win64-lgpl-8.1/" + name)
		if err != nil {
			t.Fatalf("add %s to the archive: %v", name, err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}
	return buf.Bytes()
}

func defaultArchive(t *testing.T) []byte {
	t.Helper()
	return buildArchive(t, map[string]string{
		"bin/ffmpeg.exe": binaryBody,
		"bin/ffplay.exe": "an executable that is deliberately not installed",
		"LICENSE.txt":    noticeBody,
	})
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// serve publishes the archive and returns a manager pointed at it, installing
// into a directory of the test's own.
func serve(t *testing.T, archive []byte) (*Manager, string) {
	t.Helper()
	return serveWith(t, archive, digestOf(archive), int64(len(archive)))
}

func serveWith(t *testing.T, archive []byte, digest string, size int64) (*Manager, string) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	return New(Options{
		Dir: dir,
		Build: Build{
			URL:       srv.URL + "/ffmpeg.zip",
			SHA256:    digest,
			Size:      size,
			Publisher: "test",
			License:   "LGPL v2.1 or later",
			Binary:    "bin/ffmpeg.exe",
			Notice:    "LICENSE.txt",
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}), dir
}

func TestFetchInstallsTheBinaryAndItsLicence(t *testing.T) {
	m, dir := serve(t, defaultArchive(t))

	path, err := m.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got := readFile(t, path); got != binaryBody {
		t.Errorf("installed binary = %q, want %q", got, binaryBody)
	}
	if got := readFile(t, filepath.Join(dir, noticeName)); got != noticeBody {
		t.Errorf("installed licence = %q, want %q", got, noticeBody)
	}
	// The other executables in the archive are not ours to install.
	if _, err := os.Stat(filepath.Join(dir, "ffplay.exe")); !os.IsNotExist(err) {
		t.Errorf("ffplay.exe stat error = %v, want it not to be installed", err)
	}
}

func TestFetchLeavesNoArchiveBehind(t *testing.T) {
	m, dir := serve(t, defaultArchive(t))

	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// A hundred-odd megabytes of temporary file is not something to leave in
	// the user's settings folder.
	for _, name := range readDir(t, dir) {
		if strings.HasPrefix(name, "ffmpeg-download-") || strings.Contains(name, ".exe.") {
			t.Errorf("%s was left behind in %s", name, dir)
		}
	}
}

func TestFetchRejectsAnArchiveWithTheWrongDigest(t *testing.T) {
	archive := defaultArchive(t)
	m, dir := serveWith(t, archive, digestOf([]byte("a different archive")), int64(len(archive)))

	_, err := m.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch succeeded, want a digest mismatch")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error = %v, want it to name the digest", err)
	}
	// Nothing may be installed from an archive that failed its check.
	assertEmptyOfInstalls(t, dir)
}

func TestFetchRejectsAnArchiveOfTheWrongLength(t *testing.T) {
	archive := defaultArchive(t)
	// The digest is right for the body served; only the pinned length is not.
	m, dir := serveWith(t, archive, digestOf(archive), int64(len(archive))+10)

	_, err := m.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch succeeded, want a length mismatch")
	}
	if !strings.Contains(err.Error(), "expected") {
		t.Errorf("error = %v, want it to name the expected length", err)
	}
	assertEmptyOfInstalls(t, dir)
}

// A body longer than the pinned length has to fail as a length mismatch rather
// than be truncated to something that could accidentally match.
func TestFetchRejectsABodyLongerThanPinned(t *testing.T) {
	archive := defaultArchive(t)
	m, dir := serveWith(t, append(archive, "trailing rubbish"...), digestOf(archive), int64(len(archive)))

	if _, err := m.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded, want a length mismatch")
	}
	assertEmptyOfInstalls(t, dir)
}

func TestFetchRejectsAnArchiveMissingTheBinary(t *testing.T) {
	archive := buildArchive(t, map[string]string{
		"bin/ffplay.exe": "the wrong executable",
		"LICENSE.txt":    noticeBody,
	})
	m, dir := serve(t, archive)

	_, err := m.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch succeeded, want a missing member")
	}
	if !strings.Contains(err.Error(), "bin/ffmpeg.exe") {
		t.Errorf("error = %v, want it to name the missing member", err)
	}
	assertEmptyOfInstalls(t, dir)
}

// The licence text is installed first precisely so this cannot happen the
// other way round: an ffmpeg on disk without the licence beside it.
func TestFetchInstallsNothingWhenTheLicenceIsMissing(t *testing.T) {
	archive := buildArchive(t, map[string]string{"bin/ffmpeg.exe": binaryBody})
	m, dir := serve(t, archive)

	if _, err := m.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded, want a missing licence")
	}
	assertEmptyOfInstalls(t, dir)
}

func TestFetchFailsOnAnErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := New(Options{
		Dir:   dir,
		Build: Build{URL: srv.URL, SHA256: digestOf(nil), Size: 1, Binary: "bin/ffmpeg.exe", Notice: "LICENSE.txt"},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if _, err := m.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded, want the 404 reported")
	}
	assertEmptyOfInstalls(t, dir)
}

// Cancelling has to stop the download and clean up after it, or a user who
// changes their mind is left with the temporary file anyway.
func TestFetchStopsWhenTheContextIsCancelled(t *testing.T) {
	archive := defaultArchive(t)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		// Enough to have started, then a stall until the test lets go.
		_, _ = w.Write(archive[:10])
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	dir := t.TempDir()
	m := New(Options{
		Dir:   dir,
		Build: Build{URL: srv.URL, SHA256: digestOf(archive), Size: 1000000, Binary: "bin/ffmpeg.exe", Notice: "LICENSE.txt"},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Wait until bytes have actually arrived, so the cancellation lands
		// mid-download rather than before the request was made.
		for i := 0; i < 200; i++ {
			if m.State().Received > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	if _, err := m.Fetch(ctx); err == nil {
		t.Fatal("Fetch succeeded, want the cancellation reported")
	}
	assertEmptyOfInstalls(t, dir)
	for _, name := range readDir(t, dir) {
		if strings.HasPrefix(name, "ffmpeg-download-") {
			t.Errorf("%s was left behind after cancelling", name)
		}
	}
}

func TestFetchReplacesAnEarlierInstall(t *testing.T) {
	m, dir := serve(t, defaultArchive(t))

	path := filepath.Join(dir, binaryName())
	if err := os.WriteFile(path, []byte("a stale copy from an older fetch"), 0o755); err != nil {
		t.Fatalf("seed the old copy: %v", err)
	}

	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := readFile(t, path); got != binaryBody {
		t.Errorf("installed binary = %q, want it replaced with %q", got, binaryBody)
	}
}

func TestStateTracksTheInstallAndTheProgress(t *testing.T) {
	m, dir := serve(t, defaultArchive(t))

	before := m.State()
	if before.Installed || before.Downloading {
		t.Errorf("state before = %+v, want neither installed nor downloading", before)
	}
	if before.Total != m.Build().Size {
		t.Errorf("state total = %d, want %d", before.Total, m.Build().Size)
	}

	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	after := m.State()
	if !after.Installed {
		t.Error("state after a fetch says nothing is installed")
	}
	if after.Path != filepath.Join(dir, binaryName()) {
		t.Errorf("state path = %q, want %q", after.Path, filepath.Join(dir, binaryName()))
	}
	if after.Downloading {
		t.Error("state after a fetch still says downloading")
	}
	if after.Received != after.Total {
		t.Errorf("received = %d, want the whole %d", after.Received, after.Total)
	}
	if after.LastError != "" {
		t.Errorf("last error = %q, want none", after.LastError)
	}
}

func TestStateReportsWhyTheLastAttemptFailed(t *testing.T) {
	archive := defaultArchive(t)
	m, _ := serveWith(t, archive, digestOf([]byte("wrong")), int64(len(archive)))

	if _, err := m.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded, want a digest mismatch")
	}
	if state := m.State(); state.LastError == "" {
		t.Error("state carries no error after a failed fetch")
	}
}

// Both the tray and the API can ask. The second ask while the first is running
// is not another job: it is the same file to the same place.
func TestASecondFetchIsRefusedWhileOneIsRunning(t *testing.T) {
	archive := defaultArchive(t)

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := New(Options{
		Dir: dir,
		Build: Build{
			URL: srv.URL, SHA256: digestOf(archive), Size: int64(len(archive)),
			Binary: "bin/ffmpeg.exe", Notice: "LICENSE.txt",
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	done := make(chan error, 1)
	go func() {
		_, err := m.Fetch(context.Background())
		done <- err
	}()
	<-started

	if _, err := m.Fetch(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("second Fetch error = %v, want ErrBusy", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	// And once it is over, asking again is allowed.
	if _, err := m.Fetch(context.Background()); err != nil {
		t.Errorf("Fetch after the first finished: %v", err)
	}
}

func TestStartRunsInTheBackground(t *testing.T) {
	m, dir := serve(t, defaultArchive(t))

	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return !m.State().Downloading })

	if _, err := os.Stat(filepath.Join(dir, binaryName())); err != nil {
		t.Errorf("nothing installed after Start: %v", err)
	}
}

// The archive names its top folder after the version, so members are matched
// by their trailing path rather than in full.
func TestFindEntryMatchesTheTrailingPath(t *testing.T) {
	archive := defaultArchive(t)
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}

	found, err := findEntry(zr, "bin/ffmpeg.exe")
	if err != nil {
		t.Fatalf("findEntry: %v", err)
	}
	if !strings.HasSuffix(found.Name, "bin/ffmpeg.exe") {
		t.Errorf("found %q, want the bin/ffmpeg.exe member", found.Name)
	}

	// A suffix that only matches part of a path segment is not a match:
	// "mpeg.exe" must not find "ffmpeg.exe".
	if _, err := findEntry(zr, "mpeg.exe"); err == nil {
		t.Error("findEntry matched a partial path segment")
	}
}

func TestPathLivesUnderTheSettingsFolder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(dir, "PaperBridge", "bin", binaryName())
	if path != want {
		t.Errorf("Path() = %q, want %q", path, want)
	}

	if _, ok := Installed(); ok {
		t.Error("Installed() is true with nothing on disk")
	}
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatalf("create the folder: %v", err)
	}
	if err := os.WriteFile(want, []byte("ffmpeg"), 0o755); err != nil {
		t.Fatalf("write the binary: %v", err)
	}
	if got, ok := Installed(); !ok || got != want {
		t.Errorf("Installed() = %q, %v; want %q, true", got, ok, want)
	}
}

// The pinned build is the one thing here that cannot be tested against a
// server, so at least check it is internally consistent: a digest and a length
// that are obviously unset would disable verification altogether.
func TestPinnedBuildIsFullyPinned(t *testing.T) {
	b := Pinned()
	if len(b.SHA256) != 64 {
		t.Errorf("pinned digest %q is not a sha256", b.SHA256)
	}
	if b.Size <= 0 {
		t.Errorf("pinned size = %d, want the archive length", b.Size)
	}
	if !strings.HasPrefix(b.URL, "https://") {
		t.Errorf("pinned URL %q is not https", b.URL)
	}
	// A rolling tag is rebuilt under the same URL, which would make the digest
	// wrong within a day.
	if strings.Contains(b.URL, "/latest/") {
		t.Errorf("pinned URL %q points at a rolling tag", b.URL)
	}
	if !strings.Contains(b.URL, "lgpl") {
		t.Errorf("pinned URL %q is not the LGPL build", b.URL)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertEmptyOfInstalls(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{binaryName(), noticeName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failed fetch (stat error %v)", name, err)
		}
	}
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}

// Shutting the bridge down has to take an in-flight download with it, or the
// process stays alive pulling a hundred megabytes nobody is waiting for.
func TestStartStopsWhenTheLifetimeEnds(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte("the beginning of an archive"))
		w.(http.Flusher).Flush()
		<-release
	}))
	// Closing the server waits for the handler, so the handler has to be let
	// go first.
	defer srv.Close()
	defer close(release)

	lifetime, stop := context.WithCancel(context.Background())
	dir := t.TempDir()
	m := New(Options{
		Lifetime: lifetime,
		Dir:      dir,
		Build:    Build{URL: srv.URL, SHA256: digestOf(nil), Size: 1000000, Binary: "bin/ffmpeg.exe", Notice: "LICENSE.txt"},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return m.State().Received > 0 })
	stop()
	waitFor(t, func() bool { return !m.State().Downloading })

	if state := m.State(); state.LastError == "" {
		t.Error("a cancelled download reports no error")
	}
	assertEmptyOfInstalls(t, dir)
}
