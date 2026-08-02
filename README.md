# PaperBridge

Baballonia / Project Babble 系の口トラッキングカメラを、PaperTracker クライアントが期待する
MJPEG over HTTP ストリームとしてローカルホストに再配信する Windows 常駐ブリッジです。

公式 ESP32 カメラを持っていなくても、手持ちの UVC カメラ・有線 Babble ボード・WiFi MJPEG カメラを
PaperTracker の推論・OSC 送信機能に接続できます。

映像の推論やパラメータ変換は行いません。それは PaperTracker クライアント側の責務です。

## 仕組み

```
UVC カメラ ──(ffmpeg 子プロセス)──┐
有線ボード ──(シリアル/ETVR)─────┤──▶ FrameHub ──▶ HTTP MJPEG サーバー ──▶ PaperTracker
WiFi カメラ ─(MJPEG プロキシ)─────┘   (latest-frame-wins)   127.0.0.1:18080
```

フレームの配送は latest-frame-wins です。クライアントがフレーム 1 枚分でも遅れた場合、
溜め込むのではなく古いフレームを捨てて常に最新を配信します。口トラッキング用途では
遅延がフレームの網羅性より優先されるためです。

設計は Functional Core / Imperative Shell に従っています。プロトコルのパース・検証・
multipart エンコードは副作用のない純粋関数として `internal/core` に隔離してあり、
シリアル・プロセス・ソケット・ファイル I/O はすべてドライバ側に閉じ込めています。
おかげでワイヤーフォーマットの処理は実機なしで単体テストできます。

## インストール

Windows 10 / 11 (x64) 向けの単一実行ファイルです。

1. `paperbridge.exe` を任意のフォルダへ配置する
2. UVC カメラを使う場合は同じフォルダに `ffmpeg.exe` を置く
   (`ffmpeg_path` 設定または PATH でも可)
3. 実行するとタスクトレイに常駐する

初回起動時に `%APPDATA%\PaperBridge\paperbridge.toml` が既定値で生成されます。

## 使い方

### 1. カメラを選ぶ

利用可能なデバイスを一覧します。

```
paperbridge.exe -list-devices
```

設定ファイルの `[source]` セクション、またはトレイメニューの「Source」から選択します。

| ソース | 対象 | 設定 |
|---|---|---|
| `uvc` | USB ウェブカメラ全般 | `[source.uvc] device` に DirectShow のデバイス名 |
| `serial` | 有線 Babble ボード / OpenIris 系 | `[source.serial] port`。`auto` で VID から自動探索 |
| `mjpeg` | WiFi ESP32 など HTTP 配信カメラ | `[source.mjpeg] url` |

### 2. PaperTracker から接続する

PaperTracker クライアントはシリアル未接続時、実行ファイルと同じフォルダの
`wifi_cache.txt` に保存されたアドレスへ接続します。設定でインストールフォルダを
指定しておくと、起動時にこれを自動で書き換えます。

```toml
[papertracker]
install_dir = 'C:\Program Files\PaperTracker'
write_cache = true
```

元の内容は `wifi_cache.txt.bak` へ退避します。退避は初回のみです
(毎回取り直すとバックアップがブリッジ自身のアドレスで上書きされ、
本来戻したい値が失われるため)。

手動で設定する場合は、クライアントの接続先を `127.0.0.1:18080` にしてください。

## エンドポイント

| パス | 内容 |
|---|---|
| `/` および `/stream` | multipart MJPEG ストリーム |
| `/snapshot` | 最新フレーム 1 枚 (`image/jpeg`)。動作確認用 |
| `/healthz` | 200 / 503。フレームが届いている間だけ 200 |
| `/stats` | JSON。入力 fps、接続クライアント数、ドロップ数など |
| `/api/v1/config` | GET / PUT。設定の取得と適用 |
| `/api/v1/source` | POST `{"type":"uvc"}`。ソース切替 |
| `/api/v1/devices` | GET。カメラとシリアルポートの一覧 |

ルート `/` でストリームを返すのは、クライアントがキャッシュしたアドレスを
パス指定なしで叩くためで、これが互換性要件の核心です。

管理 API (`/api/v1/*`) は認証を持たないため、待受がループバック以外の場合は
自動的に無効化されます。

## 設定

`%APPDATA%\PaperBridge\paperbridge.toml`。項目の説明は
[`configs/paperbridge.toml`](configs/paperbridge.toml) を参照してください。

優先順位はコマンドライン引数 > 環境変数 (`PAPERBRIDGE_*`) > 設定ファイル > 既定値です。

主なコマンドライン引数:

```
-config <path>            設定ファイルを指定
-listen <host:port>        待受アドレスを上書き
-source <uvc|serial|mjpeg> ソース種別を上書き
-device <name>             UVC デバイス名を上書き
-headless                  トレイなしで起動
-console                   ログを標準エラー出力にも出す
-list-devices              デバイス一覧を表示して終了
-install-autostart         サインイン時の自動起動を登録して終了
```

## 制約

UVC デバイスは排他アクセスです。PaperBridge が掴んでいる間、同じカメラを
Baballonia 本体から開くことはできません。切り替えて使う想定であり、
同時利用は保証しません。トレイメニューの「Pause」でカメラを解放できます。

MJPEG ソースについては、上流のファームウェアが HTTP の多重接続に対応していれば
併用できる可能性があります。

## ビルド

```
go test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o paperbridge.exe ./cmd/paperbridge
```

cgo は使いません。Windows でのカメラ取得は ffmpeg を子プロセスとして起動し
stdout から MJPEG を読む方式で解決しているため、`CGO_ENABLED=0` の単一バイナリを維持できます。

## ライセンス

MIT
