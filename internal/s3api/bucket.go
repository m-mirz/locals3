package s3api

import (
	"net/http"
)

func (h *Handler) listBuckets(w http.ResponseWriter, r *http.Request, rid string) {
	buckets, err := h.store.ListBuckets(r.Context())
	if err != nil {
		writeError(w, r, rid, err)
		return
	}
	var out listAllMyBucketsResult
	out.Xmlns = xmlNS
	out.Owner = defaultOwner()
	for _, b := range buckets {
		out.Buckets.Bucket = append(out.Buckets.Bucket, bucketEntry{
			Name:         b.Name,
			CreationDate: b.CreationDate,
		})
	}
	writeXML(w, http.StatusOK, out)
}

// serveBucket dispatches requests addressed at a bucket with no key.
func (h *Handler) serveBucket(w http.ResponseWriter, r *http.Request, rid, bucket string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		if unsupported := firstPresent(q, "acl", "policy", "cors", "lifecycle", "versioning", "tagging", "logging"); unsupported != "" {
			writeError(w, r, rid, errNotImplemented.withMessage("PutBucket%s is not implemented by locals3.", unsupported))
			return
		}
		// The CreateBucketConfiguration body only names a region, which locals3
		// ignores, so it is not read.
		if err := h.store.CreateBucket(r.Context(), bucket); err != nil {
			writeError(w, r, rid, err)
			return
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		if err := h.store.DeleteBucket(r.Context(), bucket); err != nil {
			writeError(w, r, rid, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodHead:
		if err := h.store.HeadBucket(r.Context(), bucket); err != nil {
			writeError(w, r, rid, err)
			return
		}
		w.Header().Set("x-amz-bucket-region", h.region)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		if err := h.store.HeadBucket(r.Context(), bucket); err != nil {
			writeError(w, r, rid, err)
			return
		}
		switch {
		case q.Has("location"):
			loc := h.region
			if loc == "us-east-1" {
				loc = "" // S3 reports the default region as an empty constraint
			}
			writeXML(w, http.StatusOK, locationConstraint{Xmlns: xmlNS, Location: loc})
		case q.Has("uploads"):
			h.listMultipartUploads(w, r, rid, bucket)
		case q.Has("versioning"):
			writeXML(w, http.StatusOK, versioningConfiguration{Xmlns: xmlNS})
		default:
			h.listObjects(w, r, rid, bucket, q.Get("list-type") == "2")
		}

	case http.MethodPost:
		if q.Has("delete") {
			h.deleteObjects(w, r, rid, bucket)
			return
		}
		writeError(w, r, rid, errNotImplemented.withMessage("POST on a bucket is not implemented by locals3."))

	default:
		writeError(w, r, rid, errMethodNotAllowed)
	}
}

// firstPresent returns the first of names present in the query, or "".
func firstPresent(q map[string][]string, names ...string) string {
	for _, n := range names {
		if _, ok := q[n]; ok {
			return n
		}
	}
	return ""
}
