package receive_http

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"slices"
	"strings"

	"github.com/klauspost/compress/gzip"
)

// recompressIngestBody streams an /ingest request body into w, re-gzipping
// the profile data at the given gzip level (1-9) as it arrives. Data that is
// already gzipped is inflated on the fly first; raw data is gzipped as-is. Data
// shorter than minRecompressSize is copied through unchanged because gzip would
// only make it larger.
//
// For multipart bodies (the JFR /ingest format) every part is re-gzipped
// independently, keeping the part headers, the boundary and therefore the
// request Content-Type intact. This matches how the Pyroscope server detects
// compression on ingest: it sniffs the gzip magic bytes of each part rather
// than honouring a Content-Encoding header. Any other body is re-gzipped as a
// whole.
//
// contentType holds the request's Content-Type header values and is returned
// unchanged, except when the original multipart boundary cannot be reused by
// multipart.Writer; then only the boundary parameter of the first value is
// replaced and all other parameters are preserved.
func recompressIngestBody(w io.Writer, body io.Reader, contentType []string, level int) ([]string, error) {
	ct := ""
	if len(contentType) > 0 {
		ct = contentType[0]
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return contentType, regzip(w, body, level)
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("multipart content type without boundary")
	}
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary); err != nil {
		params["boundary"] = mw.Boundary()
		contentType = slices.Clone(contentType)
		contentType[0] = mime.FormatMediaType(mediaType, params)
	}
	mr := multipart.NewReader(body, boundary)
	for {
		part, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart part: %w", err)
		}
		fw, err := mw.CreatePart(part.Header)
		if err != nil {
			return nil, fmt.Errorf("create multipart part: %w", err)
		}
		err = regzip(fw, part, level)
		_ = part.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}
	return contentType, nil
}

// minRecompressSize is the smallest input, in bytes, that regzip compresses.
// A gzip stream carries 18 bytes of header and trailer plus block framing, so
// shorter inputs (such as an empty or tiny multipart part) would only grow.
const minRecompressSize = 64

// regzip gzips r into w at the given level. If r is itself a gzip stream it is
// inflated on the fly, so the data is streamed rather than held fully
// decompressed in memory. Inputs shorter than minRecompressSize are copied
// through unchanged.
func regzip(w io.Writer, r io.Reader, level int) error {
	br := bufio.NewReader(r)
	head, _ := br.Peek(minRecompressSize)
	if len(head) < minRecompressSize {
		_, err := io.Copy(w, br)
		return err
	}
	var src io.Reader = br
	// Sniff the two-byte gzip magic (RFC 1952).
	if bytes.HasPrefix(head, []byte{0x1f, 0x8b}) {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("open gzip reader: %w", err)
		}
		defer zr.Close()
		src = zr
	}
	zw, err := gzip.NewWriterLevel(w, level)
	if err != nil {
		return fmt.Errorf("open gzip writer (level %d): %w", level, err)
	}
	if _, err := io.Copy(zw, src); err != nil {
		_ = zw.Close()
		return fmt.Errorf("re-compress: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close gzip writer: %w", err)
	}
	return nil
}
