package source

import (
	"errors"

	"github.com/limit7412/PTCamBridge/internal/i18n"
)

// ErrorKey は、画面がユーザーの言語で見せるべき失敗に対応するメッセージ名を返し、
// そうでない失敗には空を返します。
//
// 対象は、ログを読まずに対処できる失敗だけです。カメラが指定されていない、ffmpeg が
// 見つからない、ボードらしいポートが無い。それ以外 — 拒否されたポート、キャプチャ中に
// 消えたデバイス、壊れた URL — はドライバが書いたまま報告します。それらのメッセージは
// それらを有用にしている詳細を含んでおり、翻訳した要約では情報が減るからです。
//
// トレイや status tracker ではなくここに置いているのは、エラーが定義されているのが
// ここだからです。隣に sentinel を足した人が、翻訳可能にするまでに 2 つ隣のパッケージ
// まで行かずに済み、1 箇所の編集で届くようにしています。
func ErrorKey(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoDevice):
		return string(i18n.ErrNoCamera)
	case errors.Is(err, ErrNoFFmpeg):
		return string(i18n.ErrNoFFmpeg)
	case errors.Is(err, ErrNoSerialPort):
		return string(i18n.ErrNoPort)
	}
	return ""
}
