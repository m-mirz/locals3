package store

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MinPartSize is the smallest a part may be, except for the final one. S3
// enforces this, and so does locals3: a client that works here but splits into
// 1 KB parts would fail the moment it pointed at real S3 or Ceph.
const MinPartSize = 5 << 20

// MaxParts is the largest part number S3 accepts.
const MaxParts = 10000

// Multipart sentinel errors.
var (
	ErrNoSuchUpload    = errors.New("store: no such multipart upload")
	ErrInvalidPart     = errors.New("store: part number or ETag does not match an uploaded part")
	ErrInvalidPartMeta = errors.New("store: parts must be listed in ascending part-number order")
	ErrEntityTooSmall  = errors.New("store: every part except the last must be at least 5 MiB")
)

// Part describes one uploaded part.
type Part struct {
	PartNumber   int
	ETag         string
	Size         int64
	LastModified time.Time
}

// CompletedPart is one entry of a CompleteMultipartUpload request.
type CompletedPart struct {
	PartNumber int
	ETag       string
}

// MultipartUpload is an in-flight upload.
type MultipartUpload struct {
	UploadID    string            `json:"-"`
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	ContentType string            `json:"contentType,omitempty"`
	UserMeta    map[string]string `json:"userMeta,omitempty"`
	Initiated   time.Time         `json:"initiated"`
}

// uploadsDir holds every in-flight upload, keyed by upload id.
func (s *FSStore) uploadsDir() string {
	return filepath.Join(s.root, MetaDirName, "uploads")
}

func (s *FSStore) uploadDir(uploadID string) string {
	return filepath.Join(s.uploadsDir(), uploadID)
}

// newUploadID mints an opaque, filesystem-safe identifier.
func newUploadID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// CreateMultipartUpload starts an upload and returns its id.
func (s *FSStore) CreateMultipartUpload(ctx context.Context, bucket, key, contentType string, userMeta map[string]string) (string, error) {
	if err := s.bucketExists(bucket); err != nil {
		return "", err
	}
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if contentType == "" {
		contentType = DefaultContentType
	}
	id := newUploadID()
	dir := s.uploadDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	up := MultipartUpload{
		Bucket: bucket, Key: key, ContentType: contentType,
		UserMeta: userMeta, Initiated: time.Now().UTC().Truncate(truncateUnit),
	}
	b, err := json.Marshal(up)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		return "", err
	}
	return id, nil
}

// loadUpload reads an upload's manifest, checking it belongs to bucket/key.
func (s *FSStore) loadUpload(bucket, key, uploadID string) (MultipartUpload, error) {
	if !validUploadID(uploadID) {
		return MultipartUpload{}, fmt.Errorf("%w: %q", ErrNoSuchUpload, uploadID)
	}
	b, err := os.ReadFile(filepath.Join(s.uploadDir(uploadID), "manifest.json"))
	if err != nil {
		return MultipartUpload{}, fmt.Errorf("%w: %q", ErrNoSuchUpload, uploadID)
	}
	var up MultipartUpload
	if err := json.Unmarshal(b, &up); err != nil {
		return MultipartUpload{}, fmt.Errorf("%w: %q: corrupt manifest", ErrNoSuchUpload, uploadID)
	}
	if up.Bucket != bucket || up.Key != key {
		// An upload id is only valid for the bucket and key it was created for.
		return MultipartUpload{}, fmt.Errorf("%w: %q is for %s/%s", ErrNoSuchUpload, uploadID, up.Bucket, up.Key)
	}
	up.UploadID = uploadID
	return up, nil
}

// validUploadID rejects anything that could escape the uploads directory.
func validUploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// partPath names a staged part. Zero padding keeps the directory listing in
// part-number order, which makes an interrupted upload easy to inspect.
func (s *FSStore) partPath(uploadID string, partNumber int) string {
	return filepath.Join(s.uploadDir(uploadID), fmt.Sprintf("%05d", partNumber))
}

