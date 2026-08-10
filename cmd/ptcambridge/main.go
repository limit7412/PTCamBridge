// Command ptcambridge は、Baballonia 互換の口トラッキングカメラを PaperTracker
// クライアントへ橋渡しし、そのクライアントがループバック上で期待する
// MJPEG-over-HTTP ストリームとして配信し直します。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/limit7412/PTCamBridge/internal/autostart"
	"github.com/limit7412/PTCamBridge/internal/bridge"
	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/console"
	"github.com/limit7412/PTCamBridge/internal/core"
	"github.com/limit7412/PTCamBridge/internal/ffmpegfetch"
	"github.com/limit7412/PTCamBridge/internal/hub"
	"github.com/limit7412/PTCamBridge/internal/i18n"
	"github.com/limit7412/PTCamBridge/internal/logging"
	"github.com/limit7412/PTCamBridge/internal/output"
	"github.com/limit7412/PTCamBridge/internal/papertracker"
	"github.com/limit7412/PTCamBridge/internal/server"
	"github.com/limit7412/PTCamBridge/internal/source"
	"github.com/limit7412/PTCamBridge/internal/status"
	"github.com/limit7412/PTCamBridge/internal/tray"
)

// Version はビルド時に -ldflags "-X main.Version=..." で埋め込まれます。
var Version = "dev"

func main() {
	// -H=windowsgui のビルドは自前のストリームを持たない。コマンドラインフラグの
	// 出力を出せるよう、起動元の端末のものを借りる。
	console.Attach()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ptcambridge:", err)
		os.Exit(1)
	}
}

// options はコマンドラインフラグです。優先順位では環境変数と設定ファイルの上に
// 位置します。
type options struct {
	configPath   string
	listen       string
	sourceType   string
	device       string
	serialPort   string
	mjpegURL     string
	logLevel     string
	headless     bool
	console      bool
	listDevices  bool
	showVersion  bool
	autostartOn  bool
	autostartNo  bool
	restoreCache bool
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.configPath, "config", "", "settings file (default: the per-user ptcambridge.toml)")
	flag.StringVar(&o.listen, "listen", "", "override server.listen, for example 127.0.0.1:18080")
	flag.StringVar(&o.sourceType, "source", "", "override source.type: uvc, serial or mjpeg")
	flag.StringVar(&o.device, "device", "", "override source.uvc.device")
	flag.StringVar(&o.serialPort, "serial-port", "", "override source.serial.port")
	flag.StringVar(&o.mjpegURL, "mjpeg-url", "", "override source.mjpeg.url")
	flag.StringVar(&o.logLevel, "log-level", "", "override log.level: debug, info, warn or error")
	flag.BoolVar(&o.headless, "headless", false, "run without a system tray icon")
	flag.BoolVar(&o.console, "console", false, "also write logs to stderr")
	flag.BoolVar(&o.listDevices, "list-devices", false, "list capture devices and serial ports, then exit")
	flag.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	flag.BoolVar(&o.autostartOn, "install-autostart", false, "register to start at sign-in, then exit")
	flag.BoolVar(&o.autostartNo, "uninstall-autostart", false, "remove the sign-in registration, then exit")
	flag.BoolVar(&o.restoreCache, "restore-cache", false, "put the PaperTracker address back the way it was, then exit")
	flag.Parse()
	return o
}

