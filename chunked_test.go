package locals3_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mmirz/locals3"
	"github.com/mmirz/locals3/internal/testutil"
)

// A request whose payload is unsigned, whose body is not seekable and which
// carries a checksum algorithm arrives with its bytes wrapped in aws-chunked
// framing plus a trailer. If the server stored the framing instead of the
// payload, the object would be corrupt in a way no small round-trip reveals.
//
// The trigger is pinned down in internal/s3api/chunked.go; the test reproduces
// it through the real SDK rather than by hand-crafting the framing, so it stays
// honest if the SDK moves.
func TestChunkedBodyIsDecoded(t *testing.T) {
	t.Parallel()
	sizes := []int{0, 1, 8192, (1 << 20) + 7, 5 << 20}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dB", size), func(t *testing.T) {
			t.Parallel()
			dir, client, sawChunked := newChunkingClient(t)
			ctx := context.Background()
			if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
				Bucket: aws.String("chunked"),
			}); err != nil {
				t.Fatalf("create bucket: %v", err)
			}

			body := randomBytes(size)
			key := fmt.Sprintf("obj-%d.bin", size)
			out, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("chunked"),
				Key:    aws.String(key),
				// A reader the SDK cannot seek, so it cannot precompute the
				// checksum and must send it in a trailer.
				Body:              struct{ io.Reader }{bytes.NewReader(body)},
				ChecksumAlgorithm: "CRC32",
			})
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			if !*sawChunked {
				t.Fatalf("request was not aws-chunked; the decoder went unexercised")
			}

			if want := fmt.Sprintf("%q", md5hex(body)); aws.ToString(out.ETag) != want {
				t.Errorf("ETag = %s, want %s", aws.ToString(out.ETag), want)
			}

			got, getOut := testutil.Get(t, client, "chunked", key)
			if !bytes.Equal(got, body) {
				t.Fatalf("body mismatch: got %d bytes, want %d", len(got), len(body))
			}
			if n := aws.ToInt64(getOut.ContentLength); n != int64(size) {
				t.Errorf("ContentLength = %d, want %d", n, size)
			}

			onDisk, err := os.ReadFile(filepath.Join(dir, "chunked", key))
			if err != nil {
				t.Fatalf("read from disk: %v", err)
			}
			if !bytes.Equal(onDisk, body) {
				t.Errorf("on-disk size = %d, want %d (chunk framing leaked into the file?)",
					len(onDisk), len(body))
			}
		})
	}
}

// TestPlainBodyRoundTrip covers the ordinary path: a plain-HTTP endpoint with a
// seekable body, where the SDK signs the payload and sends raw bytes.
func TestPlainBodyRoundTrip(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "plain")
	for _, size := range []int{0, 1, 8192, (1 << 20) + 7} {
		body := randomBytes(size)
		key := fmt.Sprintf("obj-%d.bin", size)
		if got, want := testutil.Put(t, client, "plain", key, body), fmt.Sprintf("%q", md5hex(body)); got != want {
			t.Errorf("size %d: ETag = %s, want %s", size, got, want)
		}
		got, _ := testutil.Get(t, client, "plain", key)
		if !bytes.Equal(got, body) {
			t.Errorf("size %d: body mismatch", size)
		}
		onDisk, err := os.ReadFile(filepath.Join(srv.Dir, "plain", key))
		if err != nil || !bytes.Equal(onDisk, body) {
			t.Errorf("size %d: on-disk mismatch (err=%v)", size, err)
		}
	}
}

// newChunkingClient starts a TLS-backed server, which makes the SDK use an
// unsigned payload, and reports whether any PUT arrived aws-chunked.
func newChunkingClient(t *testing.T) (dir string, client *s3.Client, sawChunked *bool) {
	t.Helper()
	dir = t.TempDir()
	srv, err := locals3.New(locals3.Options{Dir: dir})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	saw := false
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut &&
			(strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "aws-chunked") ||
				strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-")) {
			saw = true
		}
		srv.ServeHTTP(w, r)
	})
	hs := httptest.NewTLSServer(spy)
	t.Cleanup(hs.Close)
	client = testutil.Client(hs.URL, func(o *s3.Options) { o.HTTPClient = hs.Client() })
	return dir, client, &saw
}

// randomBytes returns non-repeating data, so a truncated or misassembled body
// cannot coincidentally compare equal.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	r := rand.NewChaCha8([32]byte{7})
	_, _ = r.Read(b)
	return b
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}
