package core

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

// ScanJPEG が報告するエラー。
var (
	// ErrIncompleteJPEG は、バッファが妥当な画像の先頭は持っているが全体は
	// まだ揃っていないという意味です。呼び出し側はもっと読んでから再試行します。
	ErrIncompleteJPEG = errors.New("incomplete JPEG")
	// ErrNotJPEG は、バッファの先頭が解析可能な画像で始まっていないという意味です。
	// 呼び出し側はデータを待つのではなく再同期しなければなりません。
	ErrNotJPEG = errors.New("not a JPEG")
)

// ScanJPEG は、buf[0] から始まる完結した JPEG 画像の長さを返します。
//
// EOI のバイト対を探すのではなくマーカー構造をたどります。生の 0xFF 0xD9 は、
// APPn セグメントに載る EXIF のサムネイル内にも現れるからです。セグメントは
// 宣言された長さで読み飛ばし、エントロピー符号化データはバイトスタッフィングを
// 尊重して読み飛ばすので、走査を終わらせるのは本物の画像終端だけです。
//
// maxSize が 0 なら DefaultMaxFrameSize を使います。
//
// ErrNotJPEG のときに返す長さは、走査が諦めるまでに進んだ距離です。再同期する
// 呼び出し側が、数バイト先からやり直すのではなく、すでに否定された範囲を飛ばせる
// ようにするためです。ErrIncompleteJPEG のときは 0 です。何も否定されておらず、
// 単にバイトが届いていないだけだからです。
func ScanJPEG(buf []byte, maxSize int) (int, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxFrameSize
	}
	if len(buf) < 2 {
		return 0, ErrIncompleteJPEG
	}
	if buf[0] != markerPrefix || buf[1] != markerSOI {
		return 0, ErrNotJPEG
	}
	i := 2
	for {
		if i > maxSize {
			return i, ErrNotJPEG
		}
		if i >= len(buf) {
			return 0, ErrIncompleteJPEG
		}
		if buf[i] != markerPrefix {
			return i, ErrNotJPEG
		}
		// マーカーの前には 0xFF の詰め物が何個あってもよい。
		for i < len(buf) && buf[i] == markerPrefix {
			i++
		}
		// 上のループには上限が無いので、その後で改めて確認する必要がある。
		// そうしないと、詰め物の列の末尾が EOI であるようなストリームが
		// maxSize をはるかに超える長さを返し、上限が止めるはずだったフレームを
		// 呼び出し側に渡してしまう。
		if i > maxSize {
			return i, ErrNotJPEG
		}
		if i >= len(buf) {
			return 0, ErrIncompleteJPEG
		}
		marker := buf[i]
		i++

		switch {
		case marker == markerEOI:
			if i > maxSize {
				return i, ErrNotJPEG
			}
			return i, nil
		case marker == markerSOI:
			// この階層で画像が別の画像を含むことはない。サムネイルは APPn
			// セグメントの中にあり、それは宣言された長さで丸ごと飛ばされるので、
			// 走査がその上を歩くことはない。2 つ目の SOI を無害として扱うと、
			// 呼び出し側がやり直そうとしている走査の途中に再同期点を作ることに
			// なる。
			//
			// 返す長さはマーカーを含めず、その手前で止める。前のフレームが
			// 途中で切れていた場合、ここが次のフレームの始まりであり、再同期
			// する側は 2 バイト先ではなくここに着地しなければならない。
			return i - 2, ErrNotJPEG
		case marker == markerTEM, marker == 0x00,
			marker >= markerRST0 && marker <= markerRST7:
			// 単独マーカー。長さフィールドは続かない。
			continue
		}

		if i+2 > len(buf) {
			return 0, ErrIncompleteJPEG
		}
		segLen := int(buf[i])<<8 | int(buf[i+1])
		if segLen < 2 {
			return i, ErrNotJPEG
		}
		i += segLen
		if i > maxSize {
			return i, ErrNotJPEG
		}
		if marker != markerSOS {
			continue
		}

		// SOS セグメントに続くエントロピー符号化データを走査する。0xFF 0x00 は
		// スタッフィングされたリテラルであり、リスタートマーカーも走査の一部
		// なので、どちらもこれを終わらせない。
		for {
			if i >= len(buf) {
				return 0, ErrIncompleteJPEG
			}
			if buf[i] != markerPrefix {
				i++
				continue
			}
			if i+1 >= len(buf) {
				return 0, ErrIncompleteJPEG
			}
			next := buf[i+1]
			switch {
			case next == 0x00:
				i += 2
			case next == markerPrefix:
				i++
			case next >= markerRST0 && next <= markerRST7:
				i += 2
			default:
				// 本物のマーカー。外側のループに戻す。
				goto nextMarker
			}
			if i > maxSize {
				return i, ErrNotJPEG
			}
		}
	nextMarker:
	}
}