func run() error {
	opts := parseFlags()

	if opts.showVersion {
		fmt.Println("ptcambridge", Version)
		return nil
	}
	switch {
	case opts.autostartOn:
		// ここでユーザーが渡した -config を実行ファイルと一緒に登録するので、
		// 次のサインインは同じ設定ファイルで始まる。
		return autostart.Enable(opts.configPath)
	case opts.autostartNo:
		return autostart.Disable()
	}
	if opts.listDevices {
		return listDevices(opts)
	}
	if opts.restoreCache {
		// write_cache を切る操作とは分けてある。アンインストールは設定ファイルも
		// 一緒に消える場面であり、その変更に気づく次の起動が二度と来ないから。
		return restoreCache(opts)
	}

	cfgPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		return err
	}
	// 層を分けるのは Load の中ではなくここ。保存はファイルだけを土台にしなければ
	// ならないから。環境変数やフラグはこの実行のためのものであり、書き戻せば
	// 恒久的なものになってしまう。
	fileCfg, err := config.LoadFile(cfgPath)
	if err != nil {
		return err
	}
	cfg := fileCfg
	// どの葉が上書きされたかは、重ねる側にしか分かりません。ファイルと実効設定の
	// 差から推測すると、ファイルと同じ値を指定した上書き — PTCAMBRIDGE_LANGUAGE=en
	// をファイルの en に重ねる場合 — が見えません。それも上書きなので、ファイルを
	// ja に書き換えても次の起動はやはり en です。
	//
	// 数えるのは環境変数だけです。理由は applyFlags を参照。
	overridden, err := cfg.ApplyEnv(os.Getenv)
	if err != nil {
		return err
	}
	applyFlags(&cfg, opts)
	cfg.Normalise()
	if err := cfg.Validate(); err != nil {
		return err
	}
	// 起動時は、これから組み立てる出力がそのまま動く出力なので、突き合わせる相手は
	// この設定自身です。動作中の変更は bridge が起動時の設定と突き合わせます
	// (config.SerialPortConflict を参照)。
	if err := config.SerialPortConflict(cfg.Source, cfg.Output.Serial); err != nil {
		return err
	}

	log, closeLog, err := setupLogging(cfg, opts.console)
	if err != nil {
		return err
	}
	defer closeLog.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 先に bind することが単一起動の番人も兼ねる。listen ポートはこの
	// アプリケーションの身元であり、2 つ目の実体はそれを取れない。
	listener, err := server.Listen(cfg.Server.Listen)
	if err != nil {
		return describeBindFailure(cfg.Server.Listen, err)
	}
	defer listener.Close()
	address := listener.Addr().String()

	log.Info("ptcambridge starting",
		"version", Version,
		"address", address,
		"source", cfg.Source.Type,
		"config", cfgPath,
	)

	encoder, err := core.NewMultipartEncoder(cfg.Server.Boundary, cfg.StreamHeaders())
	if err != nil {
		return err
	}

	frames := hub.New()

	// シリアル出力は HTTP 配信と並ぶもう 1 つの出口で、有効なときだけ組み立てる。
	// 設定が誤っていればここで止まる。起動してから毎秒警告を出し続けるより、
	// 名前を書き間違えたことをその場で言う方がよい。
	var serialOut *output.Serial
	if cfg.Output.Serial.Enabled {
		serialOut, err = output.NewSerial(output.SerialConfig{
			Port:         cfg.Output.Serial.Port,
			Baud:         cfg.Output.Serial.Baud,
			Header:       cfg.OutputSerialHeader(),
			MaxFrameSize: cfg.Source.MaxFrameSize,
		}, frames, log)
		if err != nil {
			return err
		}
	}

	// tracker はトレイが翻訳すべき失敗に名前を付ける。どれがそれに当たるかは
	// ドライバの領分なので、答えはそちらから来る。
	tracker := status.New(status.WithErrorKeys(source.ErrorKey))
	app := bridge.New(cfg, cfgPath, frames, tracker, log)
	// 保存の起点は実効設定ではなくファイルが述べていた内容。-device や
	// PTCAMBRIDGE_* はこの実行のためのものであり、トレイが無関係な何かを変えた
	// 最初の瞬間に書き戻されてはいけない。fileCfg にはどちらの層も適用していない。
	app.SetPersistBase(fileCfg, overridden)

	admin := cfg.IsLoopback()
	if !admin {
		// 管理 API はソースを変更し設定を書き換えられるが、認証は無い。ループ
		// バックを離れたら提供しない。
		log.Warn("listening off loopback, the management API is disabled", "address", address)
	}
	// PTCamBridge は ffmpeg を同梱していない — 理由は internal/ffmpegfetch を
	// 参照 — ので、ビルドが公開されているプラットフォームでは、ユーザーが求めた
	// ときに取得できる。それ以外では提供しない。他の環境では ffmpeg はパッケージ
	// マネージャ 1 つで手に入るし、Windows のバイナリを勧めるのは何も言わないより
	// 悪い。
	var fetcher *ffmpegfetch.Manager
	if ffmpegfetch.Supported() {
		fetcher = ffmpegfetch.New(ffmpegfetch.Options{Lifetime: ctx, Log: log})
	}
	// ログの場所は診断画面とトレイの両方が表示する。どちらもここで解決した 1 つを
	// 受け取るので、2 か所が違う場所を指すことはない。
	logDir, _ := cfg.LogDir()

	srv, err := server.New(server.Options{
		Hub:              frames,
		Status:           tracker,
		Encoder:          encoder,
		Logger:           log,
		Controller:       app,
		EnableAdmin:      admin,
		FFmpeg:           ffmpegOption(fetcher),
		SerialOut:        serialOutOption(serialOut),
		HoldOnSourceLoss: cfg.Server.HoldOnSourceLoss,
		Version:          Version,
		Printer:          i18n.NewPrinter(cfg.Language()),
		ConfigPath:       cfgPath,
		LogDir:           logDir,
	})
	if err != nil {
		return err
	}
	// ストリーム設定は、今の PaperTracker クライアントが受け取れる形に合わせる
	// ために存在するので、その変更は再起動を経ずにサーバまで届かなければならない。
	app.SetStreamConfigurator(srv)

	switch {
	case cfg.PaperTracker.WriteCache:
		if err := papertracker.WriteCache(cfg.PaperTracker.InstallDir, connectAddress(address)); err != nil {
			// ブリッジ自体は動く。ユーザーが手でクライアントをこちらへ向ければ
			// よいだけ。
			log.Error("could not update the PaperTracker address cache", "error", err)
		} else {
			log.Info("PaperTracker address cache updated", "dir", cfg.PaperTracker.InstallDir, "address", address)
			// 後から取り消せるよう書き留めておく。その頃にはそれを指す設定が
			// 消えているかもしれない。既定へ戻すというのは、そういう姿をしている。
			if err := rememberWrittenDir(cfg.PaperTracker.InstallDir); err != nil {
				log.Warn("could not record which folder was changed, so restoring may not find it", "error", err)
			}
		}

	default:
		// write_cache を切ることは、入れたときにしたことを取り消すことでなければ
		// ならない。さもないとクライアントはキャッシュしたループバックアドレスを
		// 持ち続け、ブリッジが居なくなった後は何にも繋がらなくなる。ユーザーが
		// 切ったはずの設定が、持ち上げる手段の無いまま効き続けることになる。
		//
		// 設定がフォルダを指さなくなっている場合は探索する。既定へ戻すというのは
		// たいてい [papertracker] セクションを丸ごと削除することであり、それは
		// write_cache と一緒に install_dir も消す。そしてまさにそのとき、
		// バックアップを戻す必要が残っている。
		restoreCacheQuietly(log, cfg.PaperTracker.InstallDir)
	}

	// シリアル出力はソースとは独立に生きる。ソースが落ちていてもポートは開いて
	// おいてよく、フレームが戻れば書き始める。ctx で終わるので、止め方は他と同じ。
	var outputs sync.WaitGroup
	if serialOut != nil {
		outputs.Add(1)
		go func() {
			defer outputs.Done()
			if err := serialOut.Run(ctx); err != nil {
				log.Error("the serial output stopped for good", "error", err)
			}
		}()
	}

	if err := app.Start(ctx); err != nil {
		log.Error("could not start the configured source", "error", err)
		// それでも配信は続ける。ユーザーは再起動せずに、トレイか API から動く
		// ソースを選べる。
	}
	defer app.Stop()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx, listener)
		// listener が死ねばストリームのエンドポイントも道連れになる。キャンセル
		// すればトレイやヘッドレスの待機も落ちるので、プロセスは終了し監視側が
		// 再起動できる。何も配信していないのに健全そうな顔で居座り続けるよりよい。
		// エラーは、下の停止待ちが報告できるようバッファしてある。
		stop()
	}()

	if opts.headless {
		log.Info("running headless", "address", address)
		<-ctx.Done()
	} else {
		// ここから main の goroutine はトレイのもの。トレイはこのスレッドに
		// 結び付いたネイティブのメッセージループを走らせる。
		tray.Run(ctx, tray.Options{
			Controller: app,
			Hub:        frames,
			Status:     tracker,
			Log:        log,
			Address:    address,
			LogDir:     logDir,
			ConfigPath: cfgPath,
			ConfigFlag: opts.configPath,
			Dashboard:  admin,
			FFmpeg:     trayFFmpeg(fetcher),
			Printer:    i18n.NewPrinter(cfg.Language()),
			OnQuit:     stop,
		})
	}

	stop()
	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
	case <-time.After(5 * time.Second):
		log.Warn("the HTTP server did not shut down in time")
	}

	// 出力が握っているのはシリアルポートで、それは他のアプリケーションも開きたい
	// かもしれない相手。カメラと同じく、解放し終えるまで待ってから降りる。
	outputs.Wait()

	// ダウンロード中の終了は転送をキャンセルするが、goroutine には途中まで
	// 落としたアーカイブを削除する仕事が残っている。それを待たずに返ると、設定
	// フォルダに 100 メガバイト強が残り、それを片付けるものは何も動いていない。
	if fetcher != nil {
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
		fetcher.Wait(waitCtx)
		cancelWait()
	}

	log.Info("ptcambridge stopped")
	return nil
}

