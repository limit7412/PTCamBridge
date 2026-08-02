package core

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

// Errors reported by ScanJPEG.
var (
	// ErrIncompleteJPEG means the buffer holds the start of a valid image but
	// not all of it; the caller should read more bytes and retry.
	ErrIncompleteJPEG = errors.New("incomplete JPEG")
	// ErrNotJPEG means the buffer does not begin with a parsable image, so the
	// caller must resynchronise rather than wait for more data.
	ErrNotJPEG = errors.New("not a JPEG")
)

// ScanJPEG returns the length of the complete JPEG image starting at buf[0].
//
// It walks the marker structure rather than searching for the EOI byte pair,
// because a raw 0xFF 0xD9 also occurs inside an EXIF thumbnail carried in an
// APPn segment. Segments are skipped by their declared length and
// entropy-coded data is skipped by honouring byte stuffing, so only the real
// end-of-image terminates the scan.
//
// A maxSize of zero selects DefaultMaxFrameSize.
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
			return 0, ErrNotJPEG
		}
		if i >= len(buf) {
			return 0, ErrIncompleteJPEG
		}
		if buf[i] != markerPrefix {
			return 0, ErrNotJPEG
		}
		// Any number of 0xFF fill bytes may precede a marker.
		for i < len(buf) && buf[i] == markerPrefix {
			i++
		}
		// The run above is unbounded, so the ceiling has to be rechecked
		// after it: a stream of fill bytes ending in EOI would otherwise
		// return a length far past maxSize and hand the caller a frame the
		// limit was supposed to have stopped.
		if i > maxSize {
			return 0, ErrNotJPEG
		}
		if i >= len(buf) {
			return 0, ErrIncompleteJPEG
		}
		marker := buf[i]
		i++

		switch {
		case marker == markerEOI:
			if i > maxSize {
				return 0, ErrNotJPEG
			}
			return i, nil
		case marker == markerSOI, marker == markerTEM, marker == 0x00,
			marker >= markerRST0 && marker <= markerRST7:
			// Standalone marker: no length field follows.
			continue
		}

		if i+2 > len(buf) {
			return 0, ErrIncompleteJPEG
		}
		segLen := int(buf[i])<<8 | int(buf[i+1])
		if segLen < 2 {
			return 0, ErrNotJPEG
		}
		i += segLen
		if i > maxSize {
			return 0, ErrNotJPEG
		}
		if marker != markerSOS {
			continue
		}

		// Scan the entropy-coded data that follows a start-of-scan segment.
		// 0xFF 0x00 is a stuffed literal and restart markers are part of the
		// scan, so neither ends it.
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
				// Real marker: hand it back to the outer loop.
				goto nextMarker
			}
			if i > maxSize {
				return 0, ErrNotJPEG
			}
		}
	nextMarker:
	}
}

// SplitJPEGStream extracts consecutive images from a bare MJPEG byte stream,
// the shape ffmpeg writes to stdout with `-f mjpeg pipe:1`.
//
// Frames are copies and safe to retain; rest aliases buf. Leading bytes that
// do not begin an image are discarded, so a reader may join mid-stream.
func SplitJPEGStream(buf []byte, maxSize int) (frames [][]byte, rest []byte) {
	if maxSize <= 0 {
		maxSize = DefaultMaxFrameSize
	}
	soi := []byte{markerPrefix, markerSOI}
	pos := 0
	for {
		idx := bytes.Index(buf[pos:], soi)
		if idx < 0 {
			// Keep a single byte in case an SOI straddles two reads.
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
				// Runaway: this SOI cannot be the start of a real frame.
				pos = start + 2
				continue
			}
			return frames, buf[start:]
		default:
			pos = start + 2
		}
	}
}

// maxPartHeaderBytes bounds the header block of a single multipart part. A
// part whose headers are not terminated within this much data is treated as a
// false delimiter match rather than as a part still arriving, which stops a
// broken upstream from growing the reader's buffer without limit.
const maxPartHeaderBytes = 8 << 10

// SplitMultipart extracts JPEG frames from a multipart/x-mixed-replace body.
//
// Content-Length is honoured when the upstream part provides it. Otherwise the
// body is measured by walking the JPEG structure, which keeps the reader
// working against firmware that omits the header. Parts whose payload is not a
// valid JPEG are dropped rather than forwarded.
//
// Frames are copies and safe to retain; rest aliases buf.
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
			// Closing delimiter: the stream is over.
			return frames, nil
		}

		hdrLen, bodyOff, ok := findHeaderEnd(buf[afterDelim:])
		if !ok || hdrLen > maxPartHeaderBytes {
			if len(buf)-afterDelim > maxPartHeaderBytes {
				// Either these bytes are not a part header at all or the
				// upstream is malformed; resynchronise past this delimiter
				// instead of buffering everything that follows it.
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
			if bodyAt+n > len(buf) {
				return frames, buf[start:]
			}
			body = buf[bodyAt : bodyAt+n]
			pos = bodyAt + n
		} else {
			// Without Content-Length the end of the image has to be found by
			// walking its marker structure. Searching for the next delimiter
			// instead would truncate any frame that happens to contain the
			// boundary bytes inside entropy-coded data or an EXIF blob.
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
				// Not an image. Skip the part by finding the delimiter that
				// ends it, anchored to the start of a line as MIME requires.
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

// indexDelimiter finds the next multipart delimiter that begins a line, and
// returns the offset of the line break in front of it. A plain search would
// also match the same bytes occurring inside a part body, which MIME does not
// allow a delimiter to be.
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

// findHeaderEnd locates the blank line separating part headers from the part
// body. buf starts at the newline that ends the boundary line. It returns the
// header block length and the offset at which the body begins, accepting both
// CRLF and bare LF so that loosely written firmware still parses.
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

// contentLength reads the Content-Length value out of a raw part header block.
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

// BoundaryFromContentType pulls the boundary parameter out of a
// multipart/x-mixed-replace content type. It returns false for content types
// that are not multipart or that omit the parameter.
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
		// Some firmware writes the leading dashes into the parameter itself.
		value = strings.TrimPrefix(value, "--")
		if value == "" {
			return "", false
		}
		return value, true
	}
	return "", false
}
