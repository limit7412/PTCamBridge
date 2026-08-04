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
	MenuAddressTipUI    Key = "menu.address.tip.ui"
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

// 診断画面。
//
// ここはログではなく画面なので翻訳します。読み手はブラウザの前に座っている
// ユーザーであって、不具合報告に貼られた記録を後から読む人ではありません。
// 画面が表示する値そのもの — デバイス名、パス、エラーの文言 — は翻訳しません。
// それらは機械から来たものであり、書き写して検索する対象です。
const (
	UITitle       Key = "ui.title"
	UIStatus      Key = "ui.status"
	UIPreview     Key = "ui.preview"
	UIFrames      Key = "ui.frames"
	UIDevices     Key = "ui.devices"
	UIFFmpeg      Key = "ui.ffmpeg"
	UIPlaces      Key = "ui.places"
	UIUnreachable Key = "ui.unreachable"

	UIFieldSource      Key = "ui.field.source"
	UIFieldState       Key = "ui.field.state"
	UIFieldUptime      Key = "ui.field.uptime"
	UIFieldReconnects  Key = "ui.field.reconnects"
	UIFieldLastError   Key = "ui.field.last_error"
	UIFieldInputFPS    Key = "ui.field.input_fps"
	UIFieldClients     Key = "ui.field.clients"
	UIFieldPublished   Key = "ui.field.published"
	UIFieldDropped     Key = "ui.field.dropped"
	UIFieldFrameSize   Key = "ui.field.frame_size"
	UIFieldLastFrame   Key = "ui.field.last_frame"
	UIFieldAddress     Key = "ui.field.address"
	UIFieldSettings    Key = "ui.field.settings"
	UIFieldLogs        Key = "ui.field.logs"
	UIFieldVersion     Key = "ui.field.version"
	UIFieldCameras     Key = "ui.field.cameras"
	UIFieldSerialPorts Key = "ui.field.serial_ports"

	UIStateRunning      Key = "ui.state.running"
	UIStatePaused       Key = "ui.state.paused"
	UIStateConnecting   Key = "ui.state.connecting"
	UIStateReconnecting Key = "ui.state.reconnecting"
	UIStateNoSource     Key = "ui.state.nosource"

	UINone            Key = "ui.none"
	UIAgo             Key = "ui.ago"
	UIWaitingForFrame Key = "ui.waiting_for_frame"
	UIPreviewNote     Key = "ui.preview.note"

	UINavStatus   Key = "ui.nav.status"
	UINavSettings Key = "ui.nav.settings"

	UISettingsTitle   Key = "ui.settings.title"
	UISettingsStream  Key = "ui.settings.stream"
	UISettingsDisplay Key = "ui.settings.display"
	UISettingsLog     Key = "ui.settings.log"
	UITransform       Key = "ui.transform"
	UIPaperTracker    Key = "ui.papertracker"

	UIFieldDevice     Key = "ui.field.device"
	UIFieldSize       Key = "ui.field.size"
	UIFieldFramerate  Key = "ui.field.framerate"
	UIFieldFFmpegPath Key = "ui.field.ffmpeg_path"
	UIFieldPort       Key = "ui.field.port"
	UIFieldBaud       Key = "ui.field.baud"
	UIFieldURL        Key = "ui.field.url"
	UIFieldRotate     Key = "ui.field.rotate"
	UIFieldFlipH      Key = "ui.field.flip_h"
	UIFieldFlipV      Key = "ui.field.flip_v"
	UIFieldCropSquare Key = "ui.field.crop_square"
	UIFieldQuality    Key = "ui.field.quality"
	UIFieldHold       Key = "ui.field.hold"
	UIFieldBoundary   Key = "ui.field.boundary"
	UIFieldListen     Key = "ui.field.listen"
	UIFieldInstallDir Key = "ui.field.install_dir"
	UIFieldWriteCache Key = "ui.field.write_cache"
	UIFieldLanguage   Key = "ui.field.language"
	UIFieldLogLevel   Key = "ui.field.log_level"
	UIFieldLogDir     Key = "ui.field.log_dir"
	UILangAuto        Key = "ui.lang.auto"
	UIQualityHint     Key = "ui.hint.quality"
	UIFramerateHint   Key = "ui.hint.framerate"
	UISizeHint        Key = "ui.hint.size"
	UIWriteCacheHint  Key = "ui.hint.write_cache"
	UIRestartBadge    Key = "ui.restart.badge"
	UIRestartNote     Key = "ui.restart.note"
	UIOverriddenNote  Key = "ui.overridden.note"
	UISave            Key = "ui.save"
	UISaving          Key = "ui.saving"
	UISaved           Key = "ui.saved"
	UISavedPending    Key = "ui.saved_pending"
	UISaveFailed      Key = "ui.save_failed"
	UIGetFFmpeg       Key = "ui.get_ffmpeg"

	UIFFmpegInstalled   Key = "ui.ffmpeg.installed"
	UIFFmpegMissing     Key = "ui.ffmpeg.missing"
	UIFFmpegDownloading Key = "ui.ffmpeg.downloading"
	UIFFmpegUnsupported Key = "ui.ffmpeg.unsupported"
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
	MenuAddressTipUI:   {English: "Stream address; click to open the diagnostics page", Japanese: "配信アドレス。クリックで診断画面を開きます"},
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

	UITitle:       {English: "PTCamBridge diagnostics", Japanese: "PTCamBridge 診断"},
	UIStatus:      {English: "Status", Japanese: "状態"},
	UIPreview:     {English: "Preview", Japanese: "プレビュー"},
	UIFrames:      {English: "Frames", Japanese: "フレーム"},
	UIDevices:     {English: "Devices", Japanese: "デバイス"},
	UIFFmpeg:      {English: "ffmpeg", Japanese: "ffmpeg"},
	UIPlaces:      {English: "Places", Japanese: "場所"},
	UIUnreachable: {English: "PTCamBridge is not answering. It may have stopped.", Japanese: "PTCamBridge が応答しません。終了した可能性があります。"},

	UIFieldSource:      {English: "Source", Japanese: "ソース"},
	UIFieldState:       {English: "State", Japanese: "状態"},
	UIFieldUptime:      {English: "Uptime", Japanese: "稼働時間"},
	UIFieldReconnects:  {English: "Reconnects", Japanese: "再接続"},
	UIFieldLastError:   {English: "Last error", Japanese: "直近のエラー"},
	UIFieldInputFPS:    {English: "Input", Japanese: "入力"},
	UIFieldClients:     {English: "Clients", Japanese: "クライアント"},
	UIFieldPublished:   {English: "Published", Japanese: "配信"},
	UIFieldDropped:     {English: "Dropped", Japanese: "破棄"},
	UIFieldFrameSize:   {English: "Frame size", Japanese: "フレームサイズ"},
	UIFieldLastFrame:   {English: "Last frame", Japanese: "最終フレーム"},
	UIFieldAddress:     {English: "Stream address", Japanese: "配信アドレス"},
	UIFieldSettings:    {English: "Settings file", Japanese: "設定ファイル"},
	UIFieldLogs:        {English: "Log folder", Japanese: "ログフォルダ"},
	UIFieldVersion:     {English: "Version", Japanese: "バージョン"},
	UIFieldCameras:     {English: "Cameras", Japanese: "カメラ"},
	UIFieldSerialPorts: {English: "Serial ports", Japanese: "シリアルポート"},

	UIStateRunning:      {English: "receiving frames", Japanese: "受信中"},
	UIStatePaused:       {English: "paused", Japanese: "一時停止中"},
	UIStateConnecting:   {English: "connecting", Japanese: "接続中"},
	UIStateReconnecting: {English: "reconnecting", Japanese: "再接続中"},
	UIStateNoSource:     {English: "no source", Japanese: "ソース未選択"},

	UINone:            {English: "(none)", Japanese: "(なし)"},
	UIAgo:             {English: "%s ago", Japanese: "%s 前"},
	UIWaitingForFrame: {English: "Waiting for a frame", Japanese: "フレームを待っています"},
	UIPreviewNote: {
		English:  "The preview asks for one still at a time, so it is not counted as a client above.",
		Japanese: "プレビューは静止画を 1 枚ずつ取得するので、上のクライアント数には数えられません。",
	},

	UINavStatus:   {English: "Status", Japanese: "状態"},
	UINavSettings: {English: "Settings", Japanese: "設定"},

	UISettingsTitle:   {English: "PTCamBridge settings", Japanese: "PTCamBridge 設定"},
	UISettingsStream:  {English: "Stream", Japanese: "配信"},
	UISettingsDisplay: {English: "Display", Japanese: "表示"},
	UISettingsLog:     {English: "Log", Japanese: "ログ"},
	UITransform:       {English: "Transform", Japanese: "変換"},
	UIPaperTracker:    {English: "PaperTracker", Japanese: "PaperTracker 連携"},

	UIFieldDevice:     {English: "Device", Japanese: "デバイス"},
	UIFieldSize:       {English: "Resolution", Japanese: "解像度"},
	UIFieldFramerate:  {English: "Frame rate", Japanese: "フレームレート"},
	UIFieldFFmpegPath: {English: "ffmpeg path", Japanese: "ffmpeg のパス"},
	UIFieldPort:       {English: "Port", Japanese: "ポート"},
	UIFieldBaud:       {English: "Baud rate", Japanese: "ボーレート"},
	UIFieldURL:        {English: "URL", Japanese: "URL"},
	UIFieldRotate:     {English: "Rotate", Japanese: "回転"},
	UIFieldFlipH:      {English: "Flip horizontally", Japanese: "左右反転"},
	UIFieldFlipV:      {English: "Flip vertically", Japanese: "上下反転"},
	UIFieldCropSquare: {English: "Crop to a square", Japanese: "正方形に切り出す"},
	UIFieldQuality:    {English: "Re-encode quality", Japanese: "再エンコード品質"},
	UIFieldHold:       {English: "Keep clients connected while the source is away", Japanese: "ソースが落ちている間もクライアントを繋いだままにする"},
	UIFieldBoundary:   {English: "Boundary", Japanese: "boundary"},
	UIFieldListen:     {English: "Listen address", Japanese: "待受アドレス"},
	UIFieldInstallDir: {English: "Install folder", Japanese: "インストールフォルダ"},
	UIFieldWriteCache: {English: "Point PaperTracker at this bridge on startup", Japanese: "起動時に PaperTracker の接続先をこのブリッジに向ける"},
	UIFieldLanguage:   {English: "Language", Japanese: "表示言語"},
	UIFieldLogLevel:   {English: "Level", Japanese: "レベル"},
	UIFieldLogDir:     {English: "Folder", Japanese: "フォルダ"},
	UILangAuto:        {English: "Follow the system", Japanese: "OS に合わせる"},
	UIQualityHint:     {English: "0 transfers the camera's own JPEG without re-encoding it.", Japanese: "0 なら再エンコードせず、カメラの JPEG をそのまま転送します。"},
	UIFramerateHint: {
		English:  "0 leaves the frame rate to the camera. A rate the camera does not have stops it from opening at all.",
		Japanese: "0 ならフレームレートをカメラに任せます。カメラが持っていない値を書くと、そのカメラは開きません。",
	},
	UISizeHint: {
		English:  "Empty leaves the resolution to the camera. A size the camera does not have stops it from opening at all.",
		Japanese: "空ならカメラに任せます。カメラが持っていない解像度を書くと、そのカメラは開きません。",
	},
	UIWriteCacheHint: {
		English:  "The previous address is kept, and \"ptcambridge -restore-cache\" puts it back.",
		Japanese: "元の接続先は控えてあり、\"ptcambridge -restore-cache\" で戻せます。",
	},
	UIRestartBadge: {English: "needs a restart", Japanese: "再起動が必要"},
	UIRestartNote: {
		English:  "Settings marked that way are saved, but the running bridge keeps using the old values until it is restarted.",
		Japanese: "この印の付いた設定は保存されますが、動作中のブリッジは再起動するまで古い値を使い続けます。",
	},
	UIOverriddenNote: {
		English:  "These settings were given on the command line or in the environment at startup. That wins, so changing them here has no effect, this time or the next:",
		Japanese: "次の設定は、起動時にコマンドラインまたは環境変数で指定されています。そちらが優先されるので、ここで変えても今回も次回も反映されません:",
	},
	UISave:         {English: "Save", Japanese: "保存"},
	UISaving:       {English: "Saving...", Japanese: "保存中..."},
	UISaved:        {English: "Saved.", Japanese: "保存しました。"},
	UISavedPending: {English: "Saved. These take effect on the next start:", Japanese: "保存しました。次の設定は次回起動で反映されます:"},
	UISaveFailed:   {English: "Could not save:", Japanese: "保存できませんでした:"},
	UIGetFFmpeg:    {English: "Get ffmpeg", Japanese: "ffmpeg を取得"},

	UIFFmpegInstalled:   {English: "installed", Japanese: "導入済み"},
	UIFFmpegMissing:     {English: "not installed; UVC cameras need it", Japanese: "未導入。UVC カメラを使うには必要です"},
	UIFFmpegDownloading: {English: "downloading... %d%%", Japanese: "ダウンロード中... %d%%"},
	UIFFmpegUnsupported: {English: "no build is published for this platform", Japanese: "このプラットフォーム向けのビルドはありません"},

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
