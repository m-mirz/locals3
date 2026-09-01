package s3api

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
)

// Clients may assert an object's integrity in three ways, and locals3 honours
// all of them so that a corrupted upload fails here exactly as it would against
// real S3 -- rather than being stored and only surfacing later:
//
//   - Content-MD5: base64 MD5, supplied up front.
//   - x-amz-checksum-<algo>: base64 CRC32/CRC32C/SHA1/SHA256, up front.
//   - x-amz-trailer: the same, but arriving after the body in aws-chunked
//     framing, so it can only be checked once the body has been read.
//
// The digests are computed while the body streams past, which is why the check
// necessarily happens after the object is written; a mismatch then rolls the
// write back.

// crc32cTable is the Castagnoli polynomial used by CRC32C.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// newAlgoHash returns the hasher for an S3 checksum algorithm name.
func newAlgoHash(algo string) (hash.Hash, bool) {
	switch strings.ToUpper(algo) {
	case "CRC32":
		return crc32.NewIEEE(), true
	case "CRC32C":
		return crc32.New(crc32cTable), true
	case "SHA1":
		return sha1.New(), true
	case "SHA256":
		return sha256.New(), true
	default:
		return nil, false
	}
}

// checksumValidator tees the request body through the hashers a client asked
// for. Verify reports the first mismatch once the body has been consumed.
type checksumValidator struct {
	reader io.Reader

	md5Hash hash.Hash
	wantMD5 string

	algo     string
	algoHash hash.Hash
	wantAlgo string

	// trailerName is set when the checksum arrives after the body.
	trailerName string
	chunked     *chunkedReader
}

// newChecksumValidator wraps body with whatever digests the request asks for.
// When the request asserts nothing, body is returned unchanged.
func newChecksumValidator(r *http.Request, body io.Reader, cr *chunkedReader) *checksumValidator {
	v := &checksumValidator{reader: body, chunked: cr}
	var writers []io.Writer

	if want := r.Header.Get("Content-MD5"); want != "" {
		v.md5Hash = md5.New()
		v.wantMD5 = want
		writers = append(writers, v.md5Hash)
	}

	// An up-front header, e.g. x-amz-checksum-crc32.
	for name := range r.Header {
		algo, ok := strings.CutPrefix(http.CanonicalHeaderKey(name), "X-Amz-Checksum-")
		if !ok {
			continue
		}
		h, known := newAlgoHash(algo)
		if !known {
			continue
		}
		v.algo, v.algoHash, v.wantAlgo = strings.ToUpper(algo), h, r.Header.Get(name)
		writers = append(writers, h)
		break
	}

	// A trailing checksum, named up front but sent after the body.
	if v.algoHash == nil && cr != nil {
		if trailer := r.Header.Get("X-Amz-Trailer"); trailer != "" {
			name := strings.TrimSpace(strings.Split(trailer, ",")[0])
			if algo, ok := strings.CutPrefix(http.CanonicalHeaderKey(name), "X-Amz-Checksum-"); ok {
				if h, known := newAlgoHash(algo); known {
					v.algo, v.algoHash, v.trailerName = strings.ToUpper(algo), h, name
					writers = append(writers, h)
				}
			}
		}
	}

	if len(writers) > 0 {
		v.reader = io.TeeReader(body, io.MultiWriter(writers...))
	}
	return v
}

// Reader returns the body to hand to the store.
func (v *checksumValidator) Reader() io.Reader { return v.reader }

// Algorithm and Value report the checksum for storage, once known.
func (v *checksumValidator) Algorithm() string { return v.algo }

func (v *checksumValidator) Value() string {
	if v.algoHash == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(v.algoHash.Sum(nil))
}

// FromTrailer reports whether the checksum only became known after the body.
func (v *checksumValidator) FromTrailer() bool { return v.trailerName != "" }

// Verify compares every asserted digest against what was actually received.
func (v *checksumValidator) Verify() error {
	if v.md5Hash != nil {
		got := base64.StdEncoding.EncodeToString(v.md5Hash.Sum(nil))
		if got != v.wantMD5 {
			return errBadDigest.withMessage(
				"the Content-MD5 you specified (%s) did not match what we received (%s)", v.wantMD5, got)
		}
	}
	if v.algoHash == nil {
		return nil
	}
	want := v.wantAlgo
	if v.trailerName != "" {
		if v.chunked == nil {
			return nil
		}
		want = v.chunked.Trailers().Get(v.trailerName)
		if want == "" {
			return nil // the client named a trailer but never sent it
		}
	}
	if got := v.Value(); got != want {
		return errBadDigest.withMessage(
			"the %s checksum you specified (%s) did not match what we received (%s)", v.algo, want, got)
	}
	return nil
}