// UploadPart stages one part and returns its ETag.
func (s *FSStore) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, body io.Reader) (Part, error) {
	if partNumber < 1 || partNumber > MaxParts {
		return Part{}, fmt.Errorf("%w: part number %d is outside 1..%d", ErrInvalidPart, partNumber, MaxParts)
	}
	if _, err := s.loadUpload(bucket, key, uploadID); err != nil {
		return Part{}, err
	}

	tmp, err := os.CreateTemp(filepath.Join(s.root, MetaDirName, "tmp"), "part-*")
	if err != nil {
		return Part{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	h := md5.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		return Part{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Part{}, err
	}
	if err := tmp.Close(); err != nil {
		return Part{}, err
	}
	dst := s.partPath(uploadID, partNumber)
	if err := os.Rename(tmpName, dst); err != nil {
		return Part{}, err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return Part{}, err
	}
	return Part{
		PartNumber:   partNumber,
		ETag:         hex.EncodeToString(h.Sum(nil)),
		Size:         n,
		LastModified: fi.ModTime().UTC().Truncate(truncateUnit),
	}, nil
}

// ListParts returns the staged parts in ascending part-number order.
func (s *FSStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]Part, error) {
	if _, err := s.loadUpload(bucket, key, uploadID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.uploadDir(uploadID))
	if err != nil {
		return nil, err
	}
	var parts []Part
	for _, e := range entries {
		if e.IsDir() || e.Name() == "manifest.json" {
			continue
		}
		num, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := s.partInfo(uploadID, num)
		if err != nil {
			continue
		}
		parts = append(parts, p)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

// partInfo reads a staged part's size, digest and timestamp.
func (s *FSStore) partInfo(uploadID string, partNumber int) (Part, error) {
	path := s.partPath(uploadID, partNumber)
	fi, err := os.Stat(path)
	if err != nil {
		return Part{}, err
	}
	etag, err := fileETag(path)
	if err != nil {
		return Part{}, err
	}
	return Part{
		PartNumber:   partNumber,
		ETag:         etag,
		Size:         fi.Size(),
		LastModified: fi.ModTime().UTC().Truncate(truncateUnit),
	}, nil
}

// AbortMultipartUpload discards an upload and every part staged for it.
func (s *FSStore) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	if _, err := s.loadUpload(bucket, key, uploadID); err != nil {
		return err
	}
	return os.RemoveAll(s.uploadDir(uploadID))
}

// CompleteMultipartUpload assembles the named parts into the final object.
//
// The result's ETag is the MD5 of the concatenated raw part digests followed by
// "-<partCount>", which is how S3 signals a multipart object. Clients such as
// the SDK's Downloader rely on that shape, so the part digests are recorded in
// the sidecar and survive a restart.
func (s *FSStore) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (ObjectInfo, error) {
	up, err := s.loadUpload(bucket, key, uploadID)
	if err != nil {
		return ObjectInfo{}, err
	}
	if len(parts) == 0 {
		return ObjectInfo{}, fmt.Errorf("%w: no parts were listed", ErrInvalidPart)
	}

	staged, err := s.ListParts(ctx, bucket, key, uploadID)
	if err != nil {
		return ObjectInfo{}, err
	}
	byNumber := make(map[int]Part, len(staged))
	for _, p := range staged {
		byNumber[p.PartNumber] = p
	}

	// Validate before writing anything, so a bad request leaves no trace.
	last := 0
	for i, want := range parts {
		if want.PartNumber <= last {
			return ObjectInfo{}, fmt.Errorf("%w: part %d follows part %d", ErrInvalidPartMeta, want.PartNumber, last)
		}
		last = want.PartNumber
		got, ok := byNumber[want.PartNumber]
		if !ok {
			return ObjectInfo{}, fmt.Errorf("%w: part %d was never uploaded", ErrInvalidPart, want.PartNumber)
		}
		if normalizeETag(want.ETag) != got.ETag {
			return ObjectInfo{}, fmt.Errorf("%w: part %d has ETag %s, expected %s",
				ErrInvalidPart, want.PartNumber, got.ETag, normalizeETag(want.ETag))
		}
		if i < len(parts)-1 && got.Size < MinPartSize {
			return ObjectInfo{}, fmt.Errorf("%w: part %d is %d bytes", ErrEntityTooSmall, want.PartNumber, got.Size)
		}
	}

	path, err := KeyToPath(s.root, bucket, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	unlock := s.lock.Lock(bucket + "\x00" + key)
	defer unlock()

	if err := s.checkConflicts(bucket, key, path); err != nil {
		return ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ObjectInfo{}, s.classifyMkdirErr(err)
	}

	tmp, err := os.CreateTemp(filepath.Join(s.root, MetaDirName, "tmp"), "complete-*")
	if err != nil {
		return ObjectInfo{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	digests := md5.New()
	partETags := make([]string, 0, len(parts))
	var total int64
	for _, want := range parts {
		p := byNumber[want.PartNumber]
		raw, err := hex.DecodeString(p.ETag)
		if err != nil {
			return ObjectInfo{}, err
		}
		digests.Write(raw)
		partETags = append(partETags, p.ETag)

		f, err := os.Open(s.partPath(uploadID, want.PartNumber))
		if err != nil {
			return ObjectInfo{}, err
		}
		n, err := io.Copy(tmp, f)
		f.Close()
		if err != nil {
			return ObjectInfo{}, err
		}
		total += n
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

	etag := fmt.Sprintf("%s-%d", hex.EncodeToString(digests.Sum(nil)), len(parts))
	fi, err := os.Stat(path)
	if err != nil {
		return ObjectInfo{}, err
	}
	sc := sidecar{
		ContentType: up.ContentType, UserMeta: up.UserMeta, ETag: etag,
		Size: total, ModTimeNano: fi.ModTime().UnixNano(), PartETags: partETags,
	}
	if err := writeSidecar(s.root, bucket, key, sc); err != nil {
		return ObjectInfo{}, err
	}
	if err := os.RemoveAll(s.uploadDir(uploadID)); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{
		Bucket: bucket, Key: key, Size: total, ETag: etag,
		LastModified: fi.ModTime().UTC().Truncate(truncateUnit),
		ContentType:  up.ContentType, UserMeta: up.UserMeta,
	}, nil
}

// ListMultipartUploads returns the in-flight uploads for a bucket.
func (s *FSStore) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartUpload, error) {
	if err := s.bucketExists(bucket); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.uploadsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []MultipartUpload
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.uploadsDir(), e.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var up MultipartUpload
		if json.Unmarshal(b, &up) != nil || up.Bucket != bucket {
			continue
		}
		up.UploadID = e.Name()
		out = append(out, up)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].UploadID < out[j].UploadID
	})
	return out, nil
}

// normalizeETag strips the quotes clients send around an ETag.
func normalizeETag(etag string) string {
	return strings.Trim(etag, `"`)
}
