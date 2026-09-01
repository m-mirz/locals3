package locals3_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mmirz/locals3/internal/testutil"
)

// minPart is the smallest a non-final part may be, matching S3.
const minPart = 5 << 20

// multipartETag computes the digest S3 reports for an object assembled from
// these parts: the MD5 of the concatenated raw part digests, then "-<count>".
func multipartETag(parts [][]byte) string {
	h := md5.New()
	for _, p := range parts {
		sum := md5.Sum(p)
		h.Write(sum[:])
	}
	return fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(h.Sum(nil)), len(parts))
}

func TestMultipartLifecycle(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "mpu")
	ctx := context.Background()

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("mpu"), Key: aws.String("big/object.bin"),
		ContentType: aws.String("application/octet-stream"),
		Metadata:    map[string]string{"origin": "multipart"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uploadID := aws.ToString(create.UploadId)
	if uploadID == "" {
		t.Fatal("create returned no upload id")
	}

	parts := [][]byte{randomBytes(minPart), randomBytes(minPart), randomBytes(1234)}
	var completed []types.CompletedPart
	for i, body := range parts {
		num := int32(i + 1)
		out, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("mpu"), Key: aws.String("big/object.bin"),
			UploadId: aws.String(uploadID), PartNumber: aws.Int32(num),
			Body: bytes.NewReader(body),
		})
		if err != nil {
			t.Fatalf("upload part %d: %v", num, err)
		}
		want := fmt.Sprintf("%q", md5hex(body))
		if aws.ToString(out.ETag) != want {
			t.Errorf("part %d ETag = %s, want %s", num, aws.ToString(out.ETag), want)
		}
		completed = append(completed, types.CompletedPart{
			PartNumber: aws.Int32(num), ETag: out.ETag,
		})
	}

	// Parts must be visible before completion.
	listed, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String("mpu"), Key: aws.String("big/object.bin"),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		t.Fatalf("list parts: %v", err)
	}
	if len(listed.Parts) != 3 {
		t.Fatalf("ListParts returned %d parts, want 3", len(listed.Parts))
	}
	for i, p := range listed.Parts {
		if aws.ToInt32(p.PartNumber) != int32(i+1) {
			t.Errorf("parts are not in ascending order: %d at index %d", aws.ToInt32(p.PartNumber), i)
		}
		if aws.ToInt64(p.Size) != int64(len(parts[i])) {
			t.Errorf("part %d Size = %d, want %d", i+1, aws.ToInt64(p.Size), len(parts[i]))
		}
	}

	done, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("mpu"), Key: aws.String("big/object.bin"),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got, want := aws.ToString(done.ETag), multipartETag(parts); got != want {
		t.Errorf("ETag = %s, want %s (MD5 of concatenated part digests + -N)", got, want)
	}

	want := bytes.Join(parts, nil)
	got, out := testutil.Get(t, client, "mpu", "big/object.bin")
	if !bytes.Equal(got, want) {
		t.Fatalf("assembled body is %d bytes, want %d", len(got), len(want))
	}
	if v := out.Metadata["origin"]; v != "multipart" {
		t.Errorf("Metadata[origin] = %q, want multipart (set at create time)", v)
	}

	// The assembled object is an ordinary file, and the staging area is clean.
	onDisk, err := os.ReadFile(filepath.Join(srv.Dir, "mpu", "big", "object.bin"))
	if err != nil || !bytes.Equal(onDisk, want) {
		t.Errorf("on-disk object wrong (err=%v)", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(srv.Dir, ".locals3", "uploads")); len(entries) != 0 {
		t.Errorf("completed upload left %d staging directories behind", len(entries))
	}
}

func TestMultipartOutOfOrderUpload(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "ooo")
	ctx := context.Background()
	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("ooo"), Key: aws.String("obj.bin"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uploadID := aws.ToString(create.UploadId)

	parts := [][]byte{randomBytes(minPart), randomBytes(64)}
	etags := make([]*string, 2)
	// Upload part 2 before part 1: legal, and what a concurrent uploader does.
	for _, i := range []int{1, 0} {
		out, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("ooo"), Key: aws.String("obj.bin"),
			UploadId: aws.String(uploadID), PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(parts[i]),
		})
		if err != nil {
			t.Fatalf("upload part %d: %v", i+1, err)
		}
		etags[i] = out.ETag
	}

	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("ooo"), Key: aws.String("obj.bin"),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: etags[0]},
			{PartNumber: aws.Int32(2), ETag: etags[1]},
		}},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ := testutil.Get(t, client, "ooo", "obj.bin")
	if !bytes.Equal(got, bytes.Join(parts, nil)) {
		t.Error("parts were assembled in the wrong order")
	}
}