// SplitJPEGStream は、裸の MJPEG バイトストリーム — ffmpeg が
// `-f mjpeg pipe:1` で標準出力に書く形 — から連続する画像を取り出します。
//
// フレームはコピーなので保持して構いません。rest は buf を指しています。画像の
// 始まりでない先頭バイトは捨てるので、途中から読み始めることができます。
func SplitJPEGStream(buf []byte, maxSize int) (frames [][]byte, rest []byte) {
	if maxSize <= 0 {
		maxSize = DefaultMaxFrameSize
	}
	soi := []byte{markerPrefix, markerSOI}
	pos := 0
	for {
		idx := bytes.Index(buf[pos:], soi)
		if idx < 0 {
			// SOI が 2 回の読み取りにまたがる場合に備えて 1 バイトだけ残す。
			keep := 1
			if keep > len(buf)-pos {
				keep = len(buf) - pos
			}
			return frames, buf[len(buf)-keep:]
		}
		start := pos + idx
		n, err := ScanJPEG(buf[start:], maxSize)
		switch {
		case err == nil:
			frames = append(frames, bytes.Clone(buf[start:start+n]))
			pos = start + n
		case errors.Is(err, ErrIncompleteJPEG):
			if len(buf)-start > maxSize {
				// 際限がない。この SOI が本物のフレームの先頭であるはずがない。
				pos = start + 2
				continue
			}
			return frames, buf[start:]
		default:
			// 失敗した走査が歩いた範囲を丸ごと飛ばして再同期する。2 バイト先
			// ではない。その内側を走査し直すことが、ゴミを送る上流に対して
			// 計算量を二乗にする原因になる。バッファは数バイトごとに新しい SOI を
			// 含み得て、その一つひとつが再び上限まで走査されるので、ドライバは
			// 1 回の読み取りの上で回り続け、コンテキストがキャンセルされたことにも
			// 気づかない。走査した長さだけ進めれば、各バイトの総コストは 1 歩で済む。
			//
			// 代わりに諦めるのは、「無効なものの一部だと既に分かったバイト列の
			// 内側から始まる画像」を見つけることだ。実際のストリームではそれは
			// フレーム間のゴミであり、試す価値があるのはその次の SOI の方だ。
			pos = min(start+max(n, 2), len(buf))
		}
	}
}

// maxPartHeaderBytes は、multipart のパート 1 つ分のヘッダーブロックの上限です。
// これだけ読んでもヘッダーが終端しないパートは、まだ到着中のパートではなく
// 区切りの誤検出として扱います。壊れた上流が読み手のバッファを無制限に太らせる
// のを防ぐためです。
const maxPartHeaderBytes = 8 << 10

