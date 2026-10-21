package receive_http

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/stretchr/testify/require"

	"github.com/grafana/alloy/syntax"
)

// testPayload is a compressible payload resembling a profile: repeated
// symbol-like strings.
func testPayload() []byte {
	var b bytes.Buffer
	for i := 0; i < 4000; i++ {
		b.WriteString("com.example.service.HandlerImpl.processRequest(HandlerImpl.java:123)\n")
	}
	return b.Bytes()
}

// recompress runs recompressIngestBody over an in-memory body.
func recompress(t testing.TB, body []byte, contentType []string, level int) ([]byte, []string, error) {
	t.Helper()
	var buf bytes.Buffer
	ct, err := recompressIngestBody(&buf, bytes.NewReader(body), contentType, level)
	return buf.Bytes(), ct, err
}

func gzipAt(t testing.TB, raw []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	require.NoError(t, err)
	_, err = zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// truncatedGzip returns a gzip stream that is missing its trailer, which is
// larger than minRecompressSize but fails to inflate.
func truncatedGzip(t testing.TB) []byte {
	t.Helper()
	gz := gzipAt(t, testPayload(), gzip.BestSpeed)
	require.Greater(t, len(gz), minRecompressSize+8)
	return gz[:len(gz)-8]
}

// isGzipData reports whether b starts with the gzip magic bytes.
func isGzipData(b []byte) bool {
	return bytes.HasPrefix(b, []byte{0x1f, 0x8b})
}

// gunzip inflates b, which must be a gzip stream.
func gunzip(t testing.TB, b []byte) []byte {
	t.Helper()
	require.True(t, isGzipData(b), "data is not gzipped")
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return out
}

type testPart struct {
	header textproto.MIMEHeader
	data   []byte
}

func formFilePart(field string, data []byte) testPart {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="`+field+`"; filename="`+field+`"`)
	h.Set("Content-Type", "application/octet-stream")
	return testPart{header: h, data: data}
}

// buildMultipartBody builds a multipart body from the given parts, using the
// given boundary and media type parameters for the content type.
func buildMultipartBody(t testing.TB, mediaType, boundary string, params map[string]string, parts ...testPart) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.SetBoundary(boundary))
	for _, p := range parts {
		fw, err := mw.CreatePart(p.header)
		require.NoError(t, err)
		_, err = fw.Write(p.data)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	all := map[string]string{"boundary": boundary}
	for k, v := range params {
		all[k] = v
	}
	return buf.Bytes(), mime.FormatMediaType(mediaType, all)
}

// buildJFRMultipart builds a JFR-style multipart body: a "jfr" part and a
// "labels" part, each gzipped at the given level, mirroring the pyroscope-java
// agent.
func buildJFRMultipart(t testing.TB, jfr, labels []byte, level int) (body []byte, contentType string) {
	t.Helper()
	return buildMultipartBody(t, "multipart/form-data", "jfr-boundary-123", nil,
		formFilePart("jfr", gzipAt(t, jfr, level)),
		formFilePart("labels", gzipAt(t, labels, level)),
	)
}

// readParts reads all parts of a multipart body using the boundary of the given
// content type, without decoding the part data.
func readParts(t testing.TB, body []byte, contentType string) []testPart {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var parts []testPart
	for {
		part, err := mr.NextRawPart()
		if err == io.EOF {
			return parts
		}
		require.NoError(t, err)
		raw, err := io.ReadAll(part)
		require.NoError(t, err)
		parts = append(parts, testPart{header: part.Header, data: raw})
	}
}

// decodePart reads the named part and gunzips it, as the Pyroscope server does
// after sniffing the gzip magic.
func decodePart(t testing.TB, body []byte, contentType, field string) []byte {
	t.Helper()
	for _, p := range readParts(t, body, contentType) {
		_, dp, err := mime.ParseMediaType(p.header.Get("Content-Disposition"))
		require.NoError(t, err)
		if dp["name"] == field {
			return gunzip(t, p.data)
		}
	}
	t.Fatalf("part %q not found", field)
	return nil
}

func TestRecompressIngestBody_MultipartRoundTrip(t *testing.T) {
	jfr := testPayload()
	labels := []byte("labels-protobuf-bytes")

	// Client sends gzip level 1 (pyroscope-java default).
	body, ct := buildJFRMultipart(t, jfr, labels, gzip.BestSpeed)
	require.Equal(t, jfr, decodePart(t, body, ct, "jfr"), "test harness: jfr part did not round-trip before recompression")

	// Alloy recompresses at level 9.
	out, outCT, err := recompress(t, body, []string{ct}, gzip.BestCompression)
	require.NoError(t, err)

	// The content type, including the boundary, is unchanged.
	require.Equal(t, []string{ct}, outCT)

	// Server-side decode must yield the original bytes for both parts.
	require.Equal(t, jfr, decodePart(t, out, ct, "jfr"))
	require.Equal(t, labels, decodePart(t, out, ct, "labels"))

	require.Less(t, len(out), len(body), "expected level 9 to shrink the level-1 body")
	t.Logf("multipart recompress gz1->gz9: %d -> %d bytes (%.1f%% smaller)",
		len(body), len(out), (1-float64(len(out))/float64(len(body)))*100)
}

func TestRecompressIngestBody_MultipartAllPartsGzipped(t *testing.T) {
	// A mix of raw and already gzipped parts: every part is gzipped at the
	// requested level on the way out.
	jfr := testPayload()
	labels := testPayload()[:200]
	body, ct := buildMultipartBody(t, "multipart/form-data", "mixed-boundary", nil,
		formFilePart("jfr", jfr),                                  // raw
		formFilePart("labels", gzipAt(t, labels, gzip.BestSpeed)), // gzipped
	)

	out, outCT, err := recompress(t, body, []string{ct}, gzip.BestCompression)
	require.NoError(t, err)
	require.Equal(t, []string{ct}, outCT)

	parts := readParts(t, out, ct)
	require.Len(t, parts, 2)
	for _, p := range parts {
		require.True(t, isGzipData(p.data), "part %q is not gzipped", p.header.Get("Content-Disposition"))
	}
	require.Equal(t, jfr, decodePart(t, out, ct, "jfr"))
	require.Equal(t, labels, decodePart(t, out, ct, "labels"))
	require.Less(t, len(out), len(body), "expected gzipping the raw part to shrink the body")
}

func TestRecompressIngestBody_MultipartPreservesHeaders(t *testing.T) {
	// Extra content type parameters, a non form-data subtype, custom part
	// headers and additional Content-Type header values must all survive
	// recompression untouched.
	custom := textproto.MIMEHeader{}
	custom.Set("Content-Disposition", `form-data; name="jfr"; filename="profile.jfr"`)
	custom.Set("Content-Type", "application/x-jfr")
	custom.Set("X-Custom-Part-Header", "keep-me")
	custom.Add("X-Custom-Part-Header", "and-me")
	body, ct := buildMultipartBody(t, "multipart/mixed", "my.boundary_42", map[string]string{"charset": "utf-8"},
		testPart{header: custom, data: gzipAt(t, testPayload(), gzip.BestSpeed)},
	)
	require.Contains(t, ct, "multipart/mixed")
	require.Contains(t, ct, "charset=utf-8")

	out, outCT, err := recompress(t, body, []string{ct, "ct2"}, gzip.BestCompression)
	require.NoError(t, err)
	require.Equal(t, []string{ct, "ct2"}, outCT)

	inParts := readParts(t, body, ct)
	outParts := readParts(t, out, ct)
	require.Len(t, outParts, len(inParts))
	for i := range inParts {
		require.Equal(t, inParts[i].header, outParts[i].header)
		require.Equal(t, gunzip(t, inParts[i].data), gunzip(t, outParts[i].data))
	}
}

func TestRecompressIngestBody_MultipartBoundaryFallback(t *testing.T) {
	// "!" is a valid MIME token character, so the content type parses and the
	// multipart reader accepts the boundary, but multipart.Writer refuses to
	// reuse it. Only the boundary parameter of the first Content-Type value may
	// change in that case.
	const boundary = "odd!boundary"
	var buf bytes.Buffer
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="jfr"` + "\r\n\r\n")
	buf.Write(gzipAt(t, testPayload(), gzip.BestSpeed))
	buf.WriteString("\r\n--" + boundary + "--\r\n")
	ct := "multipart/form-data; boundary=" + boundary + "; charset=utf-8"
	in := []string{ct, "ct2"}

	out, outCT, err := recompress(t, buf.Bytes(), in, gzip.BestCompression)
	require.NoError(t, err)
	require.Equal(t, []string{ct, "ct2"}, in, "input must not be mutated")
	require.Len(t, outCT, 2)
	require.NotEqual(t, ct, outCT[0])
	require.Equal(t, "ct2", outCT[1])

	mediaType, params, err := mime.ParseMediaType(outCT[0])
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	require.Equal(t, "utf-8", params["charset"])
	require.NotEqual(t, boundary, params["boundary"])
	require.Equal(t, testPayload(), decodePart(t, out, outCT[0], "jfr"))
}

