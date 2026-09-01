package locals3_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mmirz/locals3/internal/testutil"
)

// seedKeys puts a zero-length object at every key.
func seedKeys(t *testing.T, c *s3.Client, bucket string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		testutil.Put(t, c, bucket, k, []byte("x"))
	}
}

func listV2(t *testing.T, c *s3.Client, in *s3.ListObjectsV2Input) ([]string, []string, *s3.ListObjectsV2Output) {
	t.Helper()
	out, err := c.ListObjectsV2(context.Background(), in)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var keys, prefixes []string
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	for _, p := range out.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(p.Prefix))
	}
	return keys, prefixes, out
}

func TestListLexicographicOrder(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "order")
	// "a.txt" must precede "a/b" because '.' (0x2E) sorts before '/' (0x2F) --
	// the opposite of the order a filesystem walk produces, since the walk
	// descends into the directory "a" before reaching the sibling file.
	seedKeys(t, client, "order", "a/b", "a.txt", "a/c", "b", "a0")

	keys, _, _ := listV2(t, client, &s3.ListObjectsV2Input{Bucket: aws.String("order")})
	want := []string{"a.txt", "a/b", "a/c", "a0", "b"}
	assertKeys(t, keys, want)
}

func TestListPrefixAndDelimiter(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "tree")
	seedKeys(t, client, "tree",
		"photos/2025/a.jpg", "photos/2025/b.jpg", "photos/2026/c.jpg",
		"photos/cover.jpg", "docs/readme.md", "top.txt")

	t.Run("prefix only", func(t *testing.T) {
		keys, prefixes, _ := listV2(t, client, &s3.ListObjectsV2Input{
			Bucket: aws.String("tree"), Prefix: aws.String("photos/"),
		})
		assertKeys(t, keys, []string{
			"photos/2025/a.jpg", "photos/2025/b.jpg", "photos/2026/c.jpg", "photos/cover.jpg",
		})
		if len(prefixes) != 0 {
			t.Errorf("without a delimiter there should be no common prefixes, got %v", prefixes)
		}
	})

	t.Run("prefix and delimiter", func(t *testing.T) {
		keys, prefixes, _ := listV2(t, client, &s3.ListObjectsV2Input{
			Bucket: aws.String("tree"), Prefix: aws.String("photos/"), Delimiter: aws.String("/"),
		})
		assertKeys(t, keys, []string{"photos/cover.jpg"})
		assertKeys(t, prefixes, []string{"photos/2025/", "photos/2026/"})
	})

	t.Run("delimiter at root", func(t *testing.T) {
		keys, prefixes, _ := listV2(t, client, &s3.ListObjectsV2Input{
			Bucket: aws.String("tree"), Delimiter: aws.String("/"),
		})
		assertKeys(t, keys, []string{"top.txt"})
		assertKeys(t, prefixes, []string{"docs/", "photos/"})
	})

	t.Run("prefix matching a partial segment", func(t *testing.T) {
		keys, _, _ := listV2(t, client, &s3.ListObjectsV2Input{
			Bucket: aws.String("tree"), Prefix: aws.String("photos/2025/a"),
		})
		assertKeys(t, keys, []string{"photos/2025/a.jpg"})
	})

	t.Run("prefix matching nothing", func(t *testing.T) {
		keys, _, out := listV2(t, client, &s3.ListObjectsV2Input{
			Bucket: aws.String("tree"), Prefix: aws.String("nothing/here/"),
		})
		assertKeys(t, keys, nil)
		if aws.ToInt32(out.KeyCount) != 0 {
			t.Errorf("KeyCount = %d, want 0", aws.ToInt32(out.KeyCount))
		}
	})
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "paged")
	var want []string
	for i := range 25 {
		k := fmt.Sprintf("key-%02d", i)
		want = append(want, k)
		testutil.Put(t, client, "paged", k, []byte("v"))
	}

	var got []string
	var token *string
	pages := 0
	for {
		keys, _, out := listV2(t, client, &s3.ListObjectsV2Input{
			Bucket:            aws.String("paged"),
			MaxKeys:           aws.Int32(10),
			ContinuationToken: token,
		})
		pages++
		got = append(got, keys...)
		if aws.ToInt32(out.KeyCount) != int32(len(keys)) {
			t.Errorf("page %d: KeyCount = %d, want %d", pages, aws.ToInt32(out.KeyCount), len(keys))
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		if out.NextContinuationToken == nil {
			t.Fatal("truncated page carried no continuation token")
		}
		token = out.NextContinuationToken
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 {
		t.Errorf("got %d pages of 10 for 25 keys, want 3", pages)
	}
	assertKeys(t, got, want)
}

func TestListPaginatorMatchesFullListing(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "paginator")
	var want []string
	for i := range 12 {
		k := fmt.Sprintf("obj/%03d.bin", i)
		want = append(want, k)
		testutil.Put(t, client, "paginator", k, []byte("v"))
	}
	// The SDK's own paginator, rather than a hand-rolled loop.
	p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String("paginator"), MaxKeys: aws.Int32(5),
	})
	var got []string
	for p.HasMorePages() {
		page, err := p.NextPage(context.Background())
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}
		for _, o := range page.Contents {
			got = append(got, aws.ToString(o.Key))
		}
	}
	assertKeys(t, got, want)
}