// ffmpegOption と trayFFmpeg は、fetcher を渡すか、まったく何も渡さないかの
// どちらかにします。
//
// 存在しないものをそのままインターフェースのフィールドへ代入することは「何も渡さない」
// ことにはなりません。nil ポインタを保持したインターフェースはそれ自体が非 nil なので、
// エンドポイントは経路に載り、メニュー項目も表示され、そのどちらもが存在しない
// ポインタを通して呼び出すことになります。
func ffmpegOption(m *ffmpegfetch.Manager) server.FFmpegFetcher {
	if m == nil {
		return nil
	}
	return m
}

func trayFFmpeg(m *ffmpegfetch.Manager) tray.FFmpegFetcher {
	if m == nil {
		return nil
	}
	return m
}

// serialOutOption も同じ理由で存在します。存在しない *output.Serial を
// インターフェースへ入れると、それ自体は非 nil になるので、/stats はシリアル出力を
// 設定していない人にもその節を見せ、存在しないポインタを通して読もうとします。
func serialOutOption(s *output.Serial) server.SerialOutput {
	if s == nil {
		return nil
	}
	return s
}

// resolveConfigPath は、パスが与えられていなければユーザーごとの場所を使います。
func resolveConfigPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return config.Path()
}

// applyFlags はコマンドラインを重ねます。これは他のすべてに優先します。
//
// 環境変数と違い、どの葉を指定したかは記録しません。フラグはこの起動だけのもので、
// 次の起動には残らないからです。自動起動が Run キーに書くのは実行ファイルと
// -config だけ (internal/autostart) なので、-log-level debug で一度起動した人が
// 設定画面から info を保存すれば、それは次のサインインで実際に効きます。これを
// 上書きとして数えると、起きる変更を「起きない」と案内することになります。
func applyFlags(cfg *config.Config, o options) {
	if o.listen != "" {
		cfg.Server.Listen = o.listen
	}
	if o.sourceType != "" {
		cfg.Source.Type = o.sourceType
	}
	if o.device != "" {
		cfg.Source.UVC.Device = o.device
	}
	if o.serialPort != "" {
		cfg.Source.Serial.Port = o.serialPort
	}
	if o.mjpegURL != "" {
		cfg.Source.MJPEG.URL = o.mjpegURL
	}
	if o.logLevel != "" {
		cfg.Log.Level = o.logLevel
	}
}