func TestRecompressIngestBody_MultipartErrors(t *testing.T) {
	t.Run("missing boundary", func(t *testing.T) {
		_, _, err := recompress(t, []byte("whatever"), []string{"multipart/form-data"}, gzip.BestCompression)
		require.ErrorContains(t, err, "boundary")
	})
	t.Run("corrupt gzip part", func(t *testing.T) {
		body, ct := buildMultipartBody(t, "multipart/form-data", "b", nil, formFilePart("jfr", truncatedGzip(t)))
		_, _, err := recompress(t, body, []string{ct}, gzip.BestCompression)
		require.Error(t, err)
	})
	t.Run("truncated body", func(t *testing.T) {
		body, ct := buildJFRMultipart(t, testPayload(), []byte("l"), gzip.BestSpeed)
		_, _, err := recompress(t, body[:len(body)/2], []string{ct}, gzip.BestCompression)
		require.Error(t, err)
	})
}

func TestRecompressIngestBody_NonMultipart(t *testing.T) {
	raw := testPayload()
	for _, tc := range []struct {
		name        string
		body        []byte
		contentType []string
	}{
		{name: "gzipped", body: gzipAt(t, raw, gzip.BestSpeed), contentType: []string{"application/octet-stream"}},
		{name: "raw", body: raw, contentType: []string{"application/octet-stream"}},
		{name: "raw with several content type values", body: raw, contentType: []string{"ct1", "ct2"}},
		{name: "raw without content type", body: raw, contentType: nil},
		{name: "raw with unparsable content type", body: raw, contentType: []string{"not/a;valid=;"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, outCT, err := recompress(t, tc.body, tc.contentType, gzip.BestCompression)
			require.NoError(t, err)
			require.Equal(t, tc.contentType, outCT, "content type must be unchanged")
			require.Equal(t, raw, gunzip(t, out))
			require.Less(t, len(out), len(tc.body))
		})
	}
}