func TestMultipartAbort(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "aborted")
	ctx := context.Background()
	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("aborted"), Key: aws.String("obj.bin"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uploadID := aws.ToString(create.UploadId)
	if _, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("aborted"), Key: aws.String("obj.bin"),
		UploadId: aws.String(uploadID), PartNumber: aws.Int32(1),
		Body: bytes.NewReader(randomBytes(1024)),
	}); err != nil {
		t.Fatalf("upload part: %v", err)
	}

	if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String("aborted"), Key: aws.String("obj.bin"),
		UploadId: aws.String(uploadID),
	}); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(srv.Dir, ".locals3", "uploads")); len(entries) != 0 {
		t.Errorf("abort left %d staging directories behind", len(entries))
	}
	_, err = client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String("aborted"), Key: aws.String("obj.bin"),
		UploadId: aws.String(uploadID),
	})
	testutil.AssertAPIError(t, err, "NoSuchUpload")

	// Nothing was written to the object namespace.
	_, err = client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("aborted"), Key: aws.String("obj.bin"),
	})
	if err == nil {
		t.Error("an aborted upload must not produce an object")
	}
}

func TestMultipartRejections(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "reject")
	ctx := context.Background()

	newUpload := func(t *testing.T, key string) string {
		t.Helper()
		out, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String("reject"), Key: aws.String(key),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return aws.ToString(out.UploadId)
	}

	t.Run("unknown upload id", func(t *testing.T) {
		_, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("reject"), Key: aws.String("x.bin"),
			UploadId: aws.String("ffffffffffffffffffffffffffffffff"), PartNumber: aws.Int32(1),
			Body: bytes.NewReader([]byte("x")),
		})
		testutil.AssertAPIError(t, err, "NoSuchUpload")
	})

	t.Run("wrong etag", func(t *testing.T) {
		id := newUpload(t, "etag.bin")
		if _, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("reject"), Key: aws.String("etag.bin"),
			UploadId: aws.String(id), PartNumber: aws.Int32(1),
			Body: bytes.NewReader([]byte("real content")),
		}); err != nil {
			t.Fatalf("upload part: %v", err)
		}
		_, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String("reject"), Key: aws.String("etag.bin"),
			UploadId: aws.String(id),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: aws.String(`"00000000000000000000000000000000"`)},
			}},
		})
		testutil.AssertAPIError(t, err, "InvalidPart")
	})

	t.Run("descending part order", func(t *testing.T) {
		id := newUpload(t, "order.bin")
		var etags []*string
		for i := range 2 {
			out, err := client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket: aws.String("reject"), Key: aws.String("order.bin"),
				UploadId: aws.String(id), PartNumber: aws.Int32(int32(i + 1)),
				Body: bytes.NewReader(randomBytes(minPart)),
			})
			if err != nil {
				t.Fatalf("upload part: %v", err)
			}
			etags = append(etags, out.ETag)
		}
		_, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String("reject"), Key: aws.String("order.bin"),
			UploadId: aws.String(id),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(2), ETag: etags[1]},
				{PartNumber: aws.Int32(1), ETag: etags[0]},
			}},
		})
		testutil.AssertAPIError(t, err, "InvalidPartOrder")
	})

	t.Run("undersized non-final part", func(t *testing.T) {
		id := newUpload(t, "small.bin")
		var etags []*string
		for i := range 2 {
			out, err := client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket: aws.String("reject"), Key: aws.String("small.bin"),
				UploadId: aws.String(id), PartNumber: aws.Int32(int32(i + 1)),
				Body: bytes.NewReader(randomBytes(1024)), // well under 5 MiB
			})
			if err != nil {
				t.Fatalf("upload part: %v", err)
			}
			etags = append(etags, out.ETag)
		}
		_, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String("reject"), Key: aws.String("small.bin"),
			UploadId: aws.String(id),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: etags[0]},
				{PartNumber: aws.Int32(2), ETag: etags[1]},
			}},
		})
		// Real S3 enforces the 5 MiB floor; locals3 does too, so a client that
		// passes here cannot fail against Ceph or AWS for this reason.
		testutil.AssertAPIError(t, err, "EntityTooSmall")
	})
}

func TestListMultipartUploads(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "inflight")
	ctx := context.Background()
	for _, key := range []string{"b.bin", "a.bin"} {
		if _, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String("inflight"), Key: aws.String(key),
		}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}
	out, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String("inflight"),
	})
	if err != nil {
		t.Fatalf("list uploads: %v", err)
	}
	var keys []string
	for _, u := range out.Uploads {
		keys = append(keys, aws.ToString(u.Key))
	}
	assertKeys(t, keys, []string{"a.bin", "b.bin"})
}

// completedPart and completeUpload keep the multipart completion boilerplate in
// one place for suites that only care about the outcome.
type completedPart struct {
	num  int32
	etag string
}

func completeUpload(ctx context.Context, c *s3.Client, bucket, key, uploadID string, parts []completedPart) error {
	in := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		in = append(in, types.CompletedPart{PartNumber: aws.Int32(p.num), ETag: aws.String(p.etag)})
	}
	_, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: in},
	})
	return err
}
