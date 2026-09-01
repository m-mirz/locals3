package s3api

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mmirz/locals3/internal/store"
)

// userMetaPrefix is the header prefix carrying caller-defined metadata.
const userMetaPrefix = "X-Amz-Meta-"

// serveObject dispatches requests addressed at a bucket and key.
func (h *Handler) serveObject(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		switch {
		case r.Header.Get("X-Amz-Copy-Source") != "":
			h.copyObject(w, r, rid, bucket, key)
		case q.Has("uploadId"):
			h.uploadPart(w, r, rid, bucket, key)
		case q.Has("acl"), q.Has("tagging"):
			writeError(w, r, rid, errNotImplemented.withMessage("PutObject sub-resources are not implemented yet."))
		default:
			h.putObject(w, r, rid, bucket, key)
		}
	case http.MethodGet:
		if q.Has("uploadId") {
			h.listParts(w, r, rid, bucket, key)
			return
		}
		h.getObject(w, r, rid, bucket, key, true)
	case http.MethodHead:
		h.getObject(w, r, rid, bucket, key, false)
	case http.MethodDelete:
		if q.Has("uploadId") {
			h.abortMultipartUpload(w, r, rid, bucket, key)
			return
		}
		h.deleteObject(w, r, rid, bucket, key)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			h.createMultipartUpload(w, r, rid, bucket, key)
		case q.Has("uploadId"):
			h.completeMultipartUpload(w, r, rid, bucket, key)
		default:
			writeError(w, r, rid, errNotImplemented.withMessage("POST on an object is not implemented by locals3."))
		}
	default:
		writeError(w, r, rid, errMethodNotAllowed)
	}
}

func (h *Handler) putObject(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	body, _, chunked := requestBody(r)
	validator := newChecksumValidator(r, body, chunked)

	req := store.PutRequest{
		Bucket:      bucket,
		Key:         key,
		Body:        validator.Reader(),
		ContentType: r.Header.Get("Content-Type"),
		UserMeta:    userMetaFromHeaders(r.Header),
	}
	// A checksum sent as a header is known before the write; one sent in a
	// trailer is recorded afterwards.
	if !validator.FromTrailer() {
		req.ChecksumAlgorithm, req.ChecksumValue = validator.Algorithm(), r.Header.Get("X-Amz-Checksum-"+validator.Algorithm())
	}

	info, err := h.store.PutObject(r.Context(), req)
	if err != nil {
		writeError(w, r, rid, err)
		return
	}

	// The digests only exist once the body has streamed past, so a mismatch is
	// caught after the write and rolled back. S3 rejects the request outright,
	// and leaving a corrupt object behind would be worse than the extra work.
	if err := validator.Verify(); err != nil {
		_ = h.store.DeleteObject(r.Context(), bucket, key)
		writeError(w, r, rid, err)
		return
	}
	if validator.FromTrailer() && validator.Algorithm() != "" {
		_ = h.store.SetObjectChecksum(r.Context(), bucket, key, validator.Algorithm(), validator.Value())
	}

	w.Header().Set("ETag", quoteETag(info.ETag))
	if algo := validator.Algorithm(); algo != "" {
		w.Header().Set("x-amz-checksum-"+strings.ToLower(algo), validator.Value())
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) getObject(w http.ResponseWriter, r *http.Request, rid, bucket, key string, withBody bool) {
	info, err := h.store.HeadObject(r.Context(), bucket, key)
	if err != nil {
		writeError(w, r, rid, err)
		return
	}

	switch checkPreconditions(r, info) {
	case preconditionNotModified:
		// 304 carries no body and only the validator headers.
		w.Header().Set("ETag", quoteETag(info.ETag))
		w.Header().Set("Last-Modified", httpTime(info.LastModified))
		w.WriteHeader(http.StatusNotModified)
		return
	case preconditionFailed:
		writeError(w, r, rid, errPreconditionFailed)
		return
	}

	br, ranged, err := parseRange(r.Header.Get("Range"), info.Size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
		writeError(w, r, rid, err)
		return
	}

	writeObjectHeaders(w, info)
	writeChecksumHeaders(w, r, info)
	status := http.StatusOK
	if ranged {
		w.Header().Set("Content-Length", strconv.FormatInt(br.length, 10))
		w.Header().Set("Content-Range", br.contentRange(info.Size))
		status = http.StatusPartialContent
	}

	if !withBody {
		w.WriteHeader(status)
		return
	}

	var rc io.ReadCloser
	if ranged {
		rc, _, err = h.store.GetObjectRange(r.Context(), bucket, key, br.offset, br.length)
	} else {
		rc, _, err = h.store.GetObject(r.Context(), bucket, key)
	}
	if err != nil {
		writeError(w, r, rid, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(status)
	_, _ = io.Copy(w, rc)
}

func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	if err := h.store.DeleteObject(r.Context(), bucket, key); err != nil {
		writeError(w, r, rid, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeObjectHeaders emits the headers common to GET and HEAD.
func writeObjectHeaders(w http.ResponseWriter, info store.ObjectInfo) {
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	for k, v := range info.UserMeta {
		w.Header().Set(userMetaPrefix+k, v)
	}
}

// writeChecksumHeaders echoes a stored checksum, which S3 does only when the
// client sets ChecksumMode to ENABLED.
func writeChecksumHeaders(w http.ResponseWriter, r *http.Request, info store.ObjectInfo) {
	if info.ChecksumAlgorithm == "" || info.ChecksumValue == "" {
		return
	}
	if !strings.EqualFold(r.Header.Get("x-amz-checksum-mode"), "ENABLED") {
		return
	}
	w.Header().Set("x-amz-checksum-"+strings.ToLower(info.ChecksumAlgorithm), info.ChecksumValue)
}

// userMetaFromHeaders collects x-amz-meta-* into a map with the prefix removed
// and names lowercased, which is how S3 reports them back.
func userMetaFromHeaders(hdr http.Header) map[string]string {
	var out map[string]string
	for name, values := range hdr {
		suffix, ok := strings.CutPrefix(name, userMetaPrefix)
		if !ok || len(values) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]string, 4)
		}
		out[strings.ToLower(suffix)] = values[0]
	}
	return out
}

// quoteETag renders a raw hex digest in the quoted form S3 uses on the wire.
func quoteETag(etag string) string {
	if etag == "" {
		return ""
	}
	if strings.HasPrefix(etag, `"`) {
		return etag
	}
	return `"` + etag + `"`
}