// SplitMultipart は、multipart/x-mixed-replace のボディから JPEG フレームを
// 取り出します。
//
// 上流のパートが Content-Length を付けていればそれに従います。無ければ JPEG の
// 構造をたどって本体の長さを測るので、そのヘッダーを省くファームウェア相手でも
// 読み続けられます。ペイロードが正しい JPEG でないパートは、流さずに捨てます。
//
// フレームはコピーなので保持して構いません。rest は buf を指しています。
func SplitMultipart(buf []byte, boundary string, maxSize int) (frames [][]byte, rest []byte) {
	if maxSize <= 0 {
		maxSize = DefaultMaxFrameSize
	}
	delim := []byte("--" + boundary)
	pos := 0
	for {
		idx := bytes.Index(buf[pos:], delim)
		if idx < 0 {
			keep := len(delim) - 1
			if keep > len(buf)-pos {
				keep = len(buf) - pos
			}
			return frames, buf[len(buf)-keep:]
		}
		start := pos + idx
		afterDelim := start + len(delim)
		if afterDelim+2 > len(buf) {
			return frames, buf[start:]
		}
		if buf[afterDelim] == '-' && buf[afterDelim+1] == '-' {
			// 終端の区切り。ストリームはここで終わり。
			return frames, nil
		}

		hdrLen, bodyOff, ok := findHeaderEnd(buf[afterDelim:])
		if !ok || hdrLen > maxPartHeaderBytes {
			if len(buf)-afterDelim > maxPartHeaderBytes {
				// これらのバイトがそもそもパートヘッダーでないか、上流が
				// 壊れているかのどちらか。この区切りより先へ再同期し、後続を
				// すべてバッファに溜めることはしない。
				pos = afterDelim
				continue
			}
			return frames, buf[start:]
		}
		headers := buf[afterDelim : afterDelim+hdrLen]
		bodyAt := afterDelim + bodyOff

		var body []byte
		if n, hasCL := contentLength(headers); hasCL {
			if n < MinJPEGSize || n > maxSize {
				pos = afterDelim
				continue
			}
			// 足さずに引く。bodyAt+n は攻撃者の影響を受ける 2 つの数であり、
			// MaxInt 付近の Content-Length はこれを負に回り込ませる。それは
			// この検査を通過し、下のスライスで panic する。bodyAt は buf への
			// 添字なので、len(buf)-bodyAt が回り込むことはない。
			if n > len(buf)-bodyAt {
				return frames, buf[start:]
			}
			body = buf[bodyAt : bodyAt+n]
			pos = bodyAt + n
		} else {
			// Content-Length が無ければ、画像の終端はマーカー構造をたどって
			// 見つけるしかない。代わりに次の区切りを探すと、エントロピー符号化
			// データや EXIF の中にたまたま boundary のバイト列を含むフレームが
			// 切り詰められてしまう。
			n, err := ScanJPEG(buf[bodyAt:], maxSize)
			switch {
			case err == nil:
				body = buf[bodyAt : bodyAt+n]
				pos = bodyAt + n
			case errors.Is(err, ErrIncompleteJPEG):
				if len(buf)-bodyAt > maxSize {
					pos = afterDelim
					continue
				}
				return frames, buf[start:]
			default:
				// 画像ではない。MIME が要求するとおり行頭に固定して、この
				// パートを終わらせる区切りを探し、パートごと飛ばす。
				nidx := indexDelimiter(buf[bodyAt:], delim)
				if nidx < 0 {
					if len(buf)-bodyAt > maxSize {
						pos = afterDelim
						continue
					}
					return frames, buf[start:]
				}
				pos = bodyAt + nidx
				continue
			}
		}

		if ValidateJPEG(body, maxSize) == nil {
			frames = append(frames, bytes.Clone(body))
		}
	}
}

// indexDelimiter は、行頭から始まる次の multipart 区切りを探し、その手前の
// 改行の位置を返します。単純に検索すると、パート本体の中に現れた同じバイト列にも
// 一致してしまいますが、MIME はそれを区切りとは認めません。
func indexDelimiter(buf, delim []byte) int {
	for off := 0; off < len(buf); {
		idx := bytes.Index(buf[off:], delim)
		if idx < 0 {
			return -1
		}
		at := off + idx
		switch {
		case at >= 2 && buf[at-2] == '\r' && buf[at-1] == '\n':
			return at - 2
		case at >= 1 && buf[at-1] == '\n':
			return at - 1
		}
		off = at + 1
	}
	return -1
}

// findHeaderEnd は、パートヘッダーと本体を隔てる空行を見つけます。buf は
// boundary 行を終わらせる改行から始まります。ヘッダーブロックの長さと本体の
// 開始位置を返します。CRLF と裸の LF の両方を受け入れるので、緩い書き方の
// ファームウェアでも解析できます。
func findHeaderEnd(buf []byte) (hdrLen, bodyOff int, ok bool) {
	crlf := bytes.Index(buf, []byte("\r\n\r\n"))
	lf := bytes.Index(buf, []byte("\n\n"))
	switch {
	case crlf >= 0 && (lf < 0 || crlf <= lf):
		return crlf, crlf + 4, true
	case lf >= 0:
		return lf, lf + 2, true
	default:
		return 0, 0, false
	}
}

// contentLength は、生のパートヘッダーブロックから Content-Length の値を読みます。
func contentLength(headers []byte) (int, bool) {
	for _, line := range strings.Split(string(headers), "\n") {
		line = strings.TrimRight(line, "\r")
		name, value, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// BoundaryFromContentType は、multipart/x-mixed-replace のコンテンツタイプから
// boundary パラメータを取り出します。multipart でないもの、パラメータが無いものに
// 対しては false を返します。
func BoundaryFromContentType(ct string) (string, bool) {
	mediaType, params, found := strings.Cut(ct, ";")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return "", false
	}
	if !found {
		return "", false
	}
	for _, param := range strings.Split(params, ";") {
		name, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "boundary") {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)
		// ファームウェアによっては、先頭のダッシュをパラメータ自体に書いてくる。
		value = strings.TrimPrefix(value, "--")
		if value == "" {
			return "", false
		}
		return value, true
	}
	return "", false
}
