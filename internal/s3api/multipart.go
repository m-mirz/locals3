package s3api

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"

	"github.com/mmirz/locals3/internal/store"
)

// maxCompleteBody caps the CompleteMultipartUpload body: 10000 parts of roughly
// 100 bytes of XML each, with room to spare.
const maxCompleteBody = 4 << 20

// multipartStore is the part of the backend that handles multipart uploads.
// The Store interface deliberately stops at single-shot objects, so this is
// asserted at the point of use and reported as NotImplemented otherwise.
type multipartStore interface {
	CreateMultipartUpload(ctx context.Context, bucket, key, contentType string, userMeta map[string]string) (string, error)
	UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, body io.Reader) (store.Part, error)
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]store.Part, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []store.CompletedPart) (store.ObjectInfo, error)
	ListMultipartUploads(ctx context.Context, bucket string) ([]store.MultipartUpload, error)
}

// multipart returns the backend's multipart support, if it has any.
func (h *Handler) multipart() (multipartStore, bool) {
	ms, ok := h.store.(multipartStore)
	return ms, ok
}

func (h *Handler) createMultipartUpload(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	ms, ok := h.multipart()
	if !ok {
		writeError(w, r, rid, errNotImplemented.withMessage("this backend does not support multipart upload"))
		return
	}
	id, err := ms.CreateMultipartUpload(r.Context(), bucket, key,
		r.Header.Get("Content-Type"), userMetaFromHeaders(r.Header))
	if err != nil {
		writeError(w, r, rid, err)
		return
	}
	writeXML(w, http.StatusOK, initiateMultipartUploadResult{
		Xmlns: xmlNS, Bucket: bucket, Key: key, UploadID: id,
	})
}

func (h *Handler) uploadPart(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	ms, ok := h.multipart()
	if !ok {
		writeError(w, r, rid, errNotImplemented.withMessage("this backend does not support multipart upload"))
		return
	}
	q := r.URL.Query()
	partNumber, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		writeError(w, r, rid, errInvalidArgument.withMessage("partNumber is not a number: %q", q.Get("partNumber")))
		return
	}
	body, _, _ := requestBody(r)
	part, err := ms.UploadPart(r.Context(), bucket, key, q.Get("uploadId"), partNumber, body)
	if err != nil {
		writeError(w, r, rid, err)
		return
	}
	w.Header().Set("ETag", quoteETag(part.ETag))
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) completeMultipartUpload(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	ms, ok := h.multipart()
	if !ok {
		writeError(w, r, rid, errNotImplemented.withMessage("this backend does not support multipart upload"))
		return
	}
	body, _, _ := requestBody(r)
	raw, err := io.ReadAll(io.LimitReader(body, maxCompleteBody))
	if err != nil {
		writeError(w, r, rid, errIncompleteBody)
		return
	}
	var req completeMultipartUploadRequest
	if err := xml.Unmarshal(raw, &req); err != nil {
		writeError(w, r, rid, errInvalidRequest.withMessage("malformed CompleteMultipartUpload body: %v", err))
		return
	}
	parts := make([]store.CompletedPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, store.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag})
	}

	info, err := ms.CompleteMultipartUpload(r.Context(), bucket, key, r.URL.Query().Get("uploadId"), parts)
	if err != nil {
		writeError(w, r, rid, err)
		return
	}
	writeXML(w, http.StatusOK, completeMultipartUploadResult{
		Xmlns:    xmlNS,
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     quoteETag(info.ETag),
	})
}

func (h *Handler) abortMultipartUpload(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	ms, ok := h.multipart()
	if !ok {
		writeError(w, r, rid, errNotImplemented.withMessage("this backend does not support multipart upload"))
		return
	}
	if err := ms.AbortMultipartUpload(r.Context(), bucket, key, r.URL.Query().Get("uploadId")); err != nil {
		writeError(w, r, rid, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listParts(w http.ResponseWriter, r *http.Request, rid, bucket, key string) {
	ms, ok := h.multipart()
	if !ok {
		writeError(w, r, rid, errNotImplemented.withMessage("this backend does not support multipart upload"))
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	parts, err := ms.ListParts(r.Context(), bucket, key, uploadID)
	if err != nil {
		writeError(w, r, rid, err)
		return
	}
	out := listPartsResult{
		Xmlns: xmlNS, Bucket: bucket, Key: key, UploadID: uploadID,
		StorageClass: storageClass, MaxParts: store.MaxParts,
		Owner: defaultOwner(), Initiator: defaultOwner(),
	}
	for _, p := range parts {
		out.Parts = append(out.Parts, partEntry{
			PartNumber:   p.PartNumber,
			LastModified: p.LastModified,
			ETag:         quoteETag(p.ETag),
			Size:         p.Size,
		})
	}
	writeXML(w, http.StatusOK, out)
}

func (h *Handler) listMultipartUploads(w http.ResponseWriter, r *http.Request, rid, bucket string) {
	ms, ok := h.multipart()
	if !ok {
		writeError(w, r, rid, errNotImplemented.withMessage("this backend does not support multipart upload"))
		return
	}
	uploads, err := ms.ListMultipartUploads(r.Context(), bucket)
	if err != nil {
		writeError(w, r, rid, err)
		return
	}
	out := listMultipartUploadsResult{Xmlns: xmlNS, Bucket: bucket}
	for _, u := range uploads {
		out.Uploads = append(out.Uploads, uploadEntry{
			Key: u.Key, UploadID: u.UploadID, Initiated: u.Initiated,
		})
	}
	writeXML(w, http.StatusOK, out)
}
