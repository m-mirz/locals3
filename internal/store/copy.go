package store

import (
	"context"
	"fmt"
	"os"
)

// CopyObject duplicates an object server-side. The copy is staged and renamed
// like any other write, so a reader never sees a partial destination, and a
// copy onto an existing key is atomic.
func (s *FSStore) CopyObject(ctx context.Context, req CopyRequest) (ObjectInfo, error) {
	if err := s.bucketExists(req.SrcBucket); err != nil {
		return ObjectInfo{}, err
	}
	if err := s.bucketExists(req.DstBucket); err != nil {
		return ObjectInfo{}, err
	}

	srcInfo, err := s.HeadObject(ctx, req.SrcBucket, req.SrcKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	srcPath, err := KeyToPath(s.root, req.SrcBucket, req.SrcKey)
	if err != nil {
		return ObjectInfo{}, err
	}

	// Copying a key onto itself without replacing metadata is the one form S3
	// rejects, because it would be a no-op.
	if req.SrcBucket == req.DstBucket && req.SrcKey == req.DstKey && !req.ReplaceMetadata {
		return ObjectInfo{}, fmt.Errorf("%w: source and destination are identical and no metadata change was requested",
			ErrInvalidArgument)
	}

	contentType, userMeta := srcInfo.ContentType, srcInfo.UserMeta
	if req.ReplaceMetadata {
		contentType, userMeta = req.ContentType, req.UserMeta
		if contentType == "" {
			contentType = DefaultContentType
		}
	}

	// Metadata-only self-copy: rewrite the sidecar and leave the bytes alone.
	if req.SrcBucket == req.DstBucket && req.SrcKey == req.DstKey {
		unlock := s.lock.Lock(req.DstBucket + "\x00" + req.DstKey)
		defer unlock()
		fi, err := os.Stat(srcPath)
		if err != nil {
			return ObjectInfo{}, err
		}
		sc := sidecar{
			ContentType: contentType, UserMeta: userMeta, ETag: srcInfo.ETag,
			Size: fi.Size(), ModTimeNano: fi.ModTime().UnixNano(),
		}
		if err := writeSidecar(s.root, req.DstBucket, req.DstKey, sc); err != nil {
			return ObjectInfo{}, err
		}
		srcInfo.ContentType, srcInfo.UserMeta = contentType, userMeta
		return srcInfo, nil
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return ObjectInfo{}, s.notFound(req.SrcBucket, req.SrcKey, err)
	}
	defer f.Close()

	return s.PutObject(ctx, PutRequest{
		Bucket:      req.DstBucket,
		Key:         req.DstKey,
		Body:        f,
		ContentType: contentType,
		UserMeta:    userMeta,
	})
}
