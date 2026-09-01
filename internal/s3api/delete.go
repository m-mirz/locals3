package s3api

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/mmirz/locals3/internal/store"
)

// maxDeleteBody caps the DeleteObjects request body. S3 allows 1000 keys per
// call; this is a generous bound on the XML that carries them.
const maxDeleteBody = 8 << 20

// deleteObjects serves POST /<bucket>?delete, removing up to 1000 keys in one
// request. Per-key failures are reported in the body, not as a request-level
// error, so a partial failure still deletes everything it can.
func (h *Handler) deleteObjects(w http.ResponseWriter, r *http.Request, rid, bucket string) {
	body, _, _ := requestBody(r)
	raw, err := io.ReadAll(io.LimitReader(body, maxDeleteBody))
	if err != nil {
		writeError(w, r, rid, errIncompleteBody)
		return
	}
	var req deleteRequest
	if err := xml.Unmarshal(raw, &req); err != nil {
		writeError(w, r, rid, errInvalidRequest.withMessage("malformed Delete request: %v", err))
		return
	}
	if len(req.Objects) == 0 {
		writeError(w, r, rid, errInvalidRequest.withMessage("the Delete request must name at least one object"))
		return
	}
	if len(req.Objects) > store.MaxKeysLimit {
		writeError(w, r, rid, errInvalidRequest.withMessage(
			"the Delete request names %d objects; the limit is %d", len(req.Objects), store.MaxKeysLimit))
		return
	}

	out := deleteResult{Xmlns: xmlNS}
	for _, obj := range req.Objects {
		if err := h.store.DeleteObject(r.Context(), bucket, obj.Key); err != nil {
			apiErr := fromStore(err)
			out.Errors = append(out.Errors, deleteErrorEntry{
				Key: obj.Key, Code: apiErr.Code, Message: apiErr.Message,
			})
			continue
		}
		if !req.Quiet {
			out.Deleted = append(out.Deleted, deletedEntry{Key: obj.Key})
		}
	}
	writeXML(w, http.StatusOK, out)
}
