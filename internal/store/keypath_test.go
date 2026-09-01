package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateBucketName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ok   bool
	}{
		{"abc", true},
		{"my-bucket", true},
		{"my.bucket.name", true},
		{"a1b2c3", true},
		{strings.Repeat("a", 63), true},
		{"ab", false},                    // too short
		{strings.Repeat("a", 64), false}, // too long
		{"UPPER", false},                 // uppercase
		{"has_underscore", false},        // illegal character
		{"-leading", false},              // leading hyphen
		{"trailing-", false},             // trailing hyphen
		{"double..dot", false},           // consecutive dots
		{"192.168.0.1", false},           // IP-shaped
		{".locals3", false},              // leading dot; also the reserved dir
		{"", false},
	}
	for _, tc := range tests {
		err := ValidateBucketName(tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateBucketName(%q) error = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestValidateKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key string
		ok  bool
		why string
	}{
		{"simple.txt", true, ""},
		{"a/b/c.txt", true, ""},
		{"trailing/slash/", true, "directory marker"},
		{"ünïcode/日本語.txt", true, ""},
		{"with space+plus.txt", true, ""},
		{".hidden", true, ""},
		{"a..b/c", true, "dots inside a segment are fine"},
		{strings.Repeat("k", MaxKeyLength), true, ""},
		{"", false, "empty"},
		{strings.Repeat("k", MaxKeyLength+1), false, "too long"},
		{"/leading", false, "empty leading segment"},
		{"double//slash", false, "empty segment"},
		{"a/../b", false, "dot segment"},
		{"../escape", false, "dot segment"},
		{"a/./b", false, "dot segment"},
		{"nul\x00byte", false, "NUL"},
	}
	for _, tc := range tests {
		err := ValidateKey(tc.key)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateKey(%q) error = %v, want ok=%v (%s)", tc.key, err, tc.ok, tc.why)
		}
	}
}

func TestKeyToPathStaysInsideBucket(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := BucketPath(root, "bucket")
	for _, key := range []string{"a", "a/b/c", "deep/nest/file.txt", "trailing/"} {
		p, err := KeyToPath(root, "bucket", key)
		if err != nil {
			t.Fatalf("KeyToPath(%q): %v", key, err)
		}
		if !strings.HasPrefix(p, base+string(os.PathSeparator)) {
			t.Errorf("KeyToPath(%q) = %q, which is outside %q", key, p, base)
		}
	}
	for _, key := range []string{"../escape", "a/../../escape", "/abs"} {
		if _, err := KeyToPath(root, "bucket", key); err == nil {
			t.Errorf("KeyToPath(%q) should have been rejected", key)
		}
	}
}

func TestKeyToPathRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, key := range []string{"a", "a/b/c.txt", "ünïcode/日本語.txt", "with space.txt"} {
		p, err := KeyToPath(root, "bucket", key)
		if err != nil {
			t.Fatalf("KeyToPath(%q): %v", key, err)
		}
		got, err := PathToKey(root, "bucket", p)
		if err != nil {
			t.Fatalf("PathToKey(%q): %v", p, err)
		}
		if got != key {
			t.Errorf("round trip: %q -> %q -> %q", key, p, got)
		}
	}
}

// FuzzKeyPath asserts the two properties the mapping must never violate: an
// accepted key resolves strictly inside its bucket, and it round-trips back to
// itself. A traversal that escaped the root would let a client read or clobber
// anything the process can reach.
func FuzzKeyPath(f *testing.F) {
	for _, seed := range []string{
		"a", "a/b", "../etc/passwd", "a/../../b", "", "/", "//", "a/./b",
		"日本語", "with space", strings.Repeat("x", 2000), "a\x00b", ".locals3/x",
		"a/b/", "....//....//x", "\\windows\\path",
	} {
		f.Add(seed)
	}
	root := f.TempDir()
	base := BucketPath(root, "bucket")

	f.Fuzz(func(t *testing.T, key string) {
		p, err := KeyToPath(root, "bucket", key)
		if err != nil {
			return // rejected keys carry no obligations
		}
		if !strings.HasPrefix(p, base+string(os.PathSeparator)) {
			t.Fatalf("key %q escaped the bucket: %q", key, p)
		}
		if p != filepath.Clean(p) {
			t.Fatalf("key %q produced an uncleaned path: %q", key, p)
		}
		got, err := PathToKey(root, "bucket", p)
		if err != nil {
			t.Fatalf("key %q mapped to %q, which does not map back: %v", key, p, err)
		}
		// A directory marker loses its trailing slash on disk, by design.
		want := strings.TrimSuffix(key, "/")
		if got != want {
			t.Fatalf("round trip: %q -> %q -> %q (want %q)", key, p, got, want)
		}
	})
}
