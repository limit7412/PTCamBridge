package tray

import (
	"context"
	"time"

	"fyne.io/systray"

	"github.com/limit7412/PTCamBridge/internal/autostart"
	"github.com/limit7412/PTCamBridge/internal/i18n"
)

// refreshInterval は、メニューの状態表示を更新する間隔です。表示上の都合でしか
// なく、パイプラインの何もこれに依存していません。
const refreshInterval = time.Second

// Run はトレイアイコンを表示し、ユーザーが終了するか ctx がキャンセルされるまで
// ブロックします。
//
// main の goroutine から呼ぶ必要があります。トレイは Windows のメッセージループを
// 走らせ、それはウィンドウを作成したスレッドに結び付いているからです。
func Run(ctx context.Context, opts Options) {
	systray.Run(func() { onReady(ctx, opts) }, func() {
		if opts.OnQuit != nil {
			opts.OnQuit()
		}
	})
}

func onReady(ctx context.Context, opts Options) {
	p := opts.Printer

	systray.SetIcon(iconICO)
	systray.SetTitle(p.S(i18n.DialogTitle))
	systray.SetTooltip(p.S(i18n.DialogTitle))

	statusItem := systray.AddMenuItem(p.S(i18n.MenuStatusStarting), p.S(i18n.MenuStatusTip))
	statusItem.Disable()
	addressItem := systray.AddMenuItem("http://"+opts.Address, p.S(i18n.MenuAddressTip))
	systray.AddSeparator()

	sourceMenu := systray.AddMenuItem(p.S(i18n.MenuSource), p.S(i18n.MenuSourceTip))
	sourceItems := make(map[string]*systray.MenuItem, len(sourceChoices))
	for _, choice := range sourceChoices {
		label := p.S(choice.label)
		sourceItems[choice.kind] = sourceMenu.AddSubMenuItemCheckbox(label, label, false)
	}

	pauseItem := systray.AddMenuItemCheckbox(p.S(i18n.MenuPause), p.S(i18n.MenuPauseTip), opts.Controller.Paused())
	systray.AddSeparator()

	logItem := systray.AddMenuItem(p.S(i18n.MenuLogDir), p.S(i18n.MenuLogDirTip))
	if opts.LogDir == "" {
		logItem.Hide()
	}
	configItem := systray.AddMenuItem(p.S(i18n.MenuSettings), p.S(i18n.MenuSettingsTip))
	if opts.ConfigPath == "" {
		configItem.Hide()
	}

	ffmpegItem := systray.AddMenuItem(p.S(i18n.MenuFFmpegGet), p.S(i18n.MenuFFmpegTip))
	if opts.FFmpeg == nil {
		ffmpegItem.Hide()
	}

	autostartItem := systray.AddMenuItemCheckbox(p.S(i18n.MenuAutostart), p.S(i18n.MenuAutostartTip), false)
	if !autostart.Supported() {
		autostartItem.Hide()
	} else if on, err := autostart.Enabled(opts.ConfigFlag); err != nil {
		opts.Log.Warn("could not read the autostart entry", "error", err)
	} else if on {
		autostartItem.Check()
	}

	systray.AddSeparator()
	quitItem := systray.AddMenuItem(p.S(i18n.MenuQuit), p.S(i18n.MenuQuitTip))

	go run(ctx, opts, menu{
		status:    statusItem,
		address:   addressItem,
		sources:   sourceItems,
		pause:     pauseItem,
		logDir:    logItem,
		configure: configItem,
		ffmpeg:    ffmpegItem,
		autostart: autostartItem,
		quit:      quitItem,
	})
}

// menu は項目をまとめ、イベントループが 1 つの switch として読めるようにします。
type menu struct {
	status    *systray.MenuItem
	address   *systray.MenuItem
	sources   map[string]*systray.MenuItem
	pause     *systray.MenuItem
	logDir    *systray.MenuItem
	configure *systray.MenuItem
	ffmpeg    *systray.MenuItem
	autostart *systray.MenuItem
	quit      *systray.MenuItem
}

