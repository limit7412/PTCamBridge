// Package core は PTCamBridge の副作用を持たないロジック — JPEG の検証、
// ワイヤプロトコルの解析、multipart のエンコード、画像変換 — を収めます。
//
// このパッケージのコードは、ソケット・シリアルポート・子プロセス・ファイル
// システムのいずれにも触れてはいけません。この制約こそが、カメラを繋がずに
// プロトコル処理をテストできる理由です。
package core

import (
	"errors"
	"fmt"
	"time"
)

// 構造スキャナが使う JPEG のマーカーバイト。
const (
	markerPrefix = 0xFF
	markerSOI    = 0xD8
	markerEOI    = 0xD9
	markerSOS    = 0xDA
	markerTEM    = 0x01
	markerRST0   = 0xD0
	markerRST7   = 0xD7
)

// DefaultMaxFrameSize は JPEG フレーム 1 枚の上限です。口の動きを追うカメラが
// 出すのは小さな画像 (240x240 が典型) なので、数 MiB を超えたということは、
// 巨大なフレームが本当に届いたのではなく、ストリームの同期が外れたことを
// 意味します。
const DefaultMaxFrameSize = 4 << 20

// MinJPEGSize は SOI + EOI の長さです。これより短いものは JPEG ではあり得ません。
const MinJPEGSize = 4

// ValidateJPEG が報告するエラー。
var (
	ErrFrameTooSmall = errors.New("frame too small to be a JPEG")
	ErrFrameTooLarge = errors.New("frame exceeds the maximum frame size")
	ErrMissingSOI    = errors.New("frame does not start with a JPEG SOI marker")
	ErrMissingEOI    = errors.New("frame does not end with a JPEG EOI marker")
)

// Frame は検証済みの JPEG 画像 1 枚です。配信された後の Data は不変として
// 扱います。購読者全員が同じ配列を共有するので、生産者はもう書き込まない
// バッファを渡さなければなりません。
type Frame struct {
	Data     []byte
	Seq      uint64
	RecvedAt time.Time
}

// Size は、エンコードされたフレームの長さをバイト単位で返します。
func (f Frame) Size() int { return len(f.Data) }

// ValidateJPEG は、data が単体で完結した JPEG に見えるかを確認します。
// maxSize が 0 なら DefaultMaxFrameSize を使います。
func ValidateJPEG(data []byte, maxSize int) error {
	if maxSize <= 0 {
		maxSize = DefaultMaxFrameSize
	}
	if len(data) < MinJPEGSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooSmall, len(data))
	}
	if len(data) > maxSize {
		return fmt.Errorf("%w: %d > %d bytes", ErrFrameTooLarge, len(data), maxSize)
	}
	if data[0] != markerPrefix || data[1] != markerSOI {
		return ErrMissingSOI
	}
	if data[len(data)-2] != markerPrefix || data[len(data)-1] != markerEOI {
		return ErrMissingEOI
	}
	return nil
}

// IsJPEG は、既定の上限で ValidateJPEG を通るかどうかを返します。
func IsJPEG(data []byte) bool { return ValidateJPEG(data, 0) == nil }
