// Package testutil builds AWS SDK clients pointed at an in-process locals3
// instance. Correctness of locals3 is defined by what the real SDK accepts, so
// every suite drives the server through this client rather than through
// hand-rolled HTTP.
package testutil

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/mmirz/locals3"
)

// Region is the region every test client is configured with.
const Region = "us-east-1"

// Client builds an S3 client for an endpoint. Retries are disabled so that a
// failing assertion surfaces immediately instead of after a backoff sequence.
func Client(endpoint string, optFns ...func(*s3.Options)) *s3.Client {
	cfg := aws.Config{
		Region:      Region,
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}
	base := func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	}
	return s3.NewFromConfig(cfg, append([]func(*s3.Options){base}, optFns...)...)
}

// NewServer starts a locals3 instance on a temporary directory and returns it
// alongside a client wired to it.
func NewServer(t *testing.T, opts ...func(*locals3.Options)) (*locals3.TestServer, *s3.Client) {
	t.Helper()
	srv := locals3.NewTestServer(t, opts...)
	return srv, Client(srv.URL)
}

// NewBucket starts a server and creates a bucket in it.
func NewBucket(t *testing.T, bucket string) (*locals3.TestServer, *s3.Client) {
	t.Helper()
	srv, client := NewServer(t)
	if _, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		t.Fatalf("create bucket %q: %v", bucket, err)
	}
	return srv, client
}

// Put stores an object and returns its ETag.
func Put(t *testing.T, c *s3.Client, bucket, key string, body []byte) string {
	t.Helper()
	out, err := c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
	return aws.ToString(out.ETag)
}

// Get reads an object back in full.
func Get(t *testing.T, c *s3.Client, bucket, key string) ([]byte, *s3.GetObjectOutput) {
	t.Helper()
	out, err := c.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("get %s/%s: %v", bucket, key, err)
	}
	defer out.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(out.Body); err != nil {
		t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return buf.Bytes(), out
}

// AssertAPIError fails unless err carries the given S3 error code. The SDK
// exposes the code through smithy.APIError, so this works for both the typed
// errors (NoSuchKey) and the generic ones.
func AssertAPIError(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", code)
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected smithy.APIError with code %q, got %T: %v", code, err, err)
	}
	if apiErr.ErrorCode() != code {
		t.Fatalf("expected error code %q, got %q (%v)", code, apiErr.ErrorCode(), err)
	}
}
