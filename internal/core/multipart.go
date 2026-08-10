package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultBoundary は、PTCamBridge が名乗る multipart の区切り文字列です。
const DefaultBoundary = "ptcambridge"

// ErrInvalidBoundary は、Content-Type ヘッダーや区切り行に書き出せない
// boundary に対して返されます。
var ErrInvalidBoundary = errors.New("invalid multipart boundary")

// StreamHeader は、各フレームに付ける追加のパートヘッダーです。PaperTracker
// クライアントが受け取る形は版によって違い得るので、ヘッダーの構成は固定せず
// 設定可能にしています。合わなかったときに、再ビルドではなく設定で直せるように
// するためです。
type StreamHeader struct {
	Name  string
	Value string
}

// MultipartEncoder は、PaperTracker クライアントが期待する
// multipart/x-mixed-replace の形にフレームを整えます。
//
//	--boundary\r\n
//	Content-Type: image/jpeg\r\n
//	Content-Length: N\r\n
//	\r\n
//	<N bytes of JPEG>\r\n
//
// Content-Length は必須です。クライアントはこれで各フレームの大きさを判断して
// おり、省くとフレームの切れ目を見失います。チャンク転送は使いません。ボディは
// 終わりのないストリームとして書き出します。
type MultipartEncoder struct {
	boundary string
	extra    []StreamHeader
}

// NewMultipartEncoder は boundary を検証し、それを使うエンコーダを返します。
// 空を渡すと DefaultBoundary を使います。
func NewMultipartEncoder(boundary string, extra []StreamHeader) (MultipartEncoder, error) {
	if boundary == "" {
		boundary = DefaultBoundary
	}
	if err := ValidateBoundary(boundary); err != nil {
		return MultipartEncoder{}, err
	}
	for _, h := range extra {
		if err := ValidateStreamHeader(h.Name, h.Value); err != nil {
			return MultipartEncoder{}, err
		}
	}
	return MultipartEncoder{boundary: boundary, extra: append([]StreamHeader(nil), extra...)}, nil
}

// reservedPartHeaders は、エンコーダ自身が書くパートヘッダーです。値の異なる
// 2 つ目が並ぶとクライアントはどちらを取るか選ぶことになり、Content-Length を
// 選び間違えればフレームの切れ目は二度と戻りません。
var reservedPartHeaders = []string{"Content-Type", "Content-Length"}

// ValidateStreamHeader は、名前と値が追加パートヘッダーとして使えるかを返します。
//
// 判定は「改行を含まない」ではなく、RFC 7230 がヘッダーフィールドに定めた構文で
// 行います。"Bad Header" のような名前や、制御バイトが紛れ込んだ値は何事もなく
// 書き出せてしまう一方、厳格な MIME パーサーはパート全体を拒否します。200 を
// 返した設定変更が、クライアントの読めないストリームに化けることになります。
func ValidateStreamHeader(name, value string) error {
	if name == "" {
		return errors.New("an extra header name cannot be empty")
	}
	for _, r := range name {
		if !isTokenRune(r) {
			return fmt.Errorf("extra header name %q contains %q, which is not allowed in a header name", name, r)
		}
	}
	for _, r := range value {
		if r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("extra header %q has a control character %q in its value", name, r)
		}
	}
	for _, reserved := range reservedPartHeaders {
		if strings.EqualFold(strings.TrimSpace(name), reserved) {
			return fmt.Errorf("extra header %q is written by the encoder itself and cannot be overridden", name)
		}
	}
	return nil
}

// ValidateBoundary は、s が multipart の区切りとして使えるかを返します。
func ValidateBoundary(s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty", ErrInvalidBoundary)
	}
	if len(s) > 70 {
		return fmt.Errorf("%w: longer than 70 characters", ErrInvalidBoundary)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("'()+_,-./:=?", r):
		default:
			return fmt.Errorf("%w: character %q is not allowed", ErrInvalidBoundary, r)
		}
	}
	return nil
}

// Boundary は、使用中の区切り文字列を返します。
func (e MultipartEncoder) Boundary() string { return e.boundary }

// ContentType は、ストリームエンドポイントの応答 Content-Type です。
//
// RFC 2046 が区切りに許す文字の中には、RFC 2045 が裸のパラメータトークンに
// 許さないものがあるため、そうした boundary は引用符付き文字列として書きます。
// 引用しなければ "a:b" のような値はメディアタイプ全体を解析不能にし、規格に
// 忠実なクライアントは区切りをまったく見つけられません。
func (e MultipartEncoder) ContentType() string {
	if strings.ContainsAny(e.boundary, nonTokenBoundaryChars) {
		// ValidateBoundary が '"' と '\\' を弾いているので、エスケープは不要。
		return `multipart/x-mixed-replace; boundary="` + e.boundary + `"`
	}
	return "multipart/x-mixed-replace; boundary=" + e.boundary
}

// nonTokenBoundaryChars は、ValidateBoundary が受け入れる文字のうち RFC 2045 の
// トークン文字ではないもので、これがあるとパラメータの引用が必要になります。
const nonTokenBoundaryChars = "()/:=?,"

// isTokenRune は、RFC 7230 の token 定義に従って、r がヘッダーフィールド名に
// 現れてよいかを返します。
func isTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

// AppendPart は、エンコードしたパート 1 つを dst に追加し、伸びたスライスを
// 返します。書き手がフレームをまたいで 1 つの作業バッファを使い回せます。
func (e MultipartEncoder) AppendPart(dst, jpeg []byte) []byte {
	dst = append(dst, "--"...)
	dst = append(dst, e.boundary...)
	dst = append(dst, "\r\n"...)
	dst = append(dst, "Content-Type: image/jpeg\r\n"...)
	dst = append(dst, "Content-Length: "...)
	dst = strconv.AppendInt(dst, int64(len(jpeg)), 10)
	dst = append(dst, "\r\n"...)
	for _, h := range e.extra {
		dst = append(dst, h.Name...)
		dst = append(dst, ": "...)
		dst = append(dst, h.Value...)
		dst = append(dst, "\r\n"...)
	}
	dst = append(dst, "\r\n"...)
	dst = append(dst, jpeg...)
	dst = append(dst, "\r\n"...)
	return dst
}

// EncodePart は、エンコードしたパート 1 つを新しいバッファとして返します。
func (e MultipartEncoder) EncodePart(jpeg []byte) []byte {
	return e.AppendPart(nil, jpeg)
}
