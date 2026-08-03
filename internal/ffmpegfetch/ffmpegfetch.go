// Package ffmpegfetch は、UVC ソースが必要とする ffmpeg のバイナリを、それを
// 作った人たちのところから、それを動かす機械へダウンロードします。
//
// PTCamBridge は ffmpeg を同梱していません。ffmpeg は子プロセスとして起動するので
// 両者は別のプログラムであり、PTCamBridge 自身のライセンスはどちらにせよ影響を
// 受けません。ただしリリースにコピーを入れれば、このプロジェクトは LGPL のバイナリの
// 再配布者になり、それに伴うソース提供の義務を負いますし、100 メガバイト強を、
// シリアルボードを中継するだけで一度も必要としないユーザーも含めた全員の前に
// 置くことになります。要求されたときに取ってくれば、そのどちらも避けられます。
// バイトは公開元からユーザーへ、ユーザーが求めたときにだけ流れます。
//
// ここで何かが自発的に始まることはありません。すべてのダウンロードは明示的な
// ユーザー操作から始まります。アーカイブを — 公開元、大きさ、ライセンス — 人に
// 提示できる言葉で記述しているのも、同意する前に見せるためです。
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
	"sync/atomic"
	"time"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/i18n"
)

// Prompt は、何かがダウンロードされる前にユーザーへ見せる内容です。
//
// PTCamBridge は ffmpeg を同梱していないので、取得を選ぶとユーザーの機械が第三者の
// バイナリを取ってくることになります。誰が作ったのか、どこから来るのか、どれくらいの
// 大きさか、どのライセンスなのか。同意するために必要なのはこの 4 つなので、README では
// なく、最初の 1 バイトが動く前の画面に出します。
//
// これは仕組みそのものの成立条件でもあります。上流から直接取ること、改変して配らない
// こと、そしてユーザーの明示的な操作を起点にし、取得元とライセンスを提示してから実行
// すること。パッケージのコメントを参照。
//
// ここに置いているのは、取得の入口が 2 つ — トレイと設定画面 — あるからです。片方だけが
// 提示する形にすると、もう片方から入ったユーザーは何も知らされずにダウンロードが始まる
// ことになります。
func Prompt(p i18n.Printer, build Build) string {
	return p.F(i18n.DialogFFmpegBody,
		build.Publisher, build.URL, build.Size/(1000*1000), build.License)
}

// Build は、公開されたアーカイブ 1 つを、検証できる程度に正確に記述します。
type Build struct {
	// URL は、公開元自身のダウンロード場所にあるアーカイブです。ミラーは決して
	// しません。こちらのどこかから配るコピーは、このプロジェクトを配布者にして
	// しまいます。取得という仕組みは、まさにそれを避けるために存在します。
	URL string `json:"url"`
	// SHA256 は、アーカイブ全体の 16 進ダイジェストです。
	SHA256 string `json:"sha256"`
	// Size はアーカイブの長さ (バイト) です。開始前にダウンロードの負担をユーザーに
	// 伝えられますし、誤った本体を早い段階で打ち切れます。
	Size int64 `json:"size"`
	// Publisher は、そのビルドを作ったのが誰かを示します。理由は同じです。
	Publisher string `json:"publisher"`
	// License は、取得したバイナリが従うライセンスです。
	License string `json:"license"`
	// Binary は、実行ファイルを収めたアーカイブ内の要素です。接尾辞で照合します。
	// アーカイブの最上位フォルダの名前にバージョンが入っているためです。
	Binary string `json:"-"`
	// Notice は、ライセンス本文を収めたアーカイブ内の要素です。実行ファイルの隣に
	// 設置するので、ディスク上のコピーが自分が何であるかを述べます。
	Notice string `json:"-"`
}

