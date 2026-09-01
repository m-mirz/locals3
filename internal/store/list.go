package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaxKeysLimit is the largest page S3 will return, and the default page size.
const MaxKeysLimit = 1000

// listEntry is one row of a listing before metadata is resolved. Names are
// collected from directory entries only, so a page of ten keys does not cost a
// digest computation over every object in the bucket.
type listEntry struct {
	name     string // the key, or the common prefix
	isPrefix bool
	path     string
	isMarker bool
}

func (s *FSStore) ListObjects(ctx context.Context, req ListRequest) (ListResult, error) {
	if err := s.bucketExists(req.Bucket); err != nil {
		return ListResult{}, err
	}
	maxKeys := req.MaxKeys
	if maxKeys <= 0 || maxKeys > MaxKeysLimit {
		maxKeys = MaxKeysLimit
	}

	keys, err := s.collectKeys(req.Bucket, req.Prefix)
	if err != nil {
		return ListResult{}, err
	}

	entries := groupByDelimiter(keys, req.Prefix, req.Delimiter)

	// S3 orders the merged stream of objects and common prefixes by name. A
	// filesystem walk cannot produce that order directly: directories sort
	// before their contents, but '/' (0x2F) sorts after '.' (0x2E), so "a.txt"
	// precedes "a/b" as a key while the walk yields the opposite.
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	after := req.StartAfter
	if req.ContinuationToken != "" {
		decoded, err := decodeToken(req.ContinuationToken)
		if err != nil {
			return ListResult{}, err
		}
		after = decoded
	}
	if after != "" {
		i := sort.Search(len(entries), func(i int) bool { return entries[i].name > after })
		entries = entries[i:]
	}

	var out ListResult
	if len(entries) > maxKeys {
		out.IsTruncated = true
		out.NextContinuationToken = encodeToken(entries[maxKeys-1].name)
		entries = entries[:maxKeys]
	}

	for _, e := range entries {
		if e.isPrefix {
			out.CommonPrefixes = append(out.CommonPrefixes, e.name)
			continue
		}
		info, err := s.infoForListing(req.Bucket, e)
		if err != nil {
			// A file that vanished mid-listing is not an error; S3 listings
			// are eventually consistent snapshots too.
			if os.IsNotExist(err) {
				continue
			}
			return ListResult{}, err
		}
		out.Objects = append(out.Objects, info)
	}
	out.KeyCount = len(out.Objects) + len(out.CommonPrefixes)
	return out, nil
}

// collectKeys walks the bucket and returns every key at or below prefix.
func (s *FSStore) collectKeys(bucket, prefix string) ([]listEntry, error) {
	base := BucketPath(s.root, bucket)

	// Descend directly to the deepest complete directory of the prefix instead
	// of walking the whole bucket. "a/b/pre" starts the walk at "a/b".
	start := base
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		sub := filepath.Join(base, filepath.FromSlash(prefix[:i]))
		if withinDir(base, sub) {
			if fi, err := os.Stat(sub); err == nil && fi.IsDir() {
				start = sub
			} else {
				return nil, nil // no such subtree, so no keys can match
			}
		}
	}

	var keys []listEntry
	err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if path == base {
			return nil
		}
		key, err := PathToKey(s.root, bucket, path)
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// A directory is a key only when an explicit marker was stored.
			marker := key + "/"
			if hasMarker(s.root, bucket, marker) && strings.HasPrefix(marker, prefix) {
				keys = append(keys, listEntry{name: marker, path: path, isMarker: true})
			}
			return nil
		}
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		keys = append(keys, listEntry{name: key, path: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// hasMarker reports whether a directory-marker sidecar exists for key.
func hasMarker(root, bucket, key string) bool {
	p, err := metaPath(root, bucket, key)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// groupByDelimiter collapses keys sharing a delimiter-bounded prefix into
// common prefixes, following the S3 rule: take the part of the key after the
// query prefix, and if it contains the delimiter, roll it up.
func groupByDelimiter(keys []listEntry, prefix, delimiter string) []listEntry {
	if delimiter == "" {
		return keys
	}
	seen := make(map[string]struct{})
	out := make([]listEntry, 0, len(keys))
	for _, k := range keys {
		rest := strings.TrimPrefix(k.name, prefix)
		i := strings.Index(rest, delimiter)
		if i < 0 {
			out = append(out, k)
			continue
		}
		cp := prefix + rest[:i+len(delimiter)]
		if _, dup := seen[cp]; dup {
			continue
		}
		seen[cp] = struct{}{}
		out = append(out, listEntry{name: cp, isPrefix: true})
	}
	return out
}

// infoForListing resolves the metadata for one listed key.
func (s *FSStore) infoForListing(bucket string, e listEntry) (ObjectInfo, error) {
	fi, err := os.Stat(e.path)
	if err != nil {
		return ObjectInfo{}, err
	}
	if e.isMarker {
		sc := readSidecar(s.root, bucket, e.name, nil)
		if sc.ContentType == "" {
			sc.ContentType = DefaultContentType
		}
		return ObjectInfo{
			Bucket: bucket, Key: e.name, Size: 0, ETag: emptyETag,
			LastModified: fi.ModTime().UTC().Truncate(truncateUnit),
			ContentType:  sc.ContentType, UserMeta: sc.UserMeta,
		}, nil
	}
	sc := readSidecar(s.root, bucket, e.name, fi)
	return resolveInfo(s.root, bucket, e.name, e.path, fi, sc)
}

// Continuation tokens are opaque to clients, so the encoding only has to be
// stable and URL-safe.
func encodeToken(key string) string {
	return base64.URLEncoding.EncodeToString([]byte(key))
}

func decodeToken(tok string) (string, error) {
	b, err := base64.URLEncoding.DecodeString(tok)
	if err != nil {
		return "", fmt.Errorf("%w: malformed continuation token", ErrInvalidArgument)
	}
	return string(b), nil
}
