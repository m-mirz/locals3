package s3api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mmirz/locals3/internal/store"
)

// byteRange is a resolved Range header: length is always concrete, because the
// object size is known by the time the range is parsed.
type byteRange struct {
	offset, length int64
}

// errUnsatisfiableRange is answered with 416 plus a Content-Range naming the
// object size, which is how a client learns what it should have asked for.
var errUnsatisfiableRange = newAPIError("InvalidRange",
	"The requested range is not satisfiable.", http.StatusRequestedRangeNotSatisfiable)

// parseRange interprets a single-range Range header against an object of the
// given size. It returns ok=false when the header is absent or names multiple
// ranges, which S3 answers with the whole object.
func parseRange(header string, size int64) (br byteRange, ok bool, err error) {
	if header == "" {
		return byteRange{}, false, nil
	}
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found {
		return byteRange{}, false, nil // an unknown unit is ignored, per RFC 9110
	}
	if strings.Contains(spec, ",") {
		// Multipart ranges are legal HTTP but S3 responds with the full object.
		return byteRange{}, false, nil
	}
	startStr, endStr, found := strings.Cut(spec, "-")
	if !found {
		return byteRange{}, false, errUnsatisfiableRange
	}
	startStr, endStr = strings.TrimSpace(startStr), strings.TrimSpace(endStr)

	switch {
	case startStr == "" && endStr == "":
		return byteRange{}, false, errUnsatisfiableRange

	case startStr == "": // suffix form: the last N bytes
		n, convErr := strconv.ParseInt(endStr, 10, 64)
		if convErr != nil || n < 0 {
			return byteRange{}, false, errUnsatisfiableRange
		}
		if n == 0 {
			return byteRange{}, false, errUnsatisfiableRange
		}
		if n > size {
			n = size
		}
		return byteRange{offset: size - n, length: n}, true, nil

	default:
		start, convErr := strconv.ParseInt(startStr, 10, 64)
		if convErr != nil || start < 0 {
			return byteRange{}, false, errUnsatisfiableRange
		}
		if start >= size {
			return byteRange{}, false, errUnsatisfiableRange
		}
		end := size - 1
		if endStr != "" {
			end, convErr = strconv.ParseInt(endStr, 10, 64)
			if convErr != nil || end < start {
				return byteRange{}, false, errUnsatisfiableRange
			}
			if end > size-1 {
				end = size - 1
			}
		}
		return byteRange{offset: start, length: end - start + 1}, true, nil
	}
}

// contentRange renders the Content-Range header for a satisfied range.
func (b byteRange) contentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", b.offset, b.offset+b.length-1, size)
}

// preconditionOutcome says how a conditional request should be answered.
type preconditionOutcome int

const (
	preconditionPass preconditionOutcome = iota
	// preconditionNotModified maps to 304, which carries no body.
	preconditionNotModified
	// preconditionFailed maps to 412.
	preconditionFailed
)

// checkPreconditions applies the S3 evaluation order for conditional headers.
// S3 evaluates If-Match before If-Unmodified-Since, and If-None-Match before
// If-Modified-Since; a satisfied If-None-Match wins over If-Modified-Since even
// when the object did change.
func checkPreconditions(r *http.Request, info store.ObjectInfo) preconditionOutcome {
	etag := quoteETag(info.ETag)

	if v := r.Header.Get("If-Match"); v != "" {
		if !etagMatches(v, etag) {
			return preconditionFailed
		}
	} else if v := r.Header.Get("If-Unmodified-Since"); v != "" {
		if ts, err := http.ParseTime(v); err == nil && info.LastModified.After(ts) {
			return preconditionFailed
		}
	}

	if v := r.Header.Get("If-None-Match"); v != "" {
		if etagMatches(v, etag) {
			return preconditionNotModified
		}
	} else if v := r.Header.Get("If-Modified-Since"); v != "" {
		// Second granularity: an object modified within the same second as the
		// header value counts as unmodified, which is why LastModified is
		// truncated when it is stored.
		if ts, err := http.ParseTime(v); err == nil && !info.LastModified.After(ts) {
			return preconditionNotModified
		}
	}
	return preconditionPass
}

// etagMatches evaluates a comma-separated If-Match/If-None-Match list, honouring
// the "*" wildcard and ignoring weak-comparison prefixes.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

// httpTime renders a timestamp the way S3 does in Last-Modified.
func httpTime(t time.Time) string { return t.UTC().Format(http.TimeFormat) }
