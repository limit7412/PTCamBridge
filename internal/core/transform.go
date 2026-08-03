package core

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
)

// DefaultQuality は、変換は有効だが JPEG 品質が明示されていないときに使う値です。
const DefaultQuality = 85

// DefaultMaxPixels は、デコードする前に許す画像の大きさの上限です。
//
// JPEG は自身の寸法を数バイトのヘッダーで宣言するので、圧縮後のバイト数を数える
// source.max_frame_size は、デコードが要求する量について何も語りません。1 KiB にも
// 満たない構造的に妥当なフレームが 65535x65535 を宣言し、デコーダにその分の
// バッファを先に確保させることができます。トラッキングカメラが送るのは 240x240
// なので、この上限は現実的な値のはるか上にありながら、悪意ある、あるいは壊れた
// 上流からの 1 フレームがメモリを食い尽くす域に達するのを防ぎます。
const DefaultMaxPixels = 16 << 20

// Transform は、ソースとストリームの間に挟む任意の幾何変換と再エンコードを
// 表します。ゼロ値は何もしない設定で、その場合は入力バイトをデコード・再エンコード
// せずそのまま流します。CPU コストと世代劣化の両方を避けられます。
type Transform struct {
	// Rotate は時計回りで、0・90・180・270 のいずれかでなければなりません。
	Rotate int
	// FlipH は左右反転、FlipV は上下反転です。どちらも切り抜きと回転の後に
	// 適用されます。
	FlipH bool
	FlipV bool
	// CropSquare は、回転の前に中央から最大の正方形を切り出します。
	CropSquare bool
	// Quality は再エンコード時の JPEG 品質で、1〜100 です。0 は「幾何変換で
	// 必要にならない限り再エンコードしない」という意味です。
	Quality int
	// MaxPixels は、これより大きい画像をデコード前に拒否します。0 なら
	// DefaultMaxPixels を使います。
	MaxPixels int
}

// Validate は、この変換が適用可能かどうかを返します。
func (t Transform) Validate() error {
	switch t.Rotate {
	case 0, 90, 180, 270:
	default:
		return fmt.Errorf("rotate must be 0, 90, 180 or 270, got %d", t.Rotate)
	}
	if t.Quality < 0 || t.Quality > 100 {
		return fmt.Errorf("quality must be between 0 and 100, got %d", t.Quality)
	}
	return nil
}

// IsNoop は、Apply が入力をそのまま返すかどうかを返します。
func (t Transform) IsNoop() bool {
	return t.Rotate == 0 && !t.FlipH && !t.FlipV && !t.CropSquare && t.Quality == 0
}

