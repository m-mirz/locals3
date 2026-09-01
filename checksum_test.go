package locals3_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mmirz/locals3/internal/testutil"
)

func TestChecksumAlgorithmsAccepted(t *testing.T) {
	t.Parallel()
	for _, algo := range []types.ChecksumAlgorithm{
		types.ChecksumAlgorithmCrc32,
		types.ChecksumAlgorithmCrc32c,
		types.ChecksumAlgorithmSha1,
		types.ChecksumAlgorithmSha256,
	} {
		t.Run(string(algo), func(t *testing.T) {
			t.Parallel()
			_, client := testutil.NewBucket(t, "checksums")
			body := randomBytes(4096)
			if _, err := client.PutObject(context.Background(), &s3.PutObjectInput{
				Bucket: aws.String("checksums"), Key: aws.String(string(algo) + ".bin"),
				Body:              bytes.NewReader(body),
				ChecksumAlgorithm: algo,
			}); err != nil {
				t.Fatalf("put with %s: %v", algo, err)
			}
			got, _ := testutil.Get(t, client, "checksums", string(algo)+".bin")
			if !bytes.Equal(got, body) {
				t.Error("round trip mismatch")
			}
		})
	}
}

func TestChecksumEchoedWhenRequested(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "echoed")
	ctx := context.Background()
	body := randomBytes(2048)
	put, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("echoed"), Key: aws.String("obj.bin"),
		Body:              bytes.NewReader(body),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if aws.ToString(put.ChecksumCRC32) == "" {
		t.Error("PutObject response carried no CRC32")
	}

	// S3 only returns stored checksums when the client opts in.
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("echoed"), Key: aws.String("obj.bin"),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if got, want := aws.ToString(head.ChecksumCRC32), aws.ToString(put.ChecksumCRC32); got != want {
		t.Errorf("HeadObject CRC32 = %q, want %q", got, want)
	}

	plain, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("echoed"), Key: aws.String("obj.bin"),
	})
	if err != nil {
		t.Fatalf("head without checksum mode: %v", err)
	}
	if aws.ToString(plain.ChecksumCRC32) != "" {
		t.Error("checksum returned without ChecksumMode=ENABLED")
	}
}

// TestBadDigestRejected drives raw HTTP, because the SDK computes correct
// digests by construction and cannot be made to send a wrong one.
func TestBadDigestRejected(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "digest")
	body := []byte("the real content")

	t.Run("wrong Content-MD5", func(t *testing.T) {
		wrong := base64.StdEncoding.EncodeToString(md5sum([]byte("something else")))
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/digest/bad.txt", bytes.NewReader(body))
		req.Header.Set("Content-MD5", wrong)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}

		// The rejected write must leave nothing behind.
		_, err = client.HeadObject(context.Background(), &s3.HeadObjectInput{
			Bucket: aws.String("digest"), Key: aws.String("bad.txt"),
		})
		if err == nil {
			t.Error("a rejected upload left an object behind")
		}
	})

	t.Run("correct Content-MD5", func(t *testing.T) {
		right := base64.StdEncoding.EncodeToString(md5sum(body))
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/digest/good.txt", bytes.NewReader(body))
		req.Header.Set("Content-MD5", right)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		got, _ := testutil.Get(t, client, "digest", "good.txt")
		if !bytes.Equal(got, body) {
			t.Error("body mismatch")
		}
	})

	t.Run("wrong x-amz-checksum-crc32", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/digest/badcrc.txt", bytes.NewReader(body))
		req.Header.Set("x-amz-checksum-crc32", base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 0}))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func md5sum(b []byte) []byte {
	sum := md5.Sum(b)
	return sum[:]
}