// pinned は、取得が設置するビルドです。
//
// 公開元の転がり続ける "latest" ではなく日付入りのタグにしています。あちらは同じ
// URL の下で毎日作り直されるので、それに固定したダイジェストは 1 日ともちません。
// 事前に固定したダイジェストと突き合わせられないダウンロードは、検証されていないのと
// 同じです。応答してきた相手が何であれ、そこからバイトが無傷で届いたことを示すだけです。
//
// GPL 版ではなく LGPL 版、共有ビルドではなく静的ビルドを選んでいます。静的
// アーカイブはダウンロードこそ大きいものの、一揃いの DLL を伴わない自己完結した
// ファイル 1 つとして設置されます。ファイルが 1 つなら、ユーザーは中途半端に動く
// インストールを残すことなく、それを移動したり削除したりできます。
var pinned = Build{
	URL:       "https://github.com/BtbN/FFmpeg-Builds/releases/download/autobuild-2026-08-02-13-17/ffmpeg-n8.1.2-34-g9b6c8969e0-win64-lgpl-8.1.zip",
	SHA256:    "1c17a2af80ca4f85e3e72a1137eb4645f8a88c2e2d754339e270b1f234f8d49c",
	Size:      145349145,
	Publisher: "BtbN/FFmpeg-Builds",
	License:   "LGPL v2.1 or later",
	Binary:    "bin/ffmpeg.exe",
	Notice:    "LICENSE.txt",
}

// Pinned は、取得が設置するビルドを返します。
func Pinned() Build { return pinned }

// Supported は、このプラットフォーム向けのビルドが公開されているかを返します。
//
// Windows だけです。PaperTracker クライアントが配布されているプラットフォームであり、
// かつ ffmpeg がパッケージマネージャ 1 つで手に入らない環境だからです。
func Supported() bool { return runtime.GOOS == "windows" }

// noticeName は、ライセンス本文を設置する際のファイル名です。ffmpeg の接頭辞を
// 残しているので、フォルダを覗いたユーザーが誰のライセンスかを判別できます。
const noticeName = "ffmpeg-LICENSE.txt"

// Dir は、取得した ffmpeg を設置する場所です。設定ディレクトリの下にある自前の
// フォルダなので、取得が実行ファイルの隣 (ユーザーが書き込めない場所にあるかも
// しれません) に書くことも、PATH に触れることもありません。
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bin"), nil
}

// Path は、取得した ffmpeg が最終的に置かれる場所です。
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, binaryName()), nil
}

// Installed は、以前に取得した ffmpeg のパスと、それが存在するかどうかを返します。
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

// ErrBusy は、ダウンロードが既に走っていることを表します。2 つ目は同じバイトを
// 同じ場所へ取ってくるだけです。
var ErrBusy = errors.New("ffmpegfetch: a download is already in progress")

// defaultStallTimeout は、諦めるまでに転送が何も受け取らずにいられる時間です。
//
// 余裕を持たせています。見ているのはスループットではないからです。悪い回線越しの
// ダウンロードは這うように遅くても機能していることがあり、それを見捨てるのは誤りです。
// 捕まえたいのはもう一方 — 応答した後に黙り込むサーバや中間装置 — で、それはバイトも
// エラーも生まないまま、そうしなければブリッジが終了するまでダウンロードを開いた
// ままにします。
const defaultStallTimeout = 60 * time.Second

// State は、トレイと管理 API がダウンロードについて表示する内容です。
type State struct {
	// Installed は、取得した ffmpeg が今ディスク上にあるかどうかです。
	Installed bool `json:"installed"`
	// Path は、それがある場合の場所です。
	Path string `json:"path,omitempty"`
	// Downloading は、取得が走っているかどうかです。
	Downloading bool `json:"downloading"`
	// Received と Total は、現在のダウンロードをバイト単位で追います。
	Received int64 `json:"received_bytes"`
	Total    int64 `json:"total_bytes"`
	// LastError は、直前の試行が失敗した理由です。失敗していなければ空です。
	LastError string `json:"last_error,omitempty"`
	// Source は、ダウンロードされる、あるいはされたものの説明です。
	Source Build `json:"source"`
	// Supported は、そもそもこのプラットフォーム向けのビルドが公開されているか
	// どうかです。
	Supported bool `json:"supported"`
}

