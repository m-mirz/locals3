package s3api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Some requests arrive with the payload wrapped in aws-chunked framing rather
// than as raw bytes:
//
//	Content-Encoding: aws-chunked
//	x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER
//	x-amz-trailer: x-amz-checksum-crc32
//	[x-amz-decoded-content-length: <real length>]
//
//	<hex-size>[;chunk-signature=...]\r\n<data>\r\n ... 0\r\n<trailer>\r\n\r\n
//
// Handing r.Body straight to the store would persist that framing as object
// bytes: silent corruption that only surfaces as a checksum mismatch on the way
// back out, long after the write.
//
// AWS SDK for Go v2 (s3 v1.109.1) emits it when it must compute a checksum it
// cannot know up front -- that is, when the payload is unsigned AND the body is
// not seekable AND a checksum algorithm is in play. In practice that means a
// TLS endpoint with a streaming body, or a client configured for unsigned
// payloads. Over a plain-HTTP endpoint the SDK signs the payload instead, which
// forces it to seek the body and precompute the digest, so no framing appears.
// Other S3 clients pick different points in that space, so the decoder is
// driven by the request headers rather than by any assumption about the client.

// chunkedReader decodes aws-chunked framing from an underlying stream and
// exposes any trailing headers once the stream is fully consumed.
//
// Not safe for concurrent use.
type chunkedReader struct {
	br       *bufio.Reader
	remain   int64 // bytes left in the current chunk
	started  bool  // a chunk has been consumed, so a CRLF separator is pending
	done     bool
	trailers http.Header
	err      error
}

func newChunkedReader(r io.Reader) *chunkedReader {
	return &chunkedReader{br: bufio.NewReader(r), trailers: make(http.Header)}
}

// Trailers returns the trailing headers. It is only populated after Read has
// returned io.EOF.
func (c *chunkedReader) Trailers() http.Header { return c.trailers }

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	for c.remain == 0 {
		if c.done {
			return 0, io.EOF
		}
		if c.started {
			if err := c.expectCRLF(); err != nil {
				return 0, c.fail(err)
			}
		}
		n, err := c.readChunkHeader()
		if err != nil {
			return 0, c.fail(err)
		}
		c.started = true
		if n == 0 {
			// Final chunk: what follows is the trailer block.
			if err := c.readTrailers(); err != nil {
				return 0, c.fail(err)
			}
			c.done = true
			return 0, io.EOF
		}
		c.remain = n
	}
	if int64(len(p)) > c.remain {
		p = p[:c.remain]
	}
	n, err := c.br.Read(p)
	c.remain -= int64(n)
	if errors.Is(err, io.EOF) && c.remain > 0 {
		return n, c.fail(io.ErrUnexpectedEOF)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, c.fail(err)
	}
	return n, nil
}

func (c *chunkedReader) fail(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		c.err = errIncompleteBody
	} else {
		c.err = err
	}
	return c.err
}

// readChunkHeader parses "<hex-size>[;ext=value]" and returns the size.
func (c *chunkedReader) readChunkHeader() (int64, error) {
	line, err := c.readLine()
	if err != nil {
		return 0, err
	}
	if i := strings.IndexByte(line, ';'); i >= 0 {
		line = line[:i] // drop chunk-signature and any other extension
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, fmt.Errorf("aws-chunked: empty chunk size")
	}
	n, err := strconv.ParseInt(line, 16, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("aws-chunked: bad chunk size %q", line)
	}
	return n, nil
}

// readTrailers consumes "name:value" lines up to the terminating blank line.
func (c *chunkedReader) readTrailers() error {
	for {
		line, err := c.readLine()
		if errors.Is(err, io.EOF) {
			return nil // trailer block may be unterminated at stream end
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "" {
			return nil
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		c.trailers.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
}

func (c *chunkedReader) readLine() (string, error) {
	line, err := c.br.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *chunkedReader) expectCRLF() error {
	var buf [2]byte
	if _, err := io.ReadFull(c.br, buf[:]); err != nil {
		return err
	}
	if buf[0] != '\r' || buf[1] != '\n' {
		return fmt.Errorf("aws-chunked: missing CRLF after chunk")
	}
	return nil
}

// isChunkedBody reports whether a request carries aws-chunked framing.
func isChunkedBody(r *http.Request) bool {
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "aws-chunked") {
		return true
	}
	return strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-")
}

// requestBody returns the decoded object bytes for a request, together with the
// declared length (-1 when unknown) and the chunked reader when one was used.
func requestBody(r *http.Request) (io.Reader, int64, *chunkedReader) {
	if !isChunkedBody(r) {
		return r.Body, r.ContentLength, nil
	}
	size := int64(-1)
	if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			size = n
		}
	}
	cr := newChunkedReader(r.Body)
	return cr, size, cr
}
