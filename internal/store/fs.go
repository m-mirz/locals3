package store

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// emptyETag is the MD5 of zero bytes, used for directory markers.
const emptyETag = "d41d8cd98f00b204e9800998ecf8427e"

// Options configures an FSStore.
type Options struct {
	// AutoCreateBuckets creates a bucket implicitly on first write instead of
	// returning ErrNoSuchBucket.
	AutoCreateBuckets bool
}

// FSStore stores each object as a plain file at <root>/<bucket>/<key>, with
// metadata in a parallel tree under <root>/.locals3. Files placed in the tree
// by other means are served as ordinary objects.
//
// Safe for concurrent use.
type FSStore struct {
	root string
	opts Options
	lock KeyedLock
}

var _ Store = (*FSStore)(nil)

// NewFS opens (creating if needed) a store rooted at dir.
func NewFS(dir string, opts Options) (*FSStore, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, MetaDirName, "tmp"), 0o755); err != nil {
		return nil, err
	}
	return &FSStore{root: abs, opts: opts}, nil
}

// Root returns the absolute storage directory.
func (s *FSStore) Root() string { return s.root }

// ---------- buckets ----------

func (s *FSStore) CreateBucket(_ context.Context, bucket string) error {
	if err := ValidateBucketName(bucket); err != nil {
		return err
	}
	p := BucketPath(s.root, bucket)
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return fmt.Errorf("%w: %q", ErrBucketAlreadyExists, bucket)
	}
	return os.MkdirAll(p, 0o755)
}

func (s *FSStore) DeleteBucket(_ context.Context, bucket string) error {
	if err := s.bucketExists(bucket); err != nil {
		return err
	}
	entries, err := os.ReadDir(BucketPath(s.root, bucket))
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %q", ErrBucketNotEmpty, bucket)
	}
	if err := os.Remove(BucketPath(s.root, bucket)); err != nil {
		return err
	}
	removeBucketMeta(s.root, bucket)
	return nil
}

func (s *FSStore) HeadBucket(_ context.Context, bucket string) error {
	return s.bucketExists(bucket)
}

func (s *FSStore) ListBuckets(_ context.Context) ([]BucketInfo, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]BucketInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || e.Name() == MetaDirName {
			continue
		}
		if ValidateBucketName(e.Name()) != nil {
			continue // a stray directory that could never be an S3 bucket
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BucketInfo{
			Name:         e.Name(),
			CreationDate: fi.ModTime().UTC().Truncate(time.Second),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *FSStore) bucketExists(bucket string) error {
	if err := ValidateBucketName(bucket); err != nil {
		return err
	}
	fi, err := os.Stat(BucketPath(s.root, bucket))
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("%w: %q", ErrNoSuchBucket, bucket)
	}
	return nil
}

// ---------- objects ----------

func (s *FSStore) PutObject(ctx context.Context, req PutRequest) (ObjectInfo, error) {
	if err := s.bucketExists(req.Bucket); err != nil {
		if !errors.Is(err, ErrNoSuchBucket) || !s.opts.AutoCreateBuckets {
			return ObjectInfo{}, err
		}
		if err := s.CreateBucket(ctx, req.Bucket); err != nil {
			return ObjectInfo{}, err
		}
	}
	path, err := KeyToPath(s.root, req.Bucket, req.Key)
	if err != nil {
		return ObjectInfo{}, err
	}

	unlock := s.lock.Lock(req.Bucket + "\x00" + req.Key)
	defer unlock()

	if err := s.checkConflicts(req.Bucket, req.Key, path); err != nil {
		return ObjectInfo{}, err
	}

	sc := sidecar{
		ContentType: req.ContentType, UserMeta: req.UserMeta,
		ChecksumAlgorithm: req.ChecksumAlgorithm, ChecksumValue: req.ChecksumValue,
	}
	if sc.ContentType == "" {
		sc.ContentType = DefaultContentType
	}

	if IsDirMarker(req.Key) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return ObjectInfo{}, err
		}
		sc.ETag, sc.Size = emptyETag, 0
		fi, err := os.Stat(path)
		if err != nil {
			return ObjectInfo{}, err
		}
		sc.ModTimeNano = fi.ModTime().UnixNano()
		if err := writeSidecar(s.root, req.Bucket, req.Key, sc); err != nil {
			return ObjectInfo{}, err
		}
		return ObjectInfo{
			Bucket: req.Bucket, Key: req.Key, Size: 0, ETag: emptyETag,
			LastModified: fi.ModTime().UTC().Truncate(time.Second),
			ContentType:  sc.ContentType, UserMeta: sc.UserMeta,
		}, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ObjectInfo{}, s.classifyMkdirErr(err)
	}

	// Stage in .locals3/tmp, then rename: a reader never sees a partial object.
	tmp, err := os.CreateTemp(filepath.Join(s.root, MetaDirName, "tmp"), "put-*")
	if err != nil {
		return ObjectInfo{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed
	}()

	h := md5.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), req.Body)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := tmp.Sync(); err != nil {
		return ObjectInfo{}, err
	}
	if err := tmp.Close(); err != nil {
		return ObjectInfo{}, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return ObjectInfo{}, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return ObjectInfo{}, err
	}

	fi, err := os.Stat(path)
	if err != nil {
		return ObjectInfo{}, err
	}
	sc.ETag = hex.EncodeToString(h.Sum(nil))
	sc.Size = n
	sc.ModTimeNano = fi.ModTime().UnixNano()
	if err := writeSidecar(s.root, req.Bucket, req.Key, sc); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{
		Bucket: req.Bucket, Key: req.Key, Size: n, ETag: sc.ETag,
		LastModified:      fi.ModTime().UTC().Truncate(time.Second),
		ContentType:       sc.ContentType,
		UserMeta:          sc.UserMeta,
		ChecksumAlgorithm: sc.ChecksumAlgorithm,
		ChecksumValue:     sc.ChecksumValue,
	}, nil
}