func setupLogging(cfg config.Config, console bool) (*slog.Logger, io.Closer, error) {
	dir, err := cfg.LogDir()
	if err != nil {
		// コンソールだけのログでも無いよりましだが、それがログとして数えられるのは
		// コンソールが実際に有効なときだけ。トレイからの起動は -console を渡さない
		// ので、これが無いと以降のエラーはすべて io.Discard へ流れる。
		fmt.Fprintln(os.Stderr, "ptcambridge: logging to the console only:", err)
		dir, console = "", true
	}
	return logging.Setup(logging.Options{Dir: dir, Level: cfg.Log.Level, Console: console})
}

// connectAddress は、listen アドレスをクライアントが接続できるアドレスに変えます。
//
// ワイルドカードの bind は "[::]:18080" のような形に解決されます。listen する対象
// としては妥当で、接続先としては役に立ちません。クライアントはこの機械の上で
// 動くので、欲しいのはループバックのアドレスです。
func connectAddress(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// describeBindFailure は "address already in use" を、ユーザーが実際に抱いている
// 問い — PTCamBridge はもう動いているのか — への答えに変えます。
func describeBindFailure(addr string, err error) error {
	if !errors.Is(err, syscall.EADDRINUSE) && !isAddrInUse(err) {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	if running, version := probeExistingInstance(addr); running {
		return fmt.Errorf("ptcambridge %s is already running on %s", version, addr)
	}
	return fmt.Errorf("%s is already in use by another program; set server.listen to a free port", addr)
}

// probeExistingInstance は、そのポートを握っている相手に、それが自分たちかどうかを
// 尋ねます。
func probeExistingInstance(addr string) (bool, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/stats")
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()

	var stats server.Stats
	if err := decodeJSON(resp.Body, &stats); err != nil || stats.Version == "" {
		return false, ""
	}
	return true, stats.Version
}

// modeListTimeout は、カメラ 1 台のモードを調べるのに与える時間。
//
// 期限はカメラごとに分ける。1 本の期限を全台で分け合うと、使用中の 1 台が待ち
// 時間を使い切った後のカメラは、繋がっていてもモードを一切出せない。遅いデバイス
// 1 台が、その後ろに並んだ全部の答えを消してしまう。
const modeListTimeout = 20 * time.Second

// listCameraModes は差し替えられるようにしてある。本物は ffmpeg を起動するので、
// テストからは踏めない。
var listCameraModes = source.ListModes

// modeQueryName は、モードを訊くときにそのカメラを指す名前。
//
// フレンドリ名は重複し得る。同じ名前のカメラが 2 台あるとき、それを見分けて
// いるのは Alternative の方なので、あるならそちらで訊く。名前で訊くと、ffmpeg が
// 曖昧として拒むか、毎回同じ 1 台を開いて、もう一方の見出しの下に別のカメラの
// モードを並べることになる。
func modeQueryName(d source.Device) string {
	if d.Alternative != "" {
		return d.Alternative
	}
	return d.Name
}

// cameraModes は、カメラ 1 台のモードを、そのカメラだけの期限のもとで調べます。
//
// モードはカメラごとに ffmpeg を 1 回起動して調べます。デバイス一覧そのものに
// 混ぜていないのは、設定画面が開くたびにそれを読むからです (source.ListModes を
// 参照)。ここは人が 1 回だけ叩くコマンドなので、その代金を払う価値があります。
// 設定に書く値を探しているのは、まさにこれを実行している人だからです。
func cameraModes(parent context.Context, ffmpegPath string, d source.Device) ([]source.Mode, error) {
	ctx, cancel := context.WithTimeout(parent, modeListTimeout)
	defer cancel()
	return listCameraModes(ctx, ffmpegPath, modeQueryName(d))
}

func listDevices(opts options) error {
	// Ctrl+C で降りられるようにします。ここは ffmpeg を何度も起動するサブコマンド
	// で、常駐側と違って signal.NotifyContext をまだ作っていません。親が黙って
	// 消えると、CREATE_NO_WINDOW で起動した ffmpeg にはコンソールの割り込みが
	// 届かず、カメラを掴んだまま残り得ます (childproc_windows.go を参照)。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// ここでも設定を読む。ffmpeg を同梱せず source.uvc.ffmpeg_path で指している
	// インストールは、そうしなければ、カメラを見つけることだけが仕事のコマンドから
	// 空のリストを受け取ることになる。
	//
	// 言語はその読み取りとは別に、しかも先に解決する。ユーザー自身の言語を最も
	// 必要とするメッセージは「設定が使えない」という類のものであり、それはまさに、
	// 設定から取った言語では届かないものだ。読み込みは既に失敗している。空のパスは
	// 特別扱いしない。ファイルが見つからないだけで、環境変数かシステムへ落ちる。
	cfgPath, pathErr := resolveConfigPath(opts.configPath)
	p := i18n.NewPrinter(config.LanguageWithoutLoading(cfgPath, os.Getenv))

	var ffmpegPath string
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, p.S(i18n.CLINoSettingsPath), pathErr)
	} else if cfg, err := config.Load(cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, p.S(i18n.CLINoSettingsRead), err)
	} else {
		ffmpegPath = cfg.Source.UVC.FFmpegPath
	}

	cameras, err := source.ListDevices(listCtx, ffmpegPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, p.S(i18n.CLINoDevices), err)
	}
	fmt.Println(p.S(i18n.CLICaptureDevices))
	if len(cameras) == 0 {
		fmt.Println(p.S(i18n.CLINoneFound))
	}
	for _, d := range cameras {
		fmt.Printf("  %s\n", d.Name)
		if d.Alternative != "" {
			fmt.Printf("    %s\n", d.Alternative)
		}
		modes, err := cameraModes(ctx, ffmpegPath, d)
		if err != nil {
			fmt.Fprintf(os.Stderr, "    %s %v\n", p.S(i18n.CLINoModes), err)
			continue
		}
		for _, m := range modes {
			fmt.Printf("      %s\n", m)
		}
	}

	ports, err := source.ListSerialPorts()
	if err != nil {
		fmt.Fprintln(os.Stderr, p.S(i18n.CLINoSerialPorts), err)
	}
	fmt.Println("\n" + p.S(i18n.CLISerialPorts))
	if len(ports) == 0 {
		fmt.Println(p.S(i18n.CLINoneFound))
	}
	for _, p := range ports {
		switch {
		case p.Vendor != "":
			fmt.Printf("  %s  [%s %s:%s]\n", p.Name, p.Vendor, p.VID, p.PID)
		case p.VID != "":
			fmt.Printf("  %s  [%s:%s]\n", p.Name, p.VID, p.PID)
		default:
			fmt.Printf("  %s\n", p.Name)
		}
	}
	return nil
}

