package i18n

// メッセージカタログ。
//
// すべてのキーが両方の言語を持ちます。英語が原文なので先に書き、翻訳が欠けることは
// 許しません。テストがそれを強制します。半分だけ翻訳されたメニューは英語のままより
// 悪く、言語が無いのではなくバグに見えるからです。
//
// 書式指定子は両言語で一致していなければなりません。同じテストがそれも確認します。
// %s が %d になると、メニュー項目が "%!d(string=uvc)" に化けるためです。

// トレイのメニュー項目とそのツールチップ。
const (
	MenuStatusStarting  Key = "menu.status.starting"
	MenuStatusTip       Key = "menu.status.tip"
	MenuAddressTip      Key = "menu.address.tip"
	MenuSource          Key = "menu.source"
	MenuSourceTip       Key = "menu.source.tip"
	MenuSourceUVC       Key = "menu.source.uvc"
	MenuSourceSerial    Key = "menu.source.serial"
	MenuSourceMJPEG     Key = "menu.source.mjpeg"
	MenuPause           Key = "menu.pause"
	MenuPauseTip        Key = "menu.pause.tip"
	MenuLogDir          Key = "menu.logdir"
	MenuLogDirTip       Key = "menu.logdir.tip"
	MenuSettings        Key = "menu.settings"
	MenuSettingsTip     Key = "menu.settings.tip"
	MenuAutostart       Key = "menu.autostart"
	MenuAutostartTip    Key = "menu.autostart.tip"
	MenuQuit            Key = "menu.quit"
	MenuQuitTip         Key = "menu.quit.tip"
	MenuFFmpegTip       Key = "menu.ffmpeg.tip"
	MenuFFmpegGet       Key = "menu.ffmpeg.get"
	MenuFFmpegRetry     Key = "menu.ffmpeg.retry"
	MenuFFmpegInstalled Key = "menu.ffmpeg.installed"
	MenuFFmpegBusy      Key = "menu.ffmpeg.busy"
	MenuFFmpegProgress  Key = "menu.ffmpeg.progress"
)

// トレイが表示する 1 行の状態表示と、取り得る状態。
const (
	StatusNoSource     Key = "status.nosource"
	StatusPaused       Key = "status.paused"
	StatusReconnecting Key = "status.reconnecting"
	StatusConnecting   Key = "status.connecting"
	StatusRunning      Key = "status.running"
)

// ffmpeg のダウンロードダイアログ。
const (
	DialogFFmpegTitle     Key = "dialog.ffmpeg.title"
	DialogFFmpegBody      Key = "dialog.ffmpeg.body"
	DialogFFmpegInstalled Key = "dialog.ffmpeg.installed"
	DialogTitle           Key = "dialog.title"
)

// コマンドラインフラグのコンソール出力。
const (
	CLICaptureDevices   Key = "cli.capture_devices"
	CLISerialPorts      Key = "cli.serial_ports"
	CLINoneFound        Key = "cli.none_found"
	CLIRestored         Key = "cli.restored"
	CLINothingToRestore Key = "cli.nothing_to_restore"
	CLIRestoreHint      Key = "cli.restore_hint"
	CLINoSettingsPath   Key = "cli.no_settings_path"
	CLINoSettingsRead   Key = "cli.no_settings_read"
	CLINoDevices        Key = "cli.no_devices"
	CLINoSerialPorts    Key = "cli.no_serial_ports"
)

// ユーザーが何かできると想定される失敗。これだけです。残りはログが記録する英語の
// ままにします。"read from COM4: access denied" は、どちらの言語であってもログの
// 文脈無しに対処できるものではないからです。
const (
	ErrNoCamera Key = "err.no_camera"
	ErrNoFFmpeg Key = "err.no_ffmpeg"
	ErrNoPort   Key = "err.no_port"
)