// Options は Manager を設定します。
type Options struct {
	// Lifetime は背後のダウンロードの生存期間を区切ります。これはリクエストの
	// コンテキストではなくアプリケーションのコンテキストです。ダウンロードは数分
	// 走り、それを求めたクリックや HTTP 呼び出しより長生きしますが、ブリッジを
	// 止めればそれも止まらなければなりません。nil の場合、ダウンロードは完了に
	// よってしか止まりません。
	Lifetime context.Context
	// Dir は設置場所を上書きします。空なら Dir() を使います。
	Dir string
	// Build は取得するアーカイブを上書きします。ゼロ値なら Pinned() を使います。
	Build Build
	// Client は HTTP クライアントを上書きします。ダウンロードは大きく遅いので、
	// 既定のクライアントには全体のタイムアウトがありません。中断はコンテキストから
	// 来ます。
	Client *http.Client
	// StallTimeout は、見捨てるまでにダウンロードが何も受け取らずにいられる時間
	// です。0 なら defaultStallTimeout を使います。
	StallTimeout time.Duration
	Log          *slog.Logger
}

// Manager は、進行し得る唯一のダウンロードと、トレイと API が読む状態を所有します。
//
// 一度に 1 つ、そして答えを持つ場所も 1 つです。2 つの入口はどちらも同じ場所の同じ
// ファイルを求めるので、1 つ目が走っている最中の 2 つ目の要求は、別の仕事ではありません。
type Manager struct {
	build Build
	// isPinned は、そのビルドが呼び出し側ではなく Pinned() から来たことを記録
	// します。プラットフォームに縛られるのはそちらだけです。自分でアーカイブを
	// 指定する呼び出し側は、自分が何を求めているか分かっています。
	isPinned     bool
	dir          string
	client       *http.Client
	stallTimeout time.Duration
	log          *slog.Logger
	// lifetime を引数で受けずに保持しているのは、それが区切る仕事が呼び出しでは
	// ないからです。Start はダウンロードを goroutine に渡して返るので、バイトが
	// 動き出す頃には、コンテキストを運ぶ呼び出しはもう残っていません。
	lifetime context.Context

	// running は背後の goroutine を追います。停止処理が、その後片付けを待てる
	// ようにするためです。
	running sync.WaitGroup

	mu          sync.Mutex
	downloading bool
	received    int64
	lastError   string
}