// SetObjectChecksum records a checksum on an existing object, for the case
// where it only becomes known once the body has been read in full -- an
// aws-chunked trailer, for instance.
func (s *FSStore) SetObjectChecksum(ctx context.Context, bucket, key, algorithm, value string) error {
	unlock := s.lock.Lock(bucket + "\x00" + key)
	defer unlock()

	path, err := KeyToPath(s.root, bucket, key)
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return s.notFound(bucket, key, err)
	}
	sc := readSidecar(s.root, bucket, key, fi)
	sc.ChecksumAlgorithm, sc.ChecksumValue = algorithm, value
	return writeSidecar(s.root, bucket, key, sc)
}

func (s *FSStore) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectInfo, error) {
	unlock := s.lock.RLock(bucket + "\x00" + key)
	defer unlock()

	info, path, err := s.stat(bucket, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	if IsDirMarker(key) {
		return io.NopCloser(strings.NewReader("")), info, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ObjectInfo{}, s.notFound(bucket, key, err)
	}
	return f, info, nil
}

// GetObjectRange serves a byte range without reading the whole object. The
// range is assumed already validated against the object size by the caller.
func (s *FSStore) GetObjectRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error) {
	unlock := s.lock.RLock(bucket + "\x00" + key)
	defer unlock()

	info, path, err := s.stat(bucket, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	if IsDirMarker(key) {
		return io.NopCloser(strings.NewReader("")), info, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ObjectInfo{}, s.notFound(bucket, key, err)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, ObjectInfo{}, err
		}
	}
	if length < 0 {
		return f, info, nil
	}
	return sectionCloser{Reader: io.LimitReader(f, length), Closer: f}, info, nil
}

// sectionCloser pairs a bounded reader with the file it came from.
type sectionCloser struct {
	io.Reader
	io.Closer
}

func (s *FSStore) HeadObject(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	unlock := s.lock.RLock(bucket + "\x00" + key)
	defer unlock()
	info, _, err := s.stat(bucket, key)
	return info, err
}

func (s *FSStore) DeleteObject(ctx context.Context, bucket, key string) error {
	if err := s.bucketExists(bucket); err != nil {
		return err
	}
	path, err := KeyToPath(s.root, bucket, key)
	if err != nil {
		return err
	}
	unlock := s.lock.Lock(bucket + "\x00" + key)
	defer unlock()

	// S3 DELETE is idempotent: absence is success.
	if IsDirMarker(key) {
		_ = os.Remove(path)
	} else if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	removeSidecar(s.root, bucket, key)
	s.pruneEmptyDirs(bucket, filepath.Dir(path))
	return nil
}

// ---------- helpers ----------

// stat resolves an object's metadata, without holding any lock of its own.
func (s *FSStore) stat(bucket, key string) (ObjectInfo, string, error) {
	if err := s.bucketExists(bucket); err != nil {
		return ObjectInfo{}, "", err
	}
	path, err := KeyToPath(s.root, bucket, key)
	if err != nil {
		return ObjectInfo{}, "", err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ObjectInfo{}, "", s.notFound(bucket, key, err)
	}
	if fi.IsDir() != IsDirMarker(key) {
		// A directory is only an object when an explicit marker key asks for
		// it; otherwise it is merely a prefix.
		return ObjectInfo{}, "", fmt.Errorf("%w: %q", ErrNoSuchKey, key)
	}
	if IsDirMarker(key) {
		sc := readSidecar(s.root, bucket, key, nil)
		if sc.ContentType == "" {
			sc.ContentType = DefaultContentType
		}
		return ObjectInfo{
			Bucket: bucket, Key: key, Size: 0, ETag: emptyETag,
			LastModified: fi.ModTime().UTC().Truncate(time.Second),
			ContentType:  sc.ContentType, UserMeta: sc.UserMeta,
		}, path, nil
	}
	sc := readSidecar(s.root, bucket, key, fi)
	info, err := resolveInfo(s.root, bucket, key, path, fi, sc)
	return info, path, err
}

func (s *FSStore) notFound(bucket, key string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %q", ErrNoSuchKey, key)
	}
	return err
}

// checkConflicts reports whether key can be represented in the mirror layout:
// no ancestor may already exist as a regular file, and the target itself may
// not already be a directory (unless the key is a directory marker).
func (s *FSStore) checkConflicts(bucket, key, path string) error {
	base := BucketPath(s.root, bucket)
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	cur := base
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		if fi, err := os.Stat(cur); err == nil && !fi.IsDir() {
			existing, _ := PathToKey(s.root, bucket, cur)
			return fmt.Errorf("%w: %q blocked by object %q", ErrKeyDirConflict, key, existing)
		}
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() && !IsDirMarker(key) {
		return fmt.Errorf("%w: %q is a prefix holding other objects", ErrKeyDirConflict, key)
	}
	return nil
}

// classifyMkdirErr turns "a parent component is a file" into a conflict error.
func (s *FSStore) classifyMkdirErr(err error) error {
	if errors.Is(err, os.ErrExist) || strings.Contains(err.Error(), "not a directory") {
		return fmt.Errorf("%w: %v", ErrKeyDirConflict, err)
	}
	return err
}

// pruneEmptyDirs removes directories left behind by a delete, stopping at the
// bucket root and at the first non-empty directory.
func (s *FSStore) pruneEmptyDirs(bucket, dir string) {
	base := BucketPath(s.root, bucket)
	for withinDir(base, dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
