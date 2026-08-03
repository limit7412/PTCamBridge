package core

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// DefaultETVRHeader は、OpenIris / ETVR 系の有線ファームウェアが出すパケットの
// 前置きです。0xFF 0xA0 のパケットヘッダーに続いて 0xFF 0xA1 のフレームマーカーが
// 来ます。ここはファームウェアの版によって異なるため、パーサーに埋め込まず
// 設定可能にしています。
var DefaultETVRHeader = []byte{0xFF, 0xA0, 0xFF, 0xA1}

// etvrLengthBytes は、ヘッダーに続くリトルエンディアンのペイロード長フィールドの
// バイト数です。
const etvrLengthBytes = 2

// ErrEmptyHeader は、探すべき前置きを持たない ETVRParser を作ろうとしたときに
// 返されます。それでは再同期が不可能になります。
var ErrEmptyHeader = errors.New("etvr: header must not be empty")

// ETVRParser は、有線シリアルのパケット形式から JPEG フレームを取り出します。
//
//	offset 0  len 2  ヘッダー        (0xFF 0xA0)
//	offset 2  len 2  フレームマーク   (0xFF 0xA1)
//	offset 4  len 2  ペイロード長     (uint16、リトルエンディアン)
//	offset 6  len N  JPEG データ
//
// ゼロ値は使えません。NewETVRParser で作ってください。
type ETVRParser struct {
	header     []byte
	maxPayload int
}

// NewETVRParser は、指定した前置きを探すパーサーを返します。header が nil または
// 空なら DefaultETVRHeader を、maxPayload が 0 なら DefaultMaxFrameSize を使います。
func NewETVRParser(header []byte, maxPayload int) (ETVRParser, error) {
	if header == nil {
		header = DefaultETVRHeader
	}
	if len(header) == 0 {
		return ETVRParser{}, ErrEmptyHeader
	}
	if maxPayload <= 0 {
		maxPayload = DefaultMaxFrameSize
	}
	h := make([]byte, len(header))
	copy(h, header)
	return ETVRParser{header: h, maxPayload: maxPayload}, nil
}

// HeaderLen は、このパーサーが探す前置きの長さを返します。
func (p ETVRParser) HeaderLen() int { return len(p.header) }

// Header は、このパーサーが探す前置きのコピーを返します。線上に何も見つからな
// かったドライバは「何を探していたか」を言えなければなりません。答えが
// 「ファームウェアが別のものを使っている」である場合があるからです。
func (p ETVRParser) Header() []byte { return bytes.Clone(p.header) }

// Parse は buf から完結したパケットを取り出し、見つかった JPEG ペイロードと、
// まだ消費できなかったバイト列を返します。
//
// 返すフレームはコピーなので保持して構いません。rest は buf を指しているため、
// 読み取りバッファを使い回す呼び出し側はコピーするか詰め直す必要があります
// (定番の buf = append(buf[:0], rest...) で構いません。copy が重なりを扱います)。
//
// 最初のヘッダーより前のバイトは捨てます。これにより、すでに流れている
// ストリームの途中から読み始められます。長さフィールドがあり得ない値のパケットや、
// ペイロードが正しい JPEG でないパケットは捨て、走査はそのヘッダーの 1 バイト先から
// 再開します。画像データの中の偽の一致でパーサーが詰まらないようにするためです。
//
// tail は、返した最後のパケットの終端より後ろに buf が持っている量です。
// 1 つも返さなかった場合は buf 全体になります。これは rest からは導けません。
// rest はまだパケットになり得る分しか保持しておらず、それ以外はこの関数が返る
// 時点で捨てられているからです。線上に何が届いたかを報告する側は、捨てた分も
// 数える必要があります。それこそが「何かは届いているが解析できていない」ことの
// 証拠だからです。
func (p ETVRParser) Parse(buf []byte) (frames [][]byte, rest []byte, tail int) {
	pos := 0
	// 最後に返したパケットが終わった位置。1 つも無いうちは 0 で、その場合は
	// バッファ全体が tail になる。フレームの後ろに位置する部分が無いのだから、
	// それで正しい。
	lastEnd := 0
	for {
		idx := bytes.Index(buf[pos:], p.header)
		if idx < 0 {
			// 途中のヘッダーは無い。2 回の読み取りにまたがって分断された
			// ヘッダーの先頭になり得る末尾だけを残す。
			keep := len(p.header) - 1
			if keep > len(buf)-pos {
				keep = len(buf) - pos
			}
			return frames, buf[len(buf)-keep:], len(buf) - lastEnd
		}
		start := pos + idx
		lenAt := start + len(p.header)
		if lenAt+etvrLengthBytes > len(buf) {
			// ヘッダーは見えたが、長さフィールドがまだ届いていない。
			return frames, buf[start:], len(buf) - lastEnd
		}
		payloadLen := int(binary.LittleEndian.Uint16(buf[lenAt : lenAt+etvrLengthBytes]))
		if payloadLen < MinJPEGSize || payloadLen > p.maxPayload {
			pos = start + 1
			continue
		}
		payloadAt := lenAt + etvrLengthBytes
		end := payloadAt + payloadLen
		if end > len(buf) {
			// ペイロードがまだ途中。残りを待つ。
			return frames, buf[start:], len(buf) - lastEnd
		}
		payload := buf[payloadAt:end]
		if ValidateJPEG(payload, p.maxPayload) != nil {
			pos = start + 1
			continue
		}
		frames = append(frames, bytes.Clone(payload))
		pos = end
		lastEnd = end
	}
}

// EncodePacket は、渡された JPEG ペイロードのワイヤパケットを組み立てます。
// テストやフィクスチャが Parse へバイト単位で正確な入力を作れるようにするための
// ものです。
func (p ETVRParser) EncodePacket(payload []byte) ([]byte, error) {
	if len(payload) > 0xFFFF {
		return nil, fmt.Errorf("etvr: payload of %d bytes exceeds the 16-bit length field", len(payload))
	}
	out := make([]byte, 0, len(p.header)+etvrLengthBytes+len(payload))
	out = append(out, p.header...)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(payload)))
	out = append(out, payload...)
	return out, nil
}
