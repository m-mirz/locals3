package store

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MetaDirName is the reserved directory at the storage root holding sidecar
// metadata, in-flight multipart parts and write staging. It is never listed as
// a bucket and never walked during listings. Bucket names cannot collide with
// it because S3 bucket names may not begin with a dot.
const MetaDirName = ".locals3"

// MaxKeyLength is the S3 limit on object key length, in bytes.
const MaxKeyLength = 1024

// ValidateBucketName applies the S3 bucket naming rules. It returns a wrapped
// ErrInvalidBucketName describing the specific violation.
func ValidateBucketName(name string) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: %q: %s", ErrInvalidBucketName, name, why)
	}
	if len(name) < 3 || len(name) > 63 {
		return bad("must be between 3 and 63 characters")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return bad("may only contain lowercase letters, digits, hyphens and dots")
		}
	}
	if !isAlnum(name[0]) || !isAlnum(name[len(name)-1]) {
		return bad("must begin and end with a letter or digit")
	}
	if strings.Contains(name, "..") {
		return bad("must not contain consecutive dots")
	}
	if net.ParseIP(name) != nil {
		return bad("must not be formatted as an IP address")
	}
	return nil
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// ValidateKey rejects keys that cannot be represented in the mirror layout.
//
// Beyond the S3 rules (non-empty, at most MaxKeyLength bytes) it also rejects
// empty path segments ("a//b", "/a") and dot segments ("a/./b", "a/../b"),
// which have no filesystem representation. A single trailing slash is allowed
// and denotes a zero-byte directory marker.
func ValidateKey(key string) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: %q: %s", ErrInvalidKey, key, why)
	}
	if key == "" {
		return bad("must not be empty")
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("%w: %q is %d bytes; the limit is %d",
			ErrKeyTooLong, key[:32]+"...", len(key), MaxKeyLength)
	}
	if !utf8.ValidString(key) {
		return bad("is not valid UTF-8")
	}
	if strings.ContainsRune(key, 0) {
		return bad("contains a NUL byte")
	}
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		// A single trailing slash yields a final empty segment: the directory
		// marker form, which is allowed.
		if seg == "" {
			if i == len(segs)-1 && i > 0 {
				continue
			}
			return bad("contains an empty path segment")
		}
		if seg == "." || seg == ".." {
			return bad("contains a dot path segment")
		}
	}
	return nil
}

// IsDirMarker reports whether key denotes a zero-byte directory marker.
func IsDirMarker(key string) bool {
	return strings.HasSuffix(key, "/")
}

// BucketPath returns the on-disk directory backing a bucket.
func BucketPath(root, bucket string) string {
	return filepath.Join(root, bucket)
}

// KeyToPath maps a bucket and key onto an absolute filesystem path, and
// guarantees the result lies strictly inside the bucket directory. Callers may
// pass the result to os.Open/os.Create without further checking.
func KeyToPath(root, bucket, key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	base := BucketPath(root, bucket)
	p := filepath.Join(base, filepath.FromSlash(strings.TrimSuffix(key, "/")))
	// filepath.Join already cleans; verify the cleaned result did not escape.
	if !withinDir(base, p) {
		return "", fmt.Errorf("%w: %q: resolves outside the bucket", ErrInvalidKey, key)
	}
	return p, nil
}

// PathToKey is the inverse of KeyToPath for a path known to be inside bucket.
func PathToKey(root, bucket, path string) (string, error) {
	base := BucketPath(root, bucket)
	rel, err := filepath.Rel(base, path)
	if err != nil || !withinDir(base, path) {
		return "", fmt.Errorf("%w: %q: outside the bucket", ErrInvalidKey, path)
	}
	return filepath.ToSlash(rel), nil
}

// withinDir reports whether p is strictly below dir.
func withinDir(dir, p string) bool {
	if p == dir {
		return false
	}
	return strings.HasPrefix(p, dir+string(os.PathSeparator))
}
