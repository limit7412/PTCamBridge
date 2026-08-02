package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultBoundary is the multipart delimiter PaperBridge advertises.
const DefaultBoundary = "paperbridge"

// ErrInvalidBoundary is returned for a boundary that cannot be written into a
// Content-Type header or a delimiter line.
var ErrInvalidBoundary = errors.New("invalid multipart boundary")

// StreamHeader is an extra part header appended to every frame. The PaperTracker
// client is closed source and its parser has changed between releases, so the
// header set is configurable rather than fixed.
type StreamHeader struct {
	Name  string
	Value string
}

// MultipartEncoder renders frames in the multipart/x-mixed-replace form the
// PaperTracker client expects:
//
//	--boundary\r\n
//	Content-Type: image/jpeg\r\n
//	Content-Length: N\r\n
//	\r\n
//	<N bytes of JPEG>\r\n
//
// Content-Length is mandatory: the client sizes each frame from it, and
// omitting it makes the client lose frame boundaries. Chunked transfer
// encoding is never used; the body is written as an unbounded stream.
type MultipartEncoder struct {
	boundary string
	extra    []StreamHeader
}

// NewMultipartEncoder validates the boundary and returns an encoder for it. An
// empty boundary selects DefaultBoundary.
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

// reservedPartHeaders are the part headers the encoder writes itself. A second
// copy with a different value would leave the client choosing between them,
// and picking the wrong Content-Length loses the frame boundary for good.
var reservedPartHeaders = []string{"Content-Type", "Content-Length"}

// ValidateStreamHeader reports whether a name and value are usable as an extra
// part header.
func ValidateStreamHeader(name, value string) error {
	if name == "" || strings.ContainsAny(name, ":\r\n") {
		return fmt.Errorf("invalid extra header name %q", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid extra header value for %q", name)
	}
	for _, reserved := range reservedPartHeaders {
		if strings.EqualFold(strings.TrimSpace(name), reserved) {
			return fmt.Errorf("extra header %q is written by the encoder itself and cannot be overridden", name)
		}
	}
	return nil
}

// ValidateBoundary reports whether s is usable as a multipart delimiter.
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

// Boundary reports the delimiter in use.
func (e MultipartEncoder) Boundary() string { return e.boundary }

// ContentType is the response Content-Type for the stream endpoint.
//
// RFC 2046 allows delimiter characters that RFC 2045 does not allow in a bare
// parameter token, so such a boundary is written as a quoted string. Left
// unquoted, a value like "a:b" would make the whole media type unparsable and
// a standards-compliant client could not find the delimiter at all.
func (e MultipartEncoder) ContentType() string {
	if strings.ContainsAny(e.boundary, nonTokenBoundaryChars) {
		// ValidateBoundary rejects '"' and '\', so nothing needs escaping.
		return `multipart/x-mixed-replace; boundary="` + e.boundary + `"`
	}
	return "multipart/x-mixed-replace; boundary=" + e.boundary
}

// nonTokenBoundaryChars are the characters ValidateBoundary accepts that are
// not RFC 2045 token characters, and so force a quoted parameter.
const nonTokenBoundaryChars = "()/:=?,"

// AppendPart appends one encoded part to dst and returns the extended slice,
// letting a writer reuse a single scratch buffer across frames.
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

// EncodePart returns one encoded part as a fresh buffer.
func (e MultipartEncoder) EncodePart(jpeg []byte) []byte {
	return e.AppendPart(nil, jpeg)
}
