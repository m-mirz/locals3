package s3api

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/mmirz/locals3/internal/store"
)

// storageClass is the only class locals3 reports.
const storageClass = "STANDARD"

// listObjects serves both GET /<bucket> (V1) and GET /<bucket>?list-type=2.
func (h *Handler) listObjects(w http.ResponseWriter, r *http.Request, rid, bucket string, v2 bool) {
	q := r.URL.Query()

	maxKeys := store.MaxKeysLimit
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, r, rid, errInvalidArgument.withMessage("max-keys is not a valid number: %q", v))
			return
		}
		maxKeys = n
	}

	req := store.ListRequest{
		Bucket:    bucket,
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		MaxKeys:   maxKeys,
	}
	if v2 {
		req.StartAfter = q.Get("start-after")
		req.ContinuationToken = q.Get("continuation-token")
	} else {
		req.StartAfter = q.Get("marker")
	}

	res, err := h.store.ListObjects(r.Context(), req)
	if err != nil {
		writeError(w, r, rid, err)
		return
	}

	// encoding-type=url asks for the key-bearing fields to be percent-encoded,
	// which is how clients round-trip keys containing characters that are not
	// legal in XML content.
	enc := q.Get("encoding-type")
	esc := func(s string) string {
		if enc == "url" {
			return url.QueryEscape(s)
		}
		return s
	}

	out := listBucketResult{
		Xmlns:        xmlNS,
		Name:         bucket,
		Prefix:       esc(req.Prefix),
		Delimiter:    esc(req.Delimiter),
		MaxKeys:      maxKeys,
		EncodingType: enc,
		IsTruncated:  res.IsTruncated,
	}
	for _, o := range res.Objects {
		out.Contents = append(out.Contents, objectEntry{
			Key:          esc(o.Key),
			LastModified: o.LastModified,
			ETag:         quoteETag(o.ETag),
			Size:         o.Size,
			StorageClass: storageClass,
		})
	}
	for _, p := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: esc(p)})
	}

	if v2 {
		out.KeyCount = res.KeyCount
		out.ContinuationToken = req.ContinuationToken
		out.NextContinuationToken = res.NextContinuationToken
		out.StartAfter = esc(q.Get("start-after"))
	} else {
		out.Marker = esc(q.Get("marker"))
		if res.IsTruncated && len(out.Contents) > 0 {
			// V1 resumes from a key, not an opaque cursor: the last name in
			// the page, which for a rolled-up page is the common prefix.
			out.NextMarker = esc(lastName(res))
		}
	}
	writeXML(w, http.StatusOK, out)
}

// lastName returns the final entry of a page in listing order.
func lastName(res store.ListResult) string {
	last := ""
	if n := len(res.Objects); n > 0 {
		last = res.Objects[n-1].Key
	}
	if n := len(res.CommonPrefixes); n > 0 && res.CommonPrefixes[n-1] > last {
		last = res.CommonPrefixes[n-1]
	}
	return last
}