// isAddrInUse は、POSIX の定数に対応しない Windows 側のエラー表現も拾います。
func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}
	var sysErr *os.SyscallError
	if !errors.As(opErr.Err, &sysErr) {
		return false
	}
	// WSAEADDRINUSE
	const wsaEAddrInUse = syscall.Errno(10048)
	return errors.Is(sysErr.Err, syscall.EADDRINUSE) || errors.Is(sysErr.Err, wsaEAddrInUse)
}

// decodeJSON は小さな包みです。組み立てだけのこのファイルの先頭に、probe のために
// encoding/json の import を持ち込まずに済ませます。
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(v)
}

// restoreCacheQuietly は、ブリッジが変更したことがあればクライアントのアドレスを
// 戻し、取り消すものが無ければ何も言いません。
//
// これは write_cache が切られていれば起動のたびに走るので、「ここにバックアップは
// 無い」「ここに PaperTracker は無い」は失敗ではなく普通の答えです。それらは
// ブリッジが触れていない機械を描写しているだけであり、エラーとして記録すれば
// 起動のたびに狼が来たと叫ぶことになります。
func restoreCacheQuietly(log *slog.Logger, installDir string) {
	restored, err := restoreEverywhereItWas(installDir)
	for _, dir := range restored {
		log.Info("PaperTracker address cache restored", "dir", dir)
	}
	if err != nil {
		log.Error("could not restore the PaperTracker address cache", "error", err)
	}
}

