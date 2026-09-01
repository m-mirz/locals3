package locals3_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mmirz/locals3/internal/testutil"
)

// errorsAs lets suites call errors.As on a typed target without repeating the
// import in every file.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

func TestBucketLifecycle(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewServer(t)
	ctx := context.Background()

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("alpha")}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("alpha")}); err != nil {
		t.Fatalf("head: %v", err)
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("alpha")}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("alpha")})
	if err == nil {
		t.Fatal("head on a deleted bucket should fail")
	}
}

func TestBucketList(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewServer(t)
	ctx := context.Background()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	out, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []string
	for _, b := range out.Buckets {
		got = append(got, aws.ToString(b.Name))
	}
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted)", got, want)
		}
	}
	for _, b := range out.Buckets {
		if b.CreationDate == nil || b.CreationDate.IsZero() {
			t.Errorf("bucket %s has no creation date", aws.ToString(b.Name))
		}
	}
}

func TestBucketDuplicateCreate(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "dupe")
	_, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String("dupe")})
	testutil.AssertAPIError(t, err, "BucketAlreadyOwnedByYou")
}

func TestBucketDeleteNonEmpty(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "full")
	testutil.Put(t, client, "full", "k", []byte("v"))
	_, err := client.DeleteBucket(context.Background(), &s3.DeleteBucketInput{Bucket: aws.String("full")})
	testutil.AssertAPIError(t, err, "BucketNotEmpty")
}

func TestBucketInvalidName(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewServer(t)
	ctx := context.Background()
	// The SDK forwards these; the server is what must reject them.
	for _, name := range []string{"ab", "has_underscore", "-leading", "trailing-", "dots..pair", "192.168.0.1"} {
		t.Run(name, func(t *testing.T) {
			_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
			testutil.AssertAPIError(t, err, "InvalidBucketName")
		})
	}
}

func TestBucketMissing(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewServer(t)
	_, err := client.DeleteBucket(context.Background(), &s3.DeleteBucketInput{Bucket: aws.String("ghost")})
	testutil.AssertAPIError(t, err, "NoSuchBucket")
}