// Apply は、エンコード済みの JPEG に変換をかけ、エンコード済みの JPEG を返します。
// 何もしない変換の場合は入力スライスそのものを返します。
func (t Transform) Apply(src []byte) ([]byte, error) {
	if t.IsNoop() {
		return src, nil
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	// 先にヘッダーを読む。デコーダは宣言された寸法からバッファの大きさを決める
	// ので、上限を超えるものは Decode に確保させる前に追い返す必要がある。
	if err := t.checkSize(src); err != nil {
		return nil, err
	}
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if t.CropSquare {
		img = cropCentredSquare(img)
	}
	if t.Rotate != 0 {
		img = rotate(img, t.Rotate)
	}
	if t.FlipH || t.FlipV {
		img = flip(img, t.FlipH, t.FlipV)
	}
	quality := t.Quality
	if quality == 0 {
		quality = DefaultQuality
	}
	var out bytes.Buffer
	out.Grow(len(src))
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return out.Bytes(), nil
}

// DecodableJPEG は、src がクライアントで実際に描画できる JPEG かどうかを返します。
// 正しいマーカーで囲まれているだけのものとは違います。
//
// 上流のパーサーは構造を確認します。フレームごとの検査としてはそれが正解で、
// 安価であり、キャプチャ速度で許されるのはそこまでです。しかし構造という主張は
// 弱いものです。SOI の直後に EOI が来るペイロードは JPEG の形をしていて中身は
// 空であり、それしか送らないソースは最後まで健全に見えます — フレーム数は増え、
// /healthz は緑 — その間トラッカーは使えるものを何も受け取りません。それを捕まえる
// のがデコードなので、ソースが機能するかを決めるフレームに対して一度だけ行う
// 価値があります。
//
// maxPixels はデコードが要求してよい確保量の上限です。0 なら DefaultMaxPixels を
// 使います。
func DecodableJPEG(src []byte, maxPixels int) error {
	// Decode が確保する前に上限を適用するため、ヘッダーを先に読む。
	if err := checkImageSize(src, maxPixels); err != nil {
		return err
	}
	if _, err := jpeg.Decode(bytes.NewReader(src)); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// checkSize は JPEG のヘッダーだけを読み、そこに書かれた画像がデコードして
// よい大きさかどうかを返します。
func (t Transform) checkSize(src []byte) error {
	return checkImageSize(src, t.MaxPixels)
}

// WithinPixelLimit は JPEG のヘッダーだけを読み、そこに書かれた画像が
// 渡してよい大きさかどうかを返します。0 なら DefaultMaxPixels を使います。
//
// そのまま流すフレームはここでデコードされないので、小さなペイロードが巨大な画像を
// 宣言していても、こちら側では誰も気づきません。しかし受け取ったクライアントは
// それをデコードしなければならず、メモリを要求させられるのはそちらです。ヘッダーを
// 読むだけならキャプチャ速度でも十分に安いので、この上限は自プロセスがデコードする
// フレームだけでなく、すべてのフレームに適用する価値があります。
func WithinPixelLimit(src []byte, maxPixels int) error {
	return checkImageSize(src, maxPixels)
}

// checkImageSize は JPEG のヘッダーだけを読み、そこに書かれた画像がデコードして
// よい大きさかどうかを返します。
func checkImageSize(src []byte, maxPixels int) error {
	limit := maxPixels
	if limit <= 0 {
		limit = DefaultMaxPixels
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return fmt.Errorf("read the image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("image declares unusable dimensions %dx%d", cfg.Width, cfg.Height)
	}
	// 掛けずに割る。宣言された 2 つの寸法の積は、比較される前に溢れ得る。
	if cfg.Width > limit/cfg.Height {
		return fmt.Errorf("image is %dx%d, over the %d pixel limit", cfg.Width, cfg.Height, limit)
	}
	return nil
}

// remap は、各出力ピクセルを at が返す入力座標から引いてきて dstW x dstH の
// 画像を作ります。ここでの幾何操作はすべて座標の純粋な置換なので、この 1 つで
// 全部を賄えます。
func remap(src image.Image, dstW, dstH int, at func(dx, dy int) (int, int)) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	b := src.Bounds()
	for dy := 0; dy < dstH; dy++ {
		for dx := 0; dx < dstW; dx++ {
			sx, sy := at(dx, dy)
			dst.Set(dx, dy, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

// cropCentredSquare は、img の中央から取れる最大の正方形を返します。
func cropCentredSquare(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == h {
		return img
	}
	size := w
	if h < size {
		size = h
	}
	offX, offY := (w-size)/2, (h-size)/2
	return remap(img, size, size, func(dx, dy int) (int, int) {
		return offX + dx, offY + dy
	})
}

// rotate は、img を時計回りに 90・180・270 度回します。
func rotate(img image.Image, degrees int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	switch degrees {
	case 90:
		return remap(img, h, w, func(dx, dy int) (int, int) { return dy, h - 1 - dx })
	case 180:
		return remap(img, w, h, func(dx, dy int) (int, int) { return w - 1 - dx, h - 1 - dy })
	case 270:
		return remap(img, h, w, func(dx, dy int) (int, int) { return w - 1 - dy, dx })
	default:
		return img
	}
}

// flip は、img をいずれか、または両方の軸で反転します。
func flip(img image.Image, horizontal, vertical bool) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	return remap(img, w, h, func(dx, dy int) (int, int) {
		sx, sy := dx, dy
		if horizontal {
			sx = w - 1 - dx
		}
		if vertical {
			sy = h - 1 - dy
		}
		return sx, sy
	})
}