var messages = map[Key]map[Lang]string{
	MenuStatusStarting: {English: "Starting...", Japanese: "起動中..."},
	MenuStatusTip:      {English: "Current source and frame rate", Japanese: "現在のソースとフレームレート"},
	MenuAddressTip:     {English: "Stream address; click to open a preview", Japanese: "配信アドレス。クリックでプレビューを開きます"},
	MenuSource:         {English: "Source", Japanese: "ソース"},
	MenuSourceTip:      {English: "Choose the camera to bridge", Japanese: "中継するカメラを選びます"},
	MenuSourceUVC:      {English: "UVC camera (USB)", Japanese: "UVC カメラ (USB)"},
	MenuSourceSerial:   {English: "Wired board (serial)", Japanese: "有線ボード (シリアル)"},
	MenuSourceMJPEG:    {English: "MJPEG stream (WiFi)", Japanese: "MJPEG ストリーム (WiFi)"},
	MenuPause:          {English: "Pause", Japanese: "一時停止"},
	MenuPauseTip:       {English: "Stop capturing and release the camera", Japanese: "取り込みを止めてカメラを解放します"},
	MenuLogDir:         {English: "Open log folder", Japanese: "ログフォルダを開く"},
	MenuLogDirTip:      {English: "Show the log files in Explorer", Japanese: "エクスプローラーでログを表示します"},
	MenuSettings:       {English: "Edit settings", Japanese: "設定を編集"},
	MenuSettingsTip:    {English: "Open ptcambridge.toml", Japanese: "ptcambridge.toml を開きます"},
	MenuAutostart:      {English: "Start with Windows", Japanese: "Windows と一緒に起動"},
	MenuAutostartTip:   {English: "Launch PTCamBridge at sign-in", Japanese: "サインイン時に PTCamBridge を起動します"},
	MenuQuit:           {English: "Quit", Japanese: "終了"},
	MenuQuitTip:        {English: "Stop PTCamBridge", Japanese: "PTCamBridge を終了します"},
	MenuFFmpegTip:      {English: "Download ffmpeg from its publisher", Japanese: "配布元から ffmpeg をダウンロードします"},

	MenuFFmpegGet:       {English: "Get ffmpeg (for UVC cameras)", Japanese: "ffmpeg を取得 (UVC カメラ用)"},
	MenuFFmpegRetry:     {English: "Get ffmpeg (last attempt failed)", Japanese: "ffmpeg を取得 (前回は失敗しました)"},
	MenuFFmpegInstalled: {English: "ffmpeg is installed", Japanese: "ffmpeg は導入済み"},
	MenuFFmpegBusy:      {English: "Downloading ffmpeg...", Japanese: "ffmpeg をダウンロード中..."},
	MenuFFmpegProgress:  {English: "Downloading ffmpeg... %d%%", Japanese: "ffmpeg をダウンロード中... %d%%"},

	StatusNoSource:     {English: "no source", Japanese: "ソース未選択"},
	StatusPaused:       {English: "%s: paused", Japanese: "%s: 一時停止中"},
	StatusReconnecting: {English: "%s: reconnecting (%s)", Japanese: "%s: 再接続中 (%s)"},
	StatusConnecting:   {English: "%s: connecting...", Japanese: "%s: 接続中..."},
	StatusRunning:      {English: "%s: %.1f fps, %d client(s)", Japanese: "%s: %.1f fps、クライアント %d 件"},

	DialogTitle:       {English: "PTCamBridge", Japanese: "PTCamBridge"},
	DialogFFmpegTitle: {English: "PTCamBridge - download ffmpeg", Japanese: "PTCamBridge - ffmpeg のダウンロード"},
	DialogFFmpegBody: {
		English: "PTCamBridge does not include ffmpeg. UVC cameras need it.\n\n" +
			"Download it now?\n\n" +
			"From: %s\n%s\n\n" +
			"Size: %d MB\n" +
			"Licence: FFmpeg, %s\n\n" +
			"It is downloaded from its publisher, not from PTCamBridge, and is\n" +
			"installed under your PTCamBridge settings folder.",
		Japanese: "PTCamBridge に ffmpeg は含まれていません。UVC カメラを使うには必要です。\n\n" +
			"今すぐダウンロードしますか?\n\n" +
			"配布元: %s\n%s\n\n" +
			"サイズ: %d MB\n" +
			"ライセンス: FFmpeg, %s\n\n" +
			"PTCamBridge からではなく配布元から直接ダウンロードし、\n" +
			"PTCamBridge の設定フォルダに配置します。",
	},
	DialogFFmpegInstalled: {
		English:  "ffmpeg is already installed:\n\n%s",
		Japanese: "ffmpeg は既に導入されています:\n\n%s",
	},

	CLICaptureDevices:   {English: "Capture devices:", Japanese: "カメラ:"},
	CLISerialPorts:      {English: "Serial ports:", Japanese: "シリアルポート:"},
	CLINoneFound:        {English: "  (none found)", Japanese: "  (見つかりません)"},
	CLIRestored:         {English: "PaperTracker address cache restored in %s", Japanese: "%s の PaperTracker 接続先を元に戻しました"},
	CLINothingToRestore: {English: "Nothing to restore: PTCamBridge has not changed the PaperTracker address cache.", Japanese: "戻すものはありません: PTCamBridge は PaperTracker の接続先を変更していません。"},
	CLIRestoreHint:      {English: "If the client is installed somewhere unusual, set papertracker.install_dir or pass -config.", Japanese: "クライアントが通常と異なる場所にある場合は、papertracker.install_dir を設定するか -config を指定してください。"},
	CLINoSettingsPath:   {English: "could not locate the settings file:", Japanese: "設定ファイルの場所が特定できません:"},
	CLINoSettingsRead:   {English: "could not read the settings file:", Japanese: "設定ファイルを読めません:"},
	CLINoDevices:        {English: "could not list capture devices:", Japanese: "カメラを列挙できません:"},
	CLINoSerialPorts:    {English: "could not list serial ports:", Japanese: "シリアルポートを列挙できません:"},

	ErrNoCamera: {
		English:  `No camera configured. Run "ptcambridge -list-devices" to see the cameras attached, then put one of the names in [source.uvc] device in the settings file.`,
		Japanese: `カメラが設定されていません。"ptcambridge -list-devices" で接続中のカメラ名を確認し、設定ファイルの [source.uvc] device に設定してください。`,
	},
	ErrNoFFmpeg: {
		English:  "ffmpeg was not found. Use \"Get ffmpeg\" in the tray menu, or set [source.uvc] ffmpeg_path.",
		Japanese: "ffmpeg が見つかりません。トレイメニューの「ffmpeg を取得」を使うか、[source.uvc] ffmpeg_path を設定してください。",
	},
	ErrNoPort: {
		English:  "No serial port matched a known camera board. Set [source.serial] port explicitly.",
		Japanese: "既知のカメラボードに一致するシリアルポートがありません。[source.serial] port を明示的に設定してください。",
	},
}