func TestRecompressIngestBody_SmallDataPassesThrough(t *testing.T) {
	small := []byte("tiny-labels")
	smallGz := gzipAt(t, []byte("x"), gzip.BestSpeed)
	require.Less(t, len(small), minRecompressSize)
	require.Less(t, len(smallGz), minRecompressSize)
	ct := []string{"application/octet-stream"}

	t.Run("multipart parts", func(t *testing.T) {
		jfr := testPayload()
		body, ct := buildMultipartBody(t, "multipart/form-data", "b", nil,
			formFilePart("jfr", jfr),
			formFilePart("labels", small),
			formFilePart("gzlabels", smallGz),
			formFilePart("empty", nil),
		)
		out, outCT, err := recompress(t, body, []string{ct}, gzip.BestCompression)
		require.NoError(t, err)
		require.Equal(t, []string{ct}, outCT)

		parts := readParts(t, out, ct)
		require.Len(t, parts, 4)
		require.Equal(t, jfr, gunzip(t, parts[0].data))
		require.Equal(t, small, parts[1].data)
		require.Equal(t, smallGz, parts[2].data)
		require.Empty(t, parts[3].data)
	})

	t.Run("non-multipart", func(t *testing.T) {
		for _, body := range [][]byte{small, smallGz} {
			out, outCT, err := recompress(t, body, ct, gzip.BestCompression)
			require.NoError(t, err)
			require.Equal(t, ct, outCT)
			require.Equal(t, body, out, "body must pass through unchanged")
		}
		out, outCT, err := recompress(t, nil, ct, gzip.BestCompression)
		require.NoError(t, err)
		require.Equal(t, ct, outCT)
		require.Empty(t, out)
	})

	t.Run("threshold", func(t *testing.T) {
		atLimit := bytes.Repeat([]byte("a"), minRecompressSize)
		out, _, err := recompress(t, atLimit, ct, gzip.BestCompression)
		require.NoError(t, err)
		require.Equal(t, atLimit, gunzip(t, out))

		below := atLimit[:minRecompressSize-1]
		out, _, err = recompress(t, below, ct, gzip.BestCompression)
		require.NoError(t, err)
		require.Equal(t, below, out)
	})
}

func TestRecompressIngestBody_NonMultipartCorruptGzip(t *testing.T) {
	_, _, err := recompress(t, truncatedGzip(t), []string{"application/octet-stream"}, gzip.BestCompression)
	require.Error(t, err)
}

func TestArguments_ValidateRecompressLevel(t *testing.T) {
	for _, tc := range []struct {
		level   int
		wantErr bool
	}{{-1, true}, {0, false}, {1, false}, {9, false}, {10, true}} {
		var args Arguments
		args.SetToDefault()
		args.RecompressGzipLevel = tc.level
		err := args.Validate()
		if tc.wantErr {
			require.ErrorContains(t, err, "recompress_gzip_level", "level %d", tc.level)
			continue
		}
		require.NoError(t, err, "level %d", tc.level)
	}
}

func TestArguments_UnmarshalRecompressLevel(t *testing.T) {
	var args Arguments
	require.NoError(t, syntax.Unmarshal([]byte(`
		recompress_gzip_level = 6
		forward_to = []
	`), &args))
	require.Equal(t, 6, args.RecompressGzipLevel)

	require.ErrorContains(t, syntax.Unmarshal([]byte(`
		recompress_gzip_level = 10
		forward_to = []
	`), &args), "recompress_gzip_level")
}
