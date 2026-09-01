package locals3_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mmirz/locals3/internal/testutil"
)

func TestCopyObjectWithinBucket(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "copysrc")
	ctx := context.Background()
	body := []byte("copy me")
	testutil.Put(t, client, "copysrc", "original.txt", body)

	out, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String("copysrc"),
		Key:        aws.String("duplicate.txt"),
		CopySource: aws.String("copysrc/original.txt"),
	})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if out.CopyObjectResult == nil || aws.ToString(out.CopyObjectResult.ETag) == "" {
		t.Fatal("copy result carried no ETag")
	}

	got, _ := testutil.Get(t, client, "copysrc", "duplicate.txt")
	if !bytes.Equal(got, body) {
		t.Errorf("copied body = %q, want %q", got, body)
	}
	// The source must survive.
	got, _ = testutil.Get(t, client, "copysrc", "original.txt")
	if !bytes.Equal(got, body) {
		t.Errorf("source body = %q, want %q", got, body)
	}
}

func TestCopyObjectAcrossBuckets(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "srcbucket")
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("dstbucket")}); err != nil {
		t.Fatalf("create dst: %v", err)
	}
	testutil.Put(t, client, "srcbucket", "a/b/c.txt", []byte("across"))

	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String("dstbucket"),
		Key:        aws.String("moved/c.txt"),
		CopySource: aws.String("srcbucket/a/b/c.txt"),
	}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	got, _ := testutil.Get(t, client, "dstbucket", "moved/c.txt")
	if string(got) != "across" {
		t.Errorf("body = %q, want across", got)
	}
}

func TestCopyObjectCarriesMetadata(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "copymeta")
	ctx := context.Background()
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("copymeta"), Key: aws.String("src.json"),
		Body:        bytes.NewReader([]byte(`{}`)),
		ContentType: aws.String("application/json"),
		Metadata:    map[string]string{"origin": "first"},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("copymeta"), Key: aws.String("dst.json"),
		CopySource: aws.String("copymeta/src.json"),
	}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("copymeta"), Key: aws.String("dst.json"),
	})
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if got := aws.ToString(head.ContentType); got != "application/json" {
		t.Errorf("ContentType = %q, want application/json (COPY directive carries it over)", got)
	}
	if got := head.Metadata["origin"]; got != "first" {
		t.Errorf("Metadata[origin] = %q, want first", got)
	}
}

func TestCopyObjectReplaceMetadata(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "replace")
	ctx := context.Background()
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("replace"), Key: aws.String("obj.txt"),
		Body:        bytes.NewReader([]byte("body")),
		ContentType: aws.String("text/plain"),
		Metadata:    map[string]string{"stage": "before"},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Self-copy with REPLACE is how S3 clients edit metadata in place.
	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("replace"), Key: aws.String("obj.txt"),
		CopySource:        aws.String("replace/obj.txt"),
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       aws.String("text/markdown"),
		Metadata:          map[string]string{"stage": "after"},
	}); err != nil {
		t.Fatalf("self-copy with REPLACE: %v", err)
	}

	body, out := testutil.Get(t, client, "replace", "obj.txt")
	if string(body) != "body" {
		t.Errorf("body = %q, want body (bytes must be untouched)", body)
	}
	if got := aws.ToString(out.ContentType); got != "text/markdown" {
		t.Errorf("ContentType = %q, want text/markdown", got)
	}
	if got := out.Metadata["stage"]; got != "after" {
		t.Errorf("Metadata[stage] = %q, want after", got)
	}
}

func TestCopyObjectSelfWithoutReplaceRejected(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "selfcopy")
	testutil.Put(t, client, "selfcopy", "obj.txt", []byte("x"))
	_, err := client.CopyObject(context.Background(), &s3.CopyObjectInput{
		Bucket: aws.String("selfcopy"), Key: aws.String("obj.txt"),
		CopySource: aws.String("selfcopy/obj.txt"),
	})
	testutil.AssertAPIError(t, err, "InvalidArgument")
}

func TestCopyObjectMissingSource(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "nosource")
	_, err := client.CopyObject(context.Background(), &s3.CopyObjectInput{
		Bucket: aws.String("nosource"), Key: aws.String("dst"),
		CopySource: aws.String("nosource/absent"),
	})
	testutil.AssertAPIError(t, err, "NoSuchKey")
}

func TestDeleteObjectsBulk(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "bulk")
	ctx := context.Background()
	seedKeys(t, client, "bulk", "a.txt", "b.txt", "c.txt", "keep.txt")

	out, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("bulk"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a.txt")},
			{Key: aws.String("b.txt")},
			{Key: aws.String("c.txt")},
			// S3 reports a delete of an absent key as a success.
			{Key: aws.String("never-existed.txt")},
		}},
	})
	if err != nil {
		t.Fatalf("delete objects: %v", err)
	}
	if len(out.Errors) != 0 {
		t.Errorf("unexpected per-key errors: %+v", out.Errors)
	}
	if len(out.Deleted) != 4 {
		t.Errorf("Deleted has %d entries, want 4", len(out.Deleted))
	}

	keys, _, _ := listV2(t, client, &s3.ListObjectsV2Input{Bucket: aws.String("bulk")})
	assertKeys(t, keys, []string{"keep.txt"})
}

func TestDeleteObjectsQuiet(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "quiet")
	seedKeys(t, client, "quiet", "x.txt")
	out, err := client.DeleteObjects(context.Background(), &s3.DeleteObjectsInput{
		Bucket: aws.String("quiet"),
		Delete: &types.Delete{
			Quiet:   aws.Bool(true),
			Objects: []types.ObjectIdentifier{{Key: aws.String("x.txt")}},
		},
	})
	if err != nil {
		t.Fatalf("delete objects: %v", err)
	}
	if len(out.Deleted) != 0 {
		t.Errorf("quiet mode should report nothing, got %d entries", len(out.Deleted))
	}
}
