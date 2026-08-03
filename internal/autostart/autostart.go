// Package autostart は、サインイン時に PTCamBridge が起動するよう登録します。
//
// Windows ではユーザーごとの Run キーの下にエントリを作ります。昇格は不要です。
// それ以外のプラットフォームでは、成功したふりをせず未対応であると報告します。
package autostart

import "errors"

// ErrUnsupported は、自動起動の実装が無いプラットフォームで返されます。
var ErrUnsupported = errors.New("autostart: not supported on this platform")

// EntryName は、Run キーの下に書く値の名前です。
const EntryName = "PTCamBridge"

// Enabled は、自動起動のエントリが存在し、同じ configPath に対して Enable が
// 書くであろう内容と一致しているかを返します。
func Enabled(configPath string) (bool, error) { return enabled(configPath) }

// Enable は、現在の実行ファイルをサインイン時に起動するよう登録します。
//
// configPath はユーザーがコマンドラインで指定した設定ファイルで、登録するコマンドに
// 書き込みます。次のサインインが同じ設定で始まるようにするためです。指定が無かった
// 場合は空文字列を渡してください。その場合エントリは既定のユーザーごとの場所を
// 使います。
func Enable(configPath string) error { return enable(configPath) }

// Disable は自動起動のエントリを削除します。存在しないエントリの削除は成功と
// して扱います。
func Disable() error { return disable() }

// Supported は、このプラットフォームに自動起動の実装があるかを返します。
func Supported() bool { return supported }
