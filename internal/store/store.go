// Package store defines the object-storage backend behind the S3 wire layer.
//
// Nothing in this package knows about HTTP: it deals in buckets, keys, bytes
// and metadata. That boundary is what allows a second backend (in-memory, or a
// versioned store) to be added without touching internal/s3api.
package store

import (
	"context"
	"errors"
	"io"
	"time"
)

// Sentinel errors. The HTTP layer recovers these with errors.Is and maps them
// onto S3 error codes; it never inspects error strings.
var (
	ErrNoSuchBucket        = errors.New("store: no such bucket")
	ErrBucketAlreadyExists = errors.New("store: bucket already exists")
	ErrBucketNotEmpty      = errors.New("store: bucket not empty")
	ErrInvalidBucketName   = errors.New("store: invalid bucket name")
	ErrNoSuchKey           = errors.New("store: no such key")
	ErrKeyTooLong          = errors.New("store: key too long")
	ErrInvalidKey          = errors.New("store: invalid key")

	// ErrKeyDirConflict is returned when a key cannot be represented in the
	// mirror layout because one of its path components already exists as an
	// object (object "a" exists, and "a/b" is written, or vice versa). Real S3
	// has no such restriction; this is the one intentional divergence.
	ErrKeyDirConflict = errors.New("store: key conflicts with an existing object path")

	// ErrInvalidArgument covers malformed query parameters, such as a
	// continuation token that did not come from a previous listing.
	ErrInvalidArgument = errors.New("store: invalid argument")
)

// BucketInfo describes a bucket.
type BucketInfo struct {
	Name         string
	CreationDate time.Time
}

// ObjectInfo describes a stored object. ETag is the raw hex digest without
// surrounding quotes; the HTTP layer adds those.
type ObjectInfo struct {
	Bucket       string
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
	UserMeta     map[string]string
	// ChecksumAlgorithm and ChecksumValue record a client-supplied additional
	// checksum (CRC32, CRC32C, SHA1 or SHA256), base64-encoded as S3 reports
	// it. Empty when the client sent none.
	ChecksumAlgorithm string
	ChecksumValue     string
}

// PutRequest is a single-shot object write. Body is consumed in full.
type PutRequest struct {
	Bucket      string
	Key         string
	Body        io.Reader
	ContentType string
	UserMeta    map[string]string
	// ChecksumAlgorithm and ChecksumValue are stored alongside the object when
	// the client supplied them up front.
	ChecksumAlgorithm string
	ChecksumValue     string
}

// ListRequest describes a ListObjects query. MaxKeys <= 0 means the default.
type ListRequest struct {
	Bucket    string
	Prefix    string
	Delimiter string
	// StartAfter resumes strictly after this key. ContinuationToken, when set,
	// takes precedence; it is an opaque cursor produced by a previous call.
	StartAfter        string
	ContinuationToken string
	MaxKeys           int
}

// ListResult is one page of a listing.
type ListResult struct {
	Objects        []ObjectInfo
	CommonPrefixes []string
	IsTruncated    bool
	// NextContinuationToken is set only when IsTruncated.
	NextContinuationToken string
	// KeyCount is len(Objects) + len(CommonPrefixes).
	KeyCount int
}

// CopyRequest describes a server-side copy. When ReplaceMetadata is false the
// source object's content type and user metadata are carried over.
type CopyRequest struct {
	SrcBucket, SrcKey string
	DstBucket, DstKey string
	ReplaceMetadata   bool
	ContentType       string
	UserMeta          map[string]string
}

// Store is the backend contract. Implementations must be safe for concurrent
// use by multiple goroutines.
type Store interface {
	CreateBucket(ctx context.Context, bucket string) error
	DeleteBucket(ctx context.Context, bucket string) error
	HeadBucket(ctx context.Context, bucket string) error
	ListBuckets(ctx context.Context) ([]BucketInfo, error)

	PutObject(ctx context.Context, req PutRequest) (ObjectInfo, error)
	// GetObject returns the object body; the caller must Close it.
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectInfo, error)
	// GetObjectRange returns length bytes starting at offset. A negative
	// length reads to the end. The caller must Close the reader.
	GetObjectRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error)
	HeadObject(ctx context.Context, bucket, key string) (ObjectInfo, error)
	DeleteObject(ctx context.Context, bucket, key string) error

	// SetObjectChecksum records a checksum learned only after the body was
	// read, as happens when it arrives in an aws-chunked trailer.
	SetObjectChecksum(ctx context.Context, bucket, key, algorithm, value string) error

	ListObjects(ctx context.Context, req ListRequest) (ListResult, error)
	CopyObject(ctx context.Context, req CopyRequest) (ObjectInfo, error)
}
