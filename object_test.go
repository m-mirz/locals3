package locals3_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mmirz/locals3/internal/testutil"
)

func TestObjectRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		body []byte
	}{
		{"simple", "hello.txt", []byte("hello world")},
		{"empty", "empty.bin", []byte{}},
		{"nested", "a/b/c/d/deep.txt", []byte("deep")},
		{"unicode", "ünïcode/日本語 key.txt", []byte("unicode body")},
		{"spaces and plus", "with space+plus.txt", []byte("spaces")},
		{"binary", "blob.bin", bytes.Repeat([]byte{0x00, 0xff, 0x7f}, 1000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, client := testutil.NewBucket(t, "roundtrip")
			testutil.Put(t, client, "roundtrip", tc.key, tc.body)

			got, out := testutil.Get(t, client, "roundtrip", tc.key)
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("body mismatch: got %d bytes, want %d", len(got), len(tc.body))
			}
			if aws.ToInt64(out.ContentLength) != int64(len(tc.body)) {
				t.Errorf("ContentLength = %d, want %d", aws.ToInt64(out.ContentLength), len(tc.body))
			}
		})
	}
}

func TestObjectETagIsMD5(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "etags")
	// Known MD5 of "hello world".
	const want = `"5eb63bbbe01eeed093cb22bb8f5acdc3"`
	if got := testutil.Put(t, client, "etags", "k", []byte("hello world")); got != want {
		t.Errorf("PutObject ETag = %s, want %s", got, want)
	}
	_, out := testutil.Get(t, client, "etags", "k")
	if aws.ToString(out.ETag) != want {
		t.Errorf("GetObject ETag = %s, want %s", aws.ToString(out.ETag), want)
	}
}

func TestObjectMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "meta")
	ctx := context.Background()

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String("meta"),
		Key:         aws.String("doc.json"),
		Body:        bytes.NewReader([]byte(`{"a":1}`)),
		ContentType: aws.String("application/json"),
		Metadata:    map[string]string{"author": "markus", "purpose": "testing"},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("meta"), Key: aws.String("doc.json"),
	})
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if got := aws.ToString(head.ContentType); got != "application/json" {
		t.Errorf("ContentType = %q, want application/json", got)
	}
	if got := head.Metadata["author"]; got != "markus" {
		t.Errorf("Metadata[author] = %q, want markus", got)
	}
	if got := head.Metadata["purpose"]; got != "testing" {
		t.Errorf("Metadata[purpose] = %q, want testing", got)
	}
	if aws.ToInt64(head.ContentLength) != 7 {
		t.Errorf("ContentLength = %d, want 7", aws.ToInt64(head.ContentLength))
	}
}

func TestObjectContentTypeSniffedForForeignFile(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "sniff")
	// A file written straight into the storage directory has no sidecar, so
	// the server must infer its type. (The SDK always sends
	// application/octet-stream when the caller sets no type, and real S3
	// records that verbatim, so uploads are not the interesting case here.)
	if err := os.WriteFile(filepath.Join(srv.Dir, "sniff", "page.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatalf("drop file: %v", err)
	}
	body, out := testutil.Get(t, client, "sniff", "page.html")
	if string(body) != "<html></html>" {
		t.Errorf("body = %q, want <html></html>", body)
	}
	if ct := aws.ToString(out.ContentType); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("ContentType = %q, want text/html...", ct)
	}
	// The server computes the digest for a file it never saw written.
	if got, want := aws.ToString(out.ETag), `"c83301425b2ad1d496473a5ff3d9ecca"`; got != want {
		t.Errorf("ETag = %s, want %s (MD5 of the dropped file)", got, want)
	}
}

func TestObjectDelete(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "deletes")
	ctx := context.Background()
	testutil.Put(t, client, "deletes", "gone.txt", []byte("bye"))

	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("deletes"), Key: aws.String("gone.txt"),
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("deletes"), Key: aws.String("gone.txt"),
	})
	testutil.AssertAPIError(t, err, "NoSuchKey")

	// S3 delete is idempotent.
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("deletes"), Key: aws.String("gone.txt"),
	}); err != nil {
		t.Fatalf("second delete should succeed, got: %v", err)
	}
}

func TestObjectErrorsAreTyped(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "errors")
	ctx := context.Background()

	// NoSuchKey is in GetObject's modeled error set, so the SDK deserialises it
	// into a typed error and callers can errors.As on it.
	_, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("errors"), Key: aws.String("absent"),
	})
	var noSuchKey *types.NoSuchKey
	if !errors.As(err, &noSuchKey) {
		t.Fatalf("expected *types.NoSuchKey, got %T: %v", err, err)
	}

	// NoSuchBucket is not modeled on GetObject, so the SDK surfaces it as a
	// generic APIError carrying the code -- exactly as it does against real S3.
	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("no-such-bucket-here"), Key: aws.String("k"),
	})
	testutil.AssertAPIError(t, err, "NoSuchBucket")

	// On an operation that does model it, the typed error comes through.
	_, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("nope")})
	var noSuchBucket *types.NoSuchBucket
	if !errors.As(err, &noSuchBucket) {
		t.Logf("ListObjectsV2 not implemented yet; NoSuchBucket typing deferred to M3 (%v)", err)
	}
}

func TestObjectStoredAsPlainFile(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "mirror")
	testutil.Put(t, client, "mirror", "docs/readme.md", []byte("# hi"))

	path := filepath.Join(srv.Dir, "mirror", "docs", "readme.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("object should exist at %s: %v", path, err)
	}
	if string(got) != "# hi" {
		t.Errorf("on-disk content = %q, want %q", got, "# hi")
	}
}

func TestObjectKeyDirectoryConflict(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "conflict")
	ctx := context.Background()
	testutil.Put(t, client, "conflict", "a", []byte("file"))

	// "a" is a file on disk, so "a/b" cannot be represented. locals3 reports
	// this rather than corrupting the tree; real S3 would accept it.
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("conflict"), Key: aws.String("a/b"),
		Body: bytes.NewReader([]byte("nested")),
	})
	testutil.AssertAPIError(t, err, "InvalidRequest")
}
