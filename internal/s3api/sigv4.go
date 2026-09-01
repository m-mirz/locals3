package s3api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// locals3 does not verify signatures: any credentials work, so local
// development needs no key management. The Authorization header is still
// parsed, because the access key is useful in logs and the presigned-URL query
// carries an expiry that is worth honouring.

// accessKey extracts the access key id from either a SigV4 Authorization
// header or a presigned URL query. It returns "" when the request is unsigned.
func accessKey(r *http.Request) string {
	if v := r.URL.Query().Get("X-Amz-Credential"); v != "" {
		return credentialScopeKey(v)
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		return ""
	}
	for _, part := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256"), ",") {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, "Credential="); ok {
			return credentialScopeKey(rest)
		}
	}
	return ""
}

// credentialScopeKey pulls the key id out of "AKID/20260831/us-east-1/s3/aws4_request".
func credentialScopeKey(cred string) string {
	if i := strings.IndexByte(cred, '/'); i >= 0 {
		return cred[:i]
	}
	return cred
}

// isPresigned reports whether the request authenticates via query parameters.
func isPresigned(r *http.Request) bool {
	return r.URL.Query().Get("X-Amz-Signature") != ""
}

// presignTimeFormat is the compact ISO8601 form SigV4 uses in X-Amz-Date.
const presignTimeFormat = "20060102T150405Z"

// checkPresignExpiry rejects a presigned URL past its lifetime.
//
// The signature itself is never verified, but the expiry is: a presigned URL
// that outlived its window is the one failure mode worth reproducing locally,
// since it is the difference between a working link and a 403 in production.
func checkPresignExpiry(r *http.Request, now time.Time) error {
	if !isPresigned(r) {
		return nil
	}
	q := r.URL.Query()
	dateStr, expiresStr := q.Get("X-Amz-Date"), q.Get("X-Amz-Expires")
	if dateStr == "" || expiresStr == "" {
		return nil // not enough information to judge; stay permissive
	}
	signedAt, err := time.Parse(presignTimeFormat, dateStr)
	if err != nil {
		return nil
	}
	seconds, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || seconds <= 0 {
		return nil
	}
	if now.After(signedAt.Add(time.Duration(seconds) * time.Second)) {
		return errAccessDenied.withMessage("Request has expired.")
	}
	return nil
}
