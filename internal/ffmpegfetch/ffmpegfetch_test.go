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

// テストはネットワークに出ない。どのテストも、期待するアーカイブを httptest の
// サーバから配る。特定の壊れ方をしたダウンロードを再現する唯一の方法でもある。

const (
	binaryBody = "not really ffmpeg, but the bytes that get installed"
	noticeBody = "GNU LESSER GENERAL PUBLIC LICENSE Version 2.1"
)

// buildArchive は、公開されているものと同じ配置の zip を返す。すべてがバージョン名の
// フォルダ 1 つの下に入る。
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

// serve はアーカイブを公開し、それを指す manager を返す。設置先はテスト自身の
// ディレクトリ。
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
	// アーカイブに含まれる他の実行ファイルは、こちらが設置すべきものではない。
	if _, err := os.Stat(filepath.Join(dir, "ffplay.exe")); !os.IsNotExist(err) {
		t.Errorf("ffplay.exe stat error = %v, want it not to be installed", err)
	}
}

func TestFetchLeavesNoArchiveBehind(t *testing.T) {
	m, dir := serve(t, defaultArchive(t))

	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// 100 メガバイト強の一時ファイルは、ユーザーの設定フォルダに残してよいもの
	// ではない。
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
	// 検査に失敗したアーカイブからは、何も設置してはならない。
	assertEmptyOfInstalls(t, dir)
}

func TestFetchRejectsAnArchiveOfTheWrongLength(t *testing.T) {
	archive := defaultArchive(t)
	// ダイジェストは配った本体に対して正しい。合っていないのは固定した長さだけ。
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

// 固定した長さより長い本体は、たまたま一致し得る何かに切り詰められるのではなく、
// 長さの不一致として失敗しなければならない。
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

// ライセンス本文を先に設置するのは、まさにその逆が起きないようにするため。
// 隣にライセンスの無い ffmpeg がディスク上にある、という状態。
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

// キャンセルはダウンロードを止め、後片付けもしなければならない。さもないと気が
// 変わったユーザーの手元に、結局一時ファイルが残る。
func TestFetchStopsWhenTheContextIsCancelled(t *testing.T) {
	archive := defaultArchive(t)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		// 開始したと言える程度だけ送り、その後はテストが解放するまで止まる。
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
		// 実際にバイトが届くまで待つ。キャンセルが、リクエスト前ではなく
		// ダウンロードの途中に落ちるようにするため。
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

// トレイからも API からも要求できる。1 つ目が走っている最中の 2 つ目の要求は別の
// 仕事ではない。同じ場所への同じファイルだ。
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
	// そして終わってしまえば、また要求できる。
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

// アーカイブは最上位フォルダをバージョンにちなんで命名するので、要素は全体では
// なく末尾のパスで照合する。
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

	// パス要素の一部にしか一致しない接尾辞は一致とみなさない。"mpeg.exe" が
	// "ffmpeg.exe" を見つけてはいけない。
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
	want := filepath.Join(dir, "PTCamBridge", "bin", binaryName())
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

// 固定したビルドは、ここでサーバ相手に検証できない唯一のもの。せめて内部の
// 整合性だけは確認する。明らかに未設定のダイジェストと長さは、検証そのものを
// 無効にしてしまう。
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
	// 転がり続けるタグは同じ URL の下で作り直されるので、ダイジェストは 1 日で
	// 誤りになる。
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

// ブリッジの停止は、進行中のダウンロードも道連れにしなければならない。さもないと
// プロセスは、誰も待っていない 100 メガバイトを引きながら生き続ける。
func TestStartStopsWhenTheLifetimeEnds(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte("the beginning of an archive"))
		w.(http.Flusher).Flush()
		<-release
	}))
	// サーバを閉じるとハンドラを待つので、先にハンドラを解放しなければならない。
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

// 応答した後に黙り込むサーバは、バイトもエラーも生まない。監視が無ければ
// ダウンロードはブリッジが終了するまで固まり、メニューは "Downloading..." のまま、
// 再試行はすべて実行中として拒否される。
func TestFetchGivesUpWhenTheServerStopsSending(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte("the beginning of an archive"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	dir := t.TempDir()
	m := New(Options{
		Dir:          dir,
		StallTimeout: 100 * time.Millisecond,
		Build:        Build{URL: srv.URL, SHA256: digestOf(nil), Size: 1000000, Binary: "bin/ffmpeg.exe", Notice: "LICENSE.txt"},
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	_, err := m.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch succeeded, want the stall reported")
	}
	if !strings.Contains(err.Error(), "nothing received") {
		t.Errorf("error = %v, want it to say the transfer stalled", err)
	}
	assertEmptyOfInstalls(t, dir)
	for _, name := range readDir(t, dir) {
		if strings.HasPrefix(name, "ffmpeg-download-") {
			t.Errorf("%s was left behind after a stall", name)
		}
	}
	// そして manager は、ダウンロード中と報告し続けたまま固まるのではなく、
	// 再び自由になる。
	if state := m.State(); state.Downloading {
		t.Error("still reporting a download after the stall")
	}
}

// 細く遅い流れは停滞ではない。遅いだけで機能しているダウンロードを見捨てることは、
// この監視が防ごうとしている固まりよりも悪い。
func TestFetchToleratesASlowButMovingDownload(t *testing.T) {
	archive := defaultArchive(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < len(archive); i += 64 {
			end := min(i+64, len(archive))
			_, _ = w.Write(archive[i:end])
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := New(Options{
		Dir: dir,
		// 転送全体にかかる時間よりはるかに短くする。監視が合計ではなく読み取りの
		// 間隔を測っている場合にだけ、これは通る。
		StallTimeout: 200 * time.Millisecond,
		Build: Build{
			URL: srv.URL, SHA256: digestOf(archive), Size: int64(len(archive)),
			Binary: "bin/ffmpeg.exe", Notice: "LICENSE.txt",
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

// 停止処理は、転送だけでなく後片付けよりも長く生きなければならない。
func TestWaitReturnsOnlyAfterTheDownloadHasTidiedUp(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte("the beginning of an archive"))
		w.(http.Flusher).Flush()
		<-release
	}))
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Wait(ctx)
	if ctx.Err() != nil {
		t.Fatal("Wait timed out")
	}

	// 待つことの意味はここ。返ってきた時点で、途中まで落としたアーカイブは消えて
	// いる。
	for _, name := range readDir(t, dir) {
		if strings.HasPrefix(name, "ffmpeg-download-") {
			t.Errorf("%s was still there when Wait returned", name)
		}
	}
}

// 何もしていない manager を待てば、すぐ返る。
func TestWaitReturnsImmediatelyWithNoDownload(t *testing.T) {
	m, _ := serve(t, defaultArchive(t))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Wait(ctx)
	if ctx.Err() != nil {
		t.Error("Wait blocked with no download running")
	}
}
