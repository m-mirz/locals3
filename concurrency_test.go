package locals3_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mmirz/locals3/internal/testutil"
)

// Run these under -race. Writes are staged and renamed into place, so a reader
// must never observe a partially written object, and concurrent writers to one
// key must leave a whole version of one of them rather than a blend.

func TestConcurrentDistinctKeys(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "parallelkeys")
	ctx := context.Background()

	const workers = 16
	bodies := make([][]byte, workers)
	for i := range bodies {
		bodies[i] = randomBytes(64 << 10)
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("worker/%02d.bin", i)
			if _, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("parallelkeys"), Key: aws.String(key),
				Body: bytes.NewReader(bodies[i]),
			}); err != nil {
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}

	for i := range workers {
		got, _ := testutil.Get(t, client, "parallelkeys", fmt.Sprintf("worker/%02d.bin", i))
		if !bytes.Equal(got, bodies[i]) {
			t.Errorf("worker %d: body mismatch", i)
		}
	}
}

func TestConcurrentSameKey(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "samekey")
	ctx := context.Background()

	// Every writer stores a distinct, self-consistent body. Whichever wins, a
	// reader must see one of them entire -- never a mixture.
	const writers = 8
	const size = 128 << 10
	bodies := make([][]byte, writers)
	for i := range bodies {
		bodies[i] = bytes.Repeat([]byte{byte('A' + i)}, size)
	}

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("samekey"), Key: aws.String("contended.bin"),
				Body: bytes.NewReader(bodies[i]),
			})
		}()
	}
	// Read while the writes are in flight.
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String("samekey"), Key: aws.String("contended.bin"),
			})
			if err != nil {
				return // the object may not exist yet
			}
			defer out.Body.Close()
			var buf bytes.Buffer
			if _, err := buf.ReadFrom(out.Body); err != nil {
				return
			}
			b := buf.Bytes()
			if len(b) == 0 {
				return
			}
			if len(b) != size {
				t.Errorf("torn read: got %d bytes, want %d", len(b), size)
				return
			}
			if bytes.Count(b, b[:1]) != size {
				t.Error("torn read: the object mixed content from several writers")
			}
		}()
	}
	wg.Wait()

	got, _ := testutil.Get(t, client, "samekey", "contended.bin")
	if len(got) != size || bytes.Count(got, got[:1]) != size {
		t.Errorf("final object is not a whole version of any writer's body (%d bytes)", len(got))
	}
}

func TestConcurrentMixedOperations(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "mixedops")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 12 {
		key := fmt.Sprintf("obj-%02d.bin", i)
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("mixedops"), Key: aws.String(key),
				Body: bytes.NewReader(randomBytes(4096)),
			})
		}()
		go func() {
			defer wg.Done()
			if out, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String("mixedops"), Key: aws.String(key),
			}); err == nil {
				out.Body.Close()
			}
		}()
		go func() {
			defer wg.Done()
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String("mixedops"), Key: aws.String(key),
			})
		}()
	}
	// Listing while the tree is churning must not fail or panic.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 10 {
			if _, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
				Bucket: aws.String("mixedops"),
			}); err != nil {
				t.Errorf("list during churn: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

func TestConcurrentMultipartParts(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "parallelparts")
	ctx := context.Background()

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("parallelparts"), Key: aws.String("obj.bin"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uploadID := aws.ToString(create.UploadId)

	const parts = 4
	bodies := make([][]byte, parts)
	etags := make([]string, parts)
	for i := range bodies {
		bodies[i] = randomBytes(minPart)
	}

	var wg sync.WaitGroup
	for i := range parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket: aws.String("parallelparts"), Key: aws.String("obj.bin"),
				UploadId: aws.String(uploadID), PartNumber: aws.Int32(int32(i + 1)),
				Body: bytes.NewReader(bodies[i]),
			})
			if err != nil {
				t.Errorf("upload part %d: %v", i+1, err)
				return
			}
			etags[i] = aws.ToString(out.ETag)
		}()
	}
	wg.Wait()

	var completed []completedPart
	for i, etag := range etags {
		completed = append(completed, completedPart{num: int32(i + 1), etag: etag})
	}
	if err := completeUpload(ctx, client, "parallelparts", "obj.bin", uploadID, completed); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ := testutil.Get(t, client, "parallelparts", "obj.bin")
	if !bytes.Equal(got, bytes.Join(bodies, nil)) {
		t.Error("concurrently uploaded parts were assembled incorrectly")
	}
}