// restoreEverywhereItWas は、ブリッジが変更したクライアントをすべて元に戻し、
// 復元したフォルダを返します。することが無いのはエラーではなく、空のリストとして
// 現れます。
//
// 最初の 1 つではなくすべてのフォルダが対象です。write_cache が有効なまま
// install_dir は変わり得ます — クライアントが再インストールされたり移動されたり —
// そしてブリッジは、新しいフォルダと同様に古いフォルダにも記録を残します。最初の
// 成功で止めることが、もう一方のクライアントを、もう動いていないブリッジを指した
// まま、それに気づくものも無いまま残すことになります。
//
// 探索が問うのは「どのフォルダにクライアントがあるか」ではなく「どのフォルダに
// ブリッジ自身の記録があるか」なので、ブリッジが一度も変更していないインストールに
// 触れることはありません。
func restoreEverywhereItWas(installDir string) ([]string, error) {
	found, err := papertracker.FindRestoreDirs()
	if err != nil {
		return nil, err
	}

	// 設定が指すフォルダを先に置き、探索が見つけられなかった場合も試す。珍しい
	// 場所にあるインストールも取り消されるようにするため。
	var dirs []string
	add := func(dir string) {
		if dir != "" && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	add(installDir)
	// どこへ書いたかについてのブリッジ自身の覚書が、もうどの設定も指していない
	// フォルダを覆う。install_dir が珍しい場所を指していて、その後消された場合で
	// あり、既定へ戻すというのはまさにそれをすることだから。
	remembered, rememberErr := rememberedWrittenDirs()
	for _, dir := range remembered {
		add(dir)
	}
	for _, dir := range found {
		add(dir)
	}

	var restored []string
	// 読めなかった覚書は飲み込まず報告する。探索の届かないフォルダについての、
	// 唯一の言及を持っていたかもしれないから。
	errs := []error{rememberErr}
	for _, dir := range dirs {
		switch err := papertracker.RestoreCache(dir); {
		case err == nil:
			restored = append(restored, dir)
		case errors.Is(err, papertracker.ErrNoBackup):
			// ここに取り消すものは無い。ブリッジが別の場所に書いた機械において、
			// 設定が指すフォルダについての普通の答え。
		default:
			errs = append(errs, err)
		}
	}
	return restored, errors.Join(errs...)
}

// rememberWrittenDir は、ブリッジが自分へ向けたフォルダを、ブリッジ自身の設定の
// 隣に記録します。
func rememberWrittenDir(installDir string) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	return papertracker.RememberWrittenDir(dir, installDir)
}