func TestListStartAfter(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "startafter")
	seedKeys(t, client, "startafter", "a", "b", "c", "d")
	keys, _, _ := listV2(t, client, &s3.ListObjectsV2Input{
		Bucket: aws.String("startafter"), StartAfter: aws.String("b"),
	})
	assertKeys(t, keys, []string{"c", "d"})
}

func TestListV1(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "listv1")
	seedKeys(t, client, "listv1", "a", "b", "c")
	out, err := client.ListObjects(context.Background(), &s3.ListObjectsInput{
		Bucket: aws.String("listv1"), MaxKeys: aws.Int32(2),
	})
	if err != nil {
		t.Fatalf("list v1: %v", err)
	}
	var keys []string
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	assertKeys(t, keys, []string{"a", "b"})
	if !aws.ToBool(out.IsTruncated) {
		t.Error("IsTruncated = false, want true")
	}
	if aws.ToString(out.NextMarker) != "b" {
		t.Errorf("NextMarker = %q, want %q", aws.ToString(out.NextMarker), "b")
	}

	out2, err := client.ListObjects(context.Background(), &s3.ListObjectsInput{
		Bucket: aws.String("listv1"), Marker: out.NextMarker,
	})
	if err != nil {
		t.Fatalf("list v1 page 2: %v", err)
	}
	keys = nil
	for _, o := range out2.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	assertKeys(t, keys, []string{"c"})
}

func TestListMetadataFields(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewBucket(t, "fields")
	testutil.Put(t, client, "fields", "sized.bin", []byte("hello world"))
	out, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("fields"),
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out.Contents) != 1 {
		t.Fatalf("got %d objects, want 1", len(out.Contents))
	}
	o := out.Contents[0]
	if aws.ToInt64(o.Size) != 11 {
		t.Errorf("Size = %d, want 11", aws.ToInt64(o.Size))
	}
	if got := aws.ToString(o.ETag); got != `"5eb63bbbe01eeed093cb22bb8f5acdc3"` {
		t.Errorf("ETag = %s, want the MD5", got)
	}
	if o.LastModified == nil || o.LastModified.IsZero() {
		t.Error("LastModified is unset")
	}
	if o.StorageClass != types.ObjectStorageClassStandard {
		t.Errorf("StorageClass = %q, want STANDARD", o.StorageClass)
	}
}

func TestListMissingBucket(t *testing.T) {
	t.Parallel()
	_, client := testutil.NewServer(t)
	_, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("ghost"),
	})
	// NoSuchBucket is modeled on ListObjectsV2, so the SDK types it.
	var noSuchBucket *types.NoSuchBucket
	if !errorsAs(err, &noSuchBucket) {
		t.Fatalf("expected *types.NoSuchBucket, got %T: %v", err, err)
	}
}

func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v (%d), want %v (%d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