// run は、トレイが終了するまでメニューのクリックを処理し、状態表示を更新します。
func run(ctx context.Context, opts Options, m menu) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	// ブリッジを変更する項目はすべて 1 箇所で監視するので、ワーカーに届く順序は
	// クリックされた順序になる。下の select で case を分けてはいけない理由は
	// watchClicks を参照。
	actions := make(chan string, commandQueueDepth)
	clicks := make(map[string]<-chan struct{}, len(m.sources)+1)
	for kind, item := range m.sources {
		clicks[kind] = item.ClickedCh
	}
	clicks[actionPause] = m.pause.ClickedCh
	go watchClicks(ctx, opts.Log, clicks, actions)

	// ブリッジを変更するものはすべて、クリックされた順に 1 つのワーカーを通る。
	// commandQueue を参照。
	queue := newCommandQueue(opts.Log, commandQueueDepth)
	defer queue.close()

	wantPaused := opts.Controller.Paused()

	refresh(opts, m)
	for {
		select {
		case <-ctx.Done():
			systray.Quit()
			return

		case <-ticker.C:
			refresh(opts, m)

		case action := <-actions:
			// 完了時に更新はしない。ticker が、ブリッジがロック無しで公開している
			// 状態から 1 秒ごとに再描画するので、操作がどう転んでもメニューは
			// 自力で追いつく。
			if action == actionPause {
				// 切り替えの基準は、ブリッジが今報告している状態ではなく、
				// 直前のクリックが要求した内容。そうしないと立て続けの 2 回の
				// クリックはどちらも古い状態を見ることになり — 1 回目はまだ
				// SetPaused に届いていない — 同じことを 2 回要求して、対で
				// 打ち消し合わない。
				//
				// 数に入れるのは実際にキューへ入った要求だけ。捨てられた要求は
				// 何も変えていないので、それでも目標を動かすと、次のクリックが
				// ブリッジの既にある状態を要求することになり、何もしないボタンの
				// ように見える。
				paused := !wantPaused
				queued := queue.submit(actionPause, func() {
					if err := opts.Controller.SetPaused(paused); err != nil {
						opts.Log.Error("could not change the paused state", "paused", paused, "error", err)
					}
				})
				if queued {
					wantPaused = paused
				}
			} else {
				kind := action
				queue.submit("switch source", func() {
					if err := opts.Controller.Switch(ctx, kind); err != nil {
						opts.Log.Error("could not switch source", "source", kind, "error", err)
					}
				})
			}
			refresh(opts, m)

		case <-m.address.ClickedCh:
			openTarget("http://"+opts.Address+"/snapshot", opts)

		case <-m.logDir.ClickedCh:
			openTarget(opts.LogDir, opts)

		case <-m.configure.ClickedCh:
			openTarget(opts.ConfigPath, opts)

		case <-m.ffmpeg.ClickedCh:
			// コマンドキューには載せない。これはブリッジの設定に触れず、ソース
			// 切替と競合もせず、そのうえ人がダイアログを読むまでブロックする。
			// キューに入れると、それにかかる時間の分だけ後続のクリックすべてが
			// 足止めされる。
			go startFFmpegFetch(opts)

		case <-m.autostart.ClickedCh:
			toggleAutostart(opts, m)

		case <-m.quit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// refresh は、実時間の状態を映すメニュー部分を再描画します。
func refresh(opts Options, m menu) {
	snapshot := opts.Status.Snapshot()
	stats := opts.Hub.Stats()
	cfg := opts.Controller.Snapshot()

	for kind, item := range m.sources {
		if kind == cfg.Source.Type {
			item.Check()
		} else {
			item.Uncheck()
		}
	}

	paused := opts.Controller.Paused()
	if paused {
		m.pause.Check()
	} else {
		m.pause.Uncheck()
	}

	if opts.FFmpeg != nil {
		m.ffmpeg.SetTitle(ffmpegStatusLine(opts.Printer, opts.FFmpeg.State()))
	}

	m.status.SetTitle(statusLine(opts.Printer, snapshot, paused, stats.InputFPS, stats.Subscribers))
	systray.SetTooltip(opts.Printer.S(i18n.DialogTitle) + " - " + m.status.String())
}

// startFFmpegFetch は、先に確認を取ってからダウンロードします。
func startFFmpegFetch(opts Options) {
	if opts.FFmpeg == nil {
		return
	}
	p := opts.Printer
	state := opts.FFmpeg.State()
	switch {
	case state.Downloading:
		return
	case state.Installed:
		// 既にある。そう言う方が、何もしなかったように見えるクリックよりまし
		// だし、動いている ffmpeg を取り直すことがその意味ではない。
		confirm(p.S(i18n.DialogTitle), p.F(i18n.DialogFFmpegInstalled, state.Path))
		return
	}
	if !confirm(p.S(i18n.DialogFFmpegTitle), ffmpegPrompt(p, state.Source)) {
		return
	}
	if err := opts.FFmpeg.Start(); err != nil {
		opts.Log.Error("could not start the ffmpeg download", "error", err)
	}
}

func toggleAutostart(opts Options, m menu) {
	var err error
	if m.autostart.Checked() {
		err = autostart.Disable()
	} else {
		err = autostart.Enable(opts.ConfigFlag)
	}
	if err != nil {
		opts.Log.Error("could not change the autostart entry", "error", err)
		return
	}
	if m.autostart.Checked() {
		m.autostart.Uncheck()
	} else {
		m.autostart.Check()
	}
}

// openTarget はシェルに開かせます。呼び出しはイベントループから外します。
//
// ShellExecuteW は同期 API です。応答しない UNC パスのログフォルダや、DDE や COM の
// 起動待ちに入った既定ハンドラを相手にすると、タイムアウトまで返ってきません。
// イベントループの上で待てば、その間トレイは終了もクリックも 1 秒ごとの状態更新も
// 処理できなくなります。以前の子プロセス起動は rundll32 に仕事を渡して即座に返って
// いたので、ここで待つとトレイ全体が固まる回帰になります。
//
// commandQueue には載せません。あちらはブリッジのロックで直列化される操作を、
// クリックされた順に保つためのものです。開く操作はブリッジの設定に触れずソース切替と
// 競合もしないので、順序を守る理由が無く、代わりに 1 つの遅い呼び出しが後続の
// クリックすべてを足止めすることになります。ffmpeg の確認ダイアログを載せていないのと
// 同じ理由です。
func openTarget(target string, opts Options) {
	if target == "" {
		return
	}
	go func() {
		if err := openPath(target); err != nil {
			opts.Log.Error("could not open", "target", target, "error", err)
		}
	}()
}
