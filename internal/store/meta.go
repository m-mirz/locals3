package store

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultContentType matches what S3 reports for objects stored without one.
const DefaultContentType = "binary/octet-stream"

// truncateUnit is the timestamp granularity S3 reports. Conditional headers
// compare at second precision, so storing more would make If-Modified-Since
// behave inconsistently with what clients were told.
const truncateUnit = time.Second

// sidecar is the JSON metadata stored alongside (not inside) an object.
//
// The object file itself is authoritative: if Size or ModTimeNano disagree with
// the file on disk, the file was edited outside locals3, so ETag and Size are
// recomputed while the declared ContentType and UserMeta are kept.
type sidecar struct {
	ContentType string            `json:"contentType,omitempty"`
	UserMeta    map[string]string `json:"userMeta,omitempty"`
	ETag        string            `json:"etag,omitempty"`
	Size        int64             `json:"size"`
	ModTimeNano int64             `json:"modTimeNano"`
	PartETags   []string          `json:"partETags,omitempty"`
	// ChecksumAlgorithm/ChecksumValue hold an additional client-supplied
	// checksum, base64-encoded, echoed back when a client asks for it.
	ChecksumAlgorithm string `json:"checksumAlgorithm,omitempty"`
	ChecksumValue     string `json:"checksumValue,omitempty"`
}

// metaRoot is the directory holding sidecars for every bucket.
func metaRoot(root string) string {
	return filepath.Join(root, MetaDirName, "meta")
}

// metaPath returns the sidecar path for an object. Directory markers get a
// distinct suffix so that the marker "a/b/" and the object "a/b" cannot
// collide in the metadata tree.
func metaPath(root, bucket, key string) (string, error) {
	rel, err := KeyToPath(metaRoot(root), bucket, key)
	if err != nil {
		return "", err
	}
	if IsDirMarker(key) {
		return rel + ".dirmarker.json", nil
	}
	return rel + ".json", nil
}

// readSidecar loads metadata for an object, reconciling it against the file on
// disk. It never fails on a missing or corrupt sidecar: a file with no usable
// metadata is still a perfectly good object, which is what makes hand-dropped
// files work.
func readSidecar(root, bucket, key string, fi fs.FileInfo) sidecar {
	var sc sidecar
	p, err := metaPath(root, bucket, key)
	if err == nil {
		if b, err := os.ReadFile(p); err == nil {
			if err := json.Unmarshal(b, &sc); err != nil {
				sc = sidecar{}
			}
		}
	}
	if fi != nil && (sc.Size != fi.Size() || sc.ModTimeNano != fi.ModTime().UnixNano()) {
		// Written or edited outside locals3: drop the digest, keep declarations.
		sc.ETag = ""
		sc.PartETags = nil
		sc.Size = fi.Size()
		sc.ModTimeNano = fi.ModTime().UnixNano()
	}
	return sc
}

// writeSidecar persists metadata, creating the parallel tree as needed. A
// failure to write is reported, but callers that are merely caching a computed
// digest may ignore it so that a read-only storage root still serves objects.
func writeSidecar(root, bucket, key string, sc sidecar) error {
	p, err := metaPath(root, bucket, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// removeSidecar deletes an object's metadata, ignoring absence.
func removeSidecar(root, bucket, key string) {
	if p, err := metaPath(root, bucket, key); err == nil {
		_ = os.Remove(p)
	}
}

// removeBucketMeta drops the whole metadata subtree for a bucket.
func removeBucketMeta(root, bucket string) {
	_ = os.RemoveAll(filepath.Join(metaRoot(root), bucket))
}

// fileETag streams a file and returns its hex MD5, matching the S3 ETag for a
// single-part upload.
func fileETag(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sniffContentType guesses a type for a file that arrived without one: by
// extension first, then by content, then the S3 default.
func sniffContentType(path string) string {
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		return ct
	}
	f, err := os.Open(path)
	if err != nil {
		return DefaultContentType
	}
	defer f.Close()
	var buf [512]byte
	n, err := f.Read(buf[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return DefaultContentType
	}
	ct := http.DetectContentType(buf[:n])
	// DetectContentType never returns "unknown"; it falls back to
	// application/octet-stream, which we normalise to the S3 spelling.
	if strings.HasPrefix(ct, "application/octet-stream") {
		return DefaultContentType
	}
	return ct
}

// resolve fills in any metadata the sidecar could not supply, caching the
// result back to disk on a best-effort basis.
func resolveInfo(root, bucket, key, path string, fi fs.FileInfo, sc sidecar) (ObjectInfo, error) {
	dirty := false
	if sc.ETag == "" {
		et, err := fileETag(path)
		if err != nil {
			return ObjectInfo{}, err
		}
		sc.ETag = et
		dirty = true
	}
	if sc.ContentType == "" {
		sc.ContentType = sniffContentType(path)
		dirty = true
	}
	sc.Size = fi.Size()
	sc.ModTimeNano = fi.ModTime().UnixNano()
	if dirty {
		_ = writeSidecar(root, bucket, key, sc)
	}
	return ObjectInfo{
		Bucket:            bucket,
		Key:               key,
		Size:              fi.Size(),
		ETag:              sc.ETag,
		LastModified:      fi.ModTime().UTC().Truncate(truncateUnit),
		ContentType:       sc.ContentType,
		UserMeta:          sc.UserMeta,
		ChecksumAlgorithm: sc.ChecksumAlgorithm,
		ChecksumValue:     sc.ChecksumValue,
	}, nil
}