// rememberedWrittenDirs はその記録を読み返します。
//
// これはブリッジの設定と一緒に置かれているので、それを手で削除すれば失われます。
// そして、探索が覆わないインストールを見つける唯一の手段も一緒に失われます。
// フォルダを消す前に -restore-cache を走らせること。このフラグはそのためにあります。
func rememberedWrittenDirs() ([]string, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return papertracker.WrittenDirs(dir)
}

// restoreCache は、ブリッジが初めてキャッシュに書き込む前のアドレスへ
// PaperTracker クライアントを戻します。
//
// これはアンインストールのためにあります。write_cache を切れば次の起動で復元
// されますが、PTCamBridge を撤去する人は設定ファイルと実行ファイルを一緒に消すので、
// それに気づく次の起動が存在しません。
func restoreCache(opts options) error {
	// 通常の Load を使わずに読む。あちらは設定ファイルが無ければ既定のものを書く。
	// このコマンドはアンインストールの最中に走らせるものなので、片付けている機械に
	// 設定フォルダを戻すことこそ、決してやってはいけない唯一のこと。
	cfgPath, _ := resolveConfigPath(opts.configPath)
	p := i18n.NewPrinter(config.LanguageWithoutLoading(cfgPath, os.Getenv))

	restored, err := restoreEverywhereItWas(configuredInstallDir(opts))
	for _, dir := range restored {
		fmt.Println(p.F(i18n.CLIRestored, dir))
	}
	if err != nil {
		return err
	}
	if len(restored) == 0 {
		// 探索はよくあるフォルダをすべて覆うので、これはブリッジがそのどれにも
		// 書いていないということを述べている。
		fmt.Println(p.S(i18n.CLINothingToRestore))
		fmt.Println(p.S(i18n.CLIRestoreHint))
	}
	return nil
}

// configuredInstallDir は設定が指すフォルダで、読むものが無ければ空です。復元は
// 探索に落ちるので、設定ファイルが削除された後でもこのフラグは機能します。それが
// 想定している状況そのものです。
//
// ファイルは、既に存在する場合にのみ読みます。config.Load は無ければ既定のものを
// 書き、それを収める %APPDATA%\PTCamBridge を作ります。つまりアンインストール中に
// 走らせるはずのコマンドが、ユーザーが取り除いている最中のフォルダを戻すことに
// なります。
// 読むのはその 1 つの設定だけで、他は何も検証しません。ブリッジが起動を拒否する
// ファイル — 綴りを誤ったキー、範囲外のボーレート、解釈できない環境変数 — でも、
// フォルダは何の問題もなく書かれています。見ることを拒めば、PTCamBridge を
// アンインストールしている人は、クライアントがまだそれを指したまま「何も変更されて
// いない」と告げられて帰されることになります。
func configuredInstallDir(opts options) string {
	// 他のどこでもそうであるように、ここでも環境変数がファイルに優先する。
	// PTCAMBRIDGE_PAPERTRACKER_DIR だけで指定されたフォルダは、ブリッジが書き込んで
	// きたフォルダであり、探索が覆うよくある場所には含まれていない可能性が非常に
	// 高い。この変数を無視することは、実際に変更された唯一のクライアントについて
	// 「何も変更されていない」と言うことを意味する。
	if dir := strings.TrimSpace(os.Getenv(config.EnvInstallDir)); dir != "" {
		return dir
	}
	cfgPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(cfgPath); err != nil {
		return ""
	}
	dir, err := config.InstallDirFromFile(cfgPath)
	if err != nil {
		// 飲み込まずはっきり言う。下の探索は引き続き走り、よくあるフォルダは
		// 覆うが、読めないファイルの中だけで指定されたフォルダは覆わない。
		fmt.Fprintln(os.Stderr, "ptcambridge:", err)
		return ""
	}
	return dir
}