// New は Manager を組み立てます。ディスクにもネットワークにも触れません。
func New(opts Options) *Manager {
	m := &Manager{
		build:        opts.Build,
		dir:          opts.Dir,
		client:       opts.Client,
		stallTimeout: opts.StallTimeout,
		log:          opts.Log,
		lifetime:     opts.Lifetime,
	}
	if m.stallTimeout <= 0 {
		m.stallTimeout = defaultStallTimeout
	}
	if m.lifetime == nil {
		m.lifetime = context.Background()
	}
	if m.build.URL == "" {
		m.build = Pinned()
		m.isPinned = true
	}
	if m.client == nil {
		// クライアント側のタイムアウトは設けない。これはユーザーの回線が何であれ
		// 100 メガバイト強を運ぶ話で、通常のリクエストに合う期限では、機能して
		// いるダウンロードを見捨てることになる。黙り込んだ接続は下の transport の
		// タイムアウトが引き続き覆うし、ユーザーの気変わりはコンテキストが覆う。
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

// Build は、この manager がダウンロードするものです。
func (m *Manager) Build() Build { return m.build }

// State は、UI が表示すべき内容を返します。
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

// Start は取得を背後で走らせ、動き出した時点で返ります。
//
// 呼び出し側は State を見て経過を追います。ダウンロードは分単位であり、メニューの
// クリックや HTTP リクエストを開いたまま待たせてよい長さを超えています。
func (m *Manager) Start() error {
	if err := m.begin(); err != nil {
		return err
	}
	m.running.Add(1)
	go func() {
		defer m.running.Done()
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

// Wait は、背後のダウンロードが後片付けを終えるまで、あるいは ctx が終わるまで
// ブロックします。
//
// lifetime をキャンセルすれば転送は止まりますが、goroutine が瞬時に消えるわけでは
// ありません。途中まで落としたアーカイブを閉じて削除する必要があります。それを
// 待たずにダウンロードの最中でブリッジを停止すると、設定フォルダに 100 メガバイト強が
// 残り、それを片付けるものが無くなり得ます。
func (m *Manager) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		m.running.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Fetch はダウンロードして設置し、バイナリのパスを返します。Start が走らせるのは
// これで、待ちたい呼び出し側のために公開しています。
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

// download はアーカイブを取得し、固定したダイジェストと照合し、必要なものを
// そこから設置します。
//
// アーカイブはメモリに載せずファイルへ書きます。このプロセスが占めるべき量より
// 大きいですし、zip を読むには seek が要るからです。
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

// fetchArchive は、設置場所の隣にある一時ファイルへダウンロードし、ダイジェストを
// 検証します。返すファイルの位置は先頭に戻してあります。
func (m *Manager) fetchArchive(ctx context.Context, dir string) (*os.File, error) {
	// 応答して本体を途中まで送り、その後黙り込むサーバは、ここの他の何にも
	// 覆われていない。transport のタイムアウトは応答ヘッダーで終わるし、100
	// メガバイトを運ぶ遅い回線は障害ではないので、クライアント全体の期限は意図的に
	// 設けていない。そこで転送ではなく読み取りを見る。少しでも進めばリセットされ、
	// まったく進まない区間だけがリクエストをキャンセルする。これが無いと
	// ダウンロードはブリッジが終了するまで固まり、その間ずっとメニューは
	// "Downloading..." のまま、再試行は実行中として拒否される。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(m.stallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()

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
	// 成功した経路 (そこでは呼び出し側が引き取る) 以外のすべてで削除する。失敗が
	// 残す 100 メガバイトは、小さな散らかりではない。
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	digest := sha256.New()
	// 期待する長さより 1 バイト多く読む。長すぎる本体を、黙って切り詰めてから
	// ダイジェスト検証で役に立たないメッセージとともに失敗させるのではなく、
	// その場で捕まえるため。
	body := io.LimitReader(resp.Body, m.build.Size+1)
	progress := &progressReader{r: body, report: func(received int64) {
		watchdog.Reset(m.stallTimeout)
		m.progress(received)
	}}
	written, err := io.Copy(io.MultiWriter(tmp, digest), progress)
	if err != nil {
		if stalled.Load() {
			return nil, fmt.Errorf("ffmpegfetch: download %s: nothing received for %s, giving up after %d of %d bytes", m.build.URL, m.stallTimeout, written, m.build.Size)
		}
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

// install は、バイナリとライセンス本文を取り出します。
//
// ライセンスを先に、バイナリを最後に置きます。他のすべてが存在を確認するのは
// バイナリだからです。この順で終えれば、見つかった ffmpeg は、既にライセンス本文が
// 隣に置かれている ffmpeg だということになります。
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

// findEntry は、末尾のパスでアーカイブ内の要素を探します。アーカイブの最上位
// フォルダはビルドにちなんで命名されているので、そうしなければ要素のパスにも
// バージョンを繰り返し書き、URL と歩調を合わせ続けることになります。
func findEntry(zr *zip.Reader, suffix string) (*zip.File, error) {
	if suffix == "" {
		return nil, errors.New("ffmpegfetch: no archive member named")
	}
	want := "/" + path.Clean(suffix)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// プラットフォームに関わらず、zip 形式自体がスラッシュ区切り。
		if name := path.Clean(f.Name); name == path.Clean(suffix) || strings.HasSuffix(name, want) {
			return f, nil
		}
	}
	return nil, fmt.Errorf("ffmpegfetch: the archive has no %s", suffix)
}

// extract は要素 1 つを所定の場所へ書き、そこにあったものを置き換えます。
//
// 一時ファイルと rename を経由するので、途中で死んだ取得が切り詰められた
// ffmpeg.exe を残すことはありません。そのファイルはソースのドライバに見つかり、
// 「未導入」とは別の何かとして失敗することになります。
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

// progressReader は、どれだけ届いたかを報告します。分単位のダウンロードの間、
// トレイが何かを表示できるようにするためです。
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
