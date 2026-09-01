package locals3_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mmirz/locals3/internal/testutil"
)

// The point of locals3 is that the storage directory is an ordinary folder: you
// can drop fixtures into it, inspect what a program wrote, and edit a file in
// place. These tests hold that property down from both directions.

func TestDroppedFilesBecomeObjects(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "dropped")

	files := map[string]string{
		"notes.txt":           "plain text",
		"data/report.json":    `{"ok":true}`,
		"deep/a/b/c/thing.md": "# heading",
		"unicode/日本語.txt":     "unicode name",
	}
	for name, content := range files {
		path := filepath.Join(srv.Dir, "dropped", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Every dropped file must be listable...
	keys, _, _ := listV2(t, client, &s3.ListObjectsV2Input{Bucket: aws.String("dropped")})
	assertKeys(t, keys, []string{
		"data/report.json", "deep/a/b/c/thing.md", "notes.txt", "unicode/日本語.txt",
	})

	// ...and readable, with a digest and type the server worked out itself.
	for name, content := range files {
		got, out := testutil.Get(t, client, "dropped", name)
		if string(got) != content {
			t.Errorf("%s: body = %q, want %q", name, got, content)
		}
		if aws.ToString(out.ETag) != `"`+md5hex([]byte(content))+`"` {
			t.Errorf("%s: ETag = %s, want the MD5", name, aws.ToString(out.ETag))
		}
		if aws.ToString(out.ContentType) == "" {
			t.Errorf("%s: no Content-Type inferred", name)
		}
	}
	if _, out := testutil.Get(t, client, "dropped", "data/report.json"); !strings.HasPrefix(aws.ToString(out.ContentType), "application/json") {
		t.Errorf("report.json got Content-Type %q, want application/json", aws.ToString(out.ContentType))
	}
}

func TestEditedFileIsServedFresh(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "edited")
	testutil.Put(t, client, "edited", "fixture.txt", []byte("first version"))

	path := filepath.Join(srv.Dir, "edited", "fixture.txt")
	// Editing a fixture by hand is the whole point; the cached digest in the
	// sidecar must not win over the file.
	if err := os.WriteFile(path, []byte("second version, longer"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// Some filesystems have coarse timestamps, so make the change unambiguous.
	newTime := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, newTime, newTime)

	got, out := testutil.Get(t, client, "edited", "fixture.txt")
	if string(got) != "second version, longer" {
		t.Errorf("body = %q, want the edited content", got)
	}
	if want := `"` + md5hex([]byte("second version, longer")) + `"`; aws.ToString(out.ETag) != want {
		t.Errorf("ETag = %s, want %s (recomputed after the edit)", aws.ToString(out.ETag), want)
	}
	if aws.ToInt64(out.ContentLength) != 22 {
		t.Errorf("ContentLength = %d, want 22", aws.ToInt64(out.ContentLength))
	}
}

func TestWrittenObjectsLandAtPredictablePaths(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "predictable")
	ctx := context.Background()

	cases := []struct{ key, relPath string }{
		{"top.txt", "top.txt"},
		{"a/b/c.txt", filepath.Join("a", "b", "c.txt")},
		{"with space.txt", "with space.txt"},
	}
	for _, tc := range cases {
		body := []byte("content of " + tc.key)
		testutil.Put(t, client, "predictable", tc.key, body)
		full := filepath.Join(srv.Dir, "predictable", tc.relPath)
		got, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("%s: expected a file at %s: %v", tc.key, full, err)
			continue
		}
		if !bytes.Equal(got, body) {
			t.Errorf("%s: on-disk content = %q, want %q", tc.key, got, body)
		}
	}

	// Metadata lives beside the data, never inside the bucket tree.
	entries, err := os.ReadDir(filepath.Join(srv.Dir, "predictable"))
	if err != nil {
		t.Fatalf("read bucket dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".locals3") {
			t.Errorf("metadata leaked into the bucket directory: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(srv.Dir, ".locals3", "meta", "predictable", "top.txt.json")); err != nil {
		t.Errorf("expected a sidecar for top.txt: %v", err)
	}

	// And the reserved directory is not a bucket.
	out, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	for _, b := range out.Buckets {
		if aws.ToString(b.Name) == ".locals3" {
			t.Error(".locals3 was listed as a bucket")
		}
	}
}

func TestDeleteRemovesFileAndSidecar(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "cleanup")
	ctx := context.Background()
	testutil.Put(t, client, "cleanup", "nested/deep/obj.txt", []byte("x"))

	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("cleanup"), Key: aws.String("nested/deep/obj.txt"),
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.Dir, "cleanup", "nested", "deep", "obj.txt")); !os.IsNotExist(err) {
		t.Error("the file survived the delete")
	}
	if _, err := os.Stat(filepath.Join(srv.Dir, ".locals3", "meta", "cleanup", "nested", "deep", "obj.txt.json")); !os.IsNotExist(err) {
		t.Error("the sidecar survived the delete")
	}
	// Emptied directories are pruned, so the tree stays inspectable.
	if _, err := os.Stat(filepath.Join(srv.Dir, "cleanup", "nested")); !os.IsNotExist(err) {
		t.Error("an emptied directory was left behind")
	}
}

func TestSidecarLossIsRecoverable(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewBucket(t, "resilient")
	body := []byte("survives metadata loss")
	testutil.Put(t, client, "resilient", "obj.txt", body)

	// Deleting the whole metadata tree simulates a user tidying up, or a
	// storage directory checked into git without it.
	if err := os.RemoveAll(filepath.Join(srv.Dir, ".locals3", "meta")); err != nil {
		t.Fatalf("remove metadata: %v", err)
	}
	got, out := testutil.Get(t, client, "resilient", "obj.txt")
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want %q", got, body)
	}
	if want := `"` + md5hex(body) + `"`; aws.ToString(out.ETag) != want {
		t.Errorf("ETag = %s, want %s (recomputed from the file)", aws.ToString(out.ETag), want)
	}
}
