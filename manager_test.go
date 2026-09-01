package locals3_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mmirz/locals3/internal/testutil"
)

// The transfer manager is the highest-value client to satisfy: it drives
// multipart upload and concurrent ranged download the way real application code
// does, without any of it being spelled out in the test. If these pass, the
// server is doing multipart, ETags, ranges and concurrency correctly at once.
//
// Note: feature/s3/manager is formally deprecated in favour of
// feature/s3/transfermanager, but that package is still v0.x with an unstable
// API. This targets what applications actually use today.

func TestManagerUploadAndDownload(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "transfer")
	ctx := context.Background()

	// 32 MiB at the default 5 MiB part size forces a real multipart upload
	// across seven parts, uploaded concurrently.
	body := randomBytes(32 << 20)

	uploader := manager.NewUploader(client)
	out, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String("transfer"),
		Key:    aws.String("large/payload.bin"),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	// A multipart ETag carries a "-<partCount>" suffix; a single PUT does not.
	if etag := aws.ToString(out.ETag); !strings.Contains(etag, "-") {
		t.Errorf("ETag = %s, want a multipart ETag (the upload did not split)", etag)
	}

	downloader := manager.NewDownloader(client)
	buf := manager.NewWriteAtBuffer(make([]byte, 0, len(body)))
	n, err := downloader.Download(ctx, buf, &s3.GetObjectInput{
		Bucket: aws.String("transfer"),
		Key:    aws.String("large/payload.bin"),
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("downloaded %d bytes, want %d", n, len(body))
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Fatal("downloaded bytes differ from what was uploaded")
	}
}

func TestManagerUploadStreamingBody(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "streamed")
	ctx := context.Background()

	body := randomBytes(12 << 20)
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = 5 << 20
		u.Concurrency = 3
	})
	// A reader the SDK cannot seek or size up front, which is what a pipe or a
	// network stream looks like.
	if _, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String("streamed"),
		Key:    aws.String("stream.bin"),
		Body:   struct{ readerOnly }{readerOnly{bytes.NewReader(body)}},
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	got, _ := testutil.Get(t, client, "streamed", "stream.bin")
	if !bytes.Equal(got, body) {
		t.Fatalf("round-tripped %d bytes, want %d", len(got), len(body))
	}
}

func TestManagerDownloadConcurrentRanges(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "ranged")
	ctx := context.Background()

	// Uploaded as a single object, then pulled back in many concurrent ranged
	// GETs: this is what exercises the Range path under load.
	body := randomBytes(9 << 20)
	testutil.Put(t, client, "ranged", "single.bin", body)

	downloader := manager.NewDownloader(client, func(d *manager.Downloader) {
		d.PartSize = 1 << 20
		d.Concurrency = 8
	})
	buf := manager.NewWriteAtBuffer(make([]byte, 0, len(body)))
	if _, err := downloader.Download(ctx, buf, &s3.GetObjectInput{
		Bucket: aws.String("ranged"), Key: aws.String("single.bin"),
	}); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Fatal("concurrently downloaded bytes differ from the original")
	}
}

func TestManagerUploadSmallBodyStaysSinglePart(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "smalltransfer")
	body := randomBytes(1024)
	out, err := manager.NewUploader(client).Upload(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("smalltransfer"), Key: aws.String("small.bin"),
		Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got, want := aws.ToString(out.ETag), fmt.Sprintf("%q", md5hex(body)); got != want {
		t.Errorf("ETag = %s, want %s (a small body should not become multipart)", got, want)
	}
}

// readerOnly hides the ReaderAt/Seeker methods of its wrapped reader.
type readerOnly struct{ r *bytes.Reader }

func (r readerOnly) Read(p []byte) (int, error) { return r.r.Read(p) }
