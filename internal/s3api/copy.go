package s3api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/mmirz/locals3/internal/store"
)

// copyObject serves PUT with an x-amz-copy-source header.
func (h *Handler) copyObject(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	srcBucket, srcKey, err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		writeError(w, r, rid, err)
		return
	}

	// COPY (the default) carries the source metadata over; REPLACE takes it
	// from this request's headers.
	replace := strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE")
	info, err := h.store.CopyObject(r.Context(), store.CopyRequest{
		SrcBucket:       srcBucket,
		SrcKey:          srcKey,
		DstBucket:       bucket,
		DstKey:          key,
		ReplaceMetadata: replace,
		ContentType:     r.Header.Get("Content-Type"),
		UserMeta:        userMetaFromHeaders(r.Header),
	})
	if err != nil {
		writeError(w, r, rid, err)
		return
	}
	writeXML(w, http.StatusOK, copyObjectResult{
		Xmlns:        xmlNS,
		LastModified: info.LastModified,
		ETag:         quoteETag(info.ETag),
	})
}

// parseCopySource splits "/bucket/key" or "bucket/key" into its parts. The
// value is percent-encoded, and may carry a ?versionId suffix that locals3
// ignores because it is unversioned.
func parseCopySource(v string) (string, string, error) {
	if v == "" {
		return "", "", errInvalidArgument.withMessage("x-amz-copy-source is empty")
	}
	if i := strings.Index(v, "?versionId="); i >= 0 {
		v = v[:i]
	}
	decoded, err := url.QueryUnescape(v)
	if err != nil {
		// Not percent-encoded after all; a literal key is still usable.
		decoded = v
	}
	decoded = strings.TrimPrefix(decoded, "/")
	bucket, key, ok := strings.Cut(decoded, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", errInvalidArgument.withMessage(
			"x-amz-copy-source must be \"/bucket/key\", got %q", v)
	}
	return bucket, key, nil
}
