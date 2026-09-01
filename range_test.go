package locals3_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mmirz/locals3/internal/testutil"
)

const rangeBody = "0123456789abcdefghij" // 20 bytes, position-identifiable

func TestGetObjectRange(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "ranges")
	testutil.Put(t, client, "ranges", "obj.txt", []byte(rangeBody))
	ctx := context.Background()

	tests := []struct {
		name         string
		rangeHeader  string
		want         string
		contentRange string
	}{
		{"leading bytes", "bytes=0-9", "0123456789", "bytes 0-9/20"},
		{"middle", "bytes=5-9", "56789", "bytes 5-9/20"},
		{"single byte", "bytes=3-3", "3", "bytes 3-3/20"},
		{"open ended", "bytes=15-", "fghij", "bytes 15-19/20"},
		{"suffix", "bytes=-4", "ghij", "bytes 16-19/20"},
		{"suffix longer than object", "bytes=-100", rangeBody, "bytes 0-19/20"},
		{"end past object", "bytes=18-99", "ij", "bytes 18-19/20"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String("ranges"), Key: aws.String("obj.txt"),
				Range: aws.String(tc.rangeHeader),
			})
			if err != nil {
				t.Fatalf("get %s: %v", tc.rangeHeader, err)
			}
			defer out.Body.Close()
			got, _ := io.ReadAll(out.Body)
			if string(got) != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if cr := aws.ToString(out.ContentRange); cr != tc.contentRange {
				t.Errorf("Content-Range = %q, want %q", cr, tc.contentRange)
			}
			if n := aws.ToInt64(out.ContentLength); n != int64(len(tc.want)) {
				t.Errorf("ContentLength = %d, want %d", n, len(tc.want))
			}
		})
	}
}

func TestGetObjectRangeUnsatisfiable(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "badrange")
	testutil.Put(t, client, "badrange", "obj.txt", []byte(rangeBody))

	_, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("badrange"), Key: aws.String("obj.txt"),
		Range: aws.String("bytes=100-200"),
	})
	testutil.AssertAPIError(t, err, "InvalidRange")

	// The 416 must carry Content-Range so the client learns the real size.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/badrange/obj.txt", nil)
	req.Header.Set("Range", "bytes=100-200")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes */20" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes */20")
	}
}

func TestConditionalHeaders(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "conditional")
	ctx := context.Background()
	etag := testutil.Put(t, client, "conditional", "obj.txt", []byte(rangeBody))
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("obj.txt"),
	})
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	modified := aws.ToTime(head.LastModified)

	t.Run("If-Match hit", func(t *testing.T) {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("conditional"), Key: aws.String("obj.txt"),
			IfMatch: aws.String(etag),
		})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		out.Body.Close()
	})

	t.Run("If-Match miss", func(t *testing.T) {
		_, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("conditional"), Key: aws.String("obj.txt"),
			IfMatch: aws.String(`"00000000000000000000000000000000"`),
		})
		testutil.AssertAPIError(t, err, "PreconditionFailed")
	})

	t.Run("If-None-Match hit yields 304", func(t *testing.T) {
		_, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("conditional"), Key: aws.String("obj.txt"),
			IfNoneMatch: aws.String(etag),
		})
		// The SDK surfaces a 304 as an error carrying the status.
		if err == nil {
			t.Fatal("expected a 304 to be reported as an error")
		}
		if !strings.Contains(err.Error(), "304") {
			t.Errorf("expected a 304, got %v", err)
		}
	})

	t.Run("If-Modified-Since not modified", func(t *testing.T) {
		_, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("conditional"), Key: aws.String("obj.txt"),
			IfModifiedSince: aws.Time(modified),
		})
		if err == nil || !strings.Contains(err.Error(), "304") {
			t.Errorf("an object modified at exactly the header time counts as unmodified; got %v", err)
		}
	})

	t.Run("If-Modified-Since modified", func(t *testing.T) {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("conditional"), Key: aws.String("obj.txt"),
			IfModifiedSince: aws.Time(modified.Add(-time.Hour)),
		})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		out.Body.Close()
	})

	t.Run("If-Unmodified-Since violated", func(t *testing.T) {
		_, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("conditional"), Key: aws.String("obj.txt"),
			IfUnmodifiedSince: aws.Time(modified.Add(-time.Hour)),
		})
		testutil.AssertAPIError(t, err, "PreconditionFailed")
	})
}

func TestPresignedGetAndPut(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "presign")
	ctx := context.Background()
	presigner := s3.NewPresignClient(client)

	// PUT through a presigned URL, with no SDK involved in the request itself.
	putReq, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("presign"), Key: aws.String("uploaded.txt"),
	}, s3.WithPresignExpires(10*time.Minute))
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	req, _ := http.NewRequest(putReq.Method, putReq.URL, bytes.NewReader([]byte("via presigned url")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("presigned put: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned put status = %d, want 200", resp.StatusCode)
	}

	getReq, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("presign"), Key: aws.String("uploaded.txt"),
	}, s3.WithPresignExpires(10*time.Minute))
	if err != nil {
		t.Fatalf("presign get: %v", err)
	}
	resp, err = http.Get(getReq.URL)
	if err != nil {
		t.Fatalf("presigned get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via presigned url" {
		t.Errorf("body = %q, want %q", body, "via presigned url")
	}
}

func TestPresignedURLExpires(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "expiry")
	testutil.Put(t, client, "expiry", "obj.txt", []byte("secret"))

	presigner := s3.NewPresignClient(client)
	req, err := presigner.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("expiry"), Key: aws.String("obj.txt"),
	}, s3.WithPresignExpires(time.Second))
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	// Rewind the signing time rather than sleeping: the URL carries its own
	// X-Amz-Date, so an older one is indistinguishable from having waited.
	stale := strings.Replace(req.URL,
		"X-Amz-Expires=1", "X-Amz-Expires=1&X-Amz-Ignored=1", 1)
	stale = replaceDate(t, stale)

	resp, err := http.Get(stale)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an expired URL", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "AccessDenied") {
		t.Errorf("body = %s, want an AccessDenied error", body)
	}
}

// replaceDate rewrites X-Amz-Date in a presigned URL to an hour ago.
func replaceDate(t *testing.T, url string) string {
	t.Helper()
	i := strings.Index(url, "X-Amz-Date=")
	if i < 0 {
		t.Fatal("presigned URL carried no X-Amz-Date")
	}
	end := strings.Index(url[i:], "&")
	if end < 0 {
		end = len(url) - i
	}
	old := url[i : i+end]
	stamp := time.Now().Add(-time.Hour).UTC().Format("20060102T150405Z")
	return strings.Replace(url, old, fmt.Sprintf("X-Amz-Date=%s", stamp), 1)
}
