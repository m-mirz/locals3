// Package s3api implements the S3 HTTP wire protocol on top of a store.Store.
// Nothing in this package touches the filesystem directly.
package s3api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/mmirz/locals3/internal/store"
)

// APIError is an S3 error code together with the status it is reported under.
// The AWS SDK maps Code onto its typed errors, so the spelling matters.
type APIError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func newAPIError(code, msg string, status int) *APIError {
	return &APIError{Code: code, Message: msg, HTTPStatus: status}
}

// Canonical errors. Message text is replaced with something specific wherever
// the handler knows more.
var (
	errNoSuchBucket       = newAPIError("NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
	errNoSuchKey          = newAPIError("NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
	errBucketNotEmpty     = newAPIError("BucketNotEmpty", "The bucket you tried to delete is not empty.", http.StatusConflict)
	errBucketExists       = newAPIError("BucketAlreadyOwnedByYou", "The bucket already exists.", http.StatusConflict)
	errInvalidBucket      = newAPIError("InvalidBucketName", "The specified bucket is not valid.", http.StatusBadRequest)
	errInvalidArgument    = newAPIError("InvalidArgument", "Invalid argument.", http.StatusBadRequest)
	errInvalidRequest     = newAPIError("InvalidRequest", "Invalid request.", http.StatusBadRequest)
	errKeyTooLong         = newAPIError("KeyTooLongError", "Your key is too long.", http.StatusBadRequest)
	errBadDigest          = newAPIError("BadDigest", "The Content-MD5 or checksum you specified did not match what we received.", http.StatusBadRequest)
	errIncompleteBody     = newAPIError("IncompleteBody", "The request body terminated unexpectedly.", http.StatusBadRequest)
	errMethodNotAllowed   = newAPIError("MethodNotAllowed", "The specified method is not allowed against this resource.", http.StatusMethodNotAllowed)
	errPreconditionFailed = newAPIError("PreconditionFailed", "At least one of the preconditions you specified did not hold.", http.StatusPreconditionFailed)
	errNoSuchUpload       = newAPIError("NoSuchUpload", "The specified multipart upload does not exist.", http.StatusNotFound)
	errInvalidPart        = newAPIError("InvalidPart", "One or more of the specified parts could not be found.", http.StatusBadRequest)
	errInvalidPartOrder   = newAPIError("InvalidPartOrder", "The list of parts was not in ascending order.", http.StatusBadRequest)
	errEntityTooSmall     = newAPIError("EntityTooSmall", "Your proposed upload is smaller than the minimum allowed size.", http.StatusBadRequest)
	errAccessDenied       = newAPIError("AccessDenied", "Access Denied.", http.StatusForbidden)
	errNotImplemented     = newAPIError("NotImplemented", "This operation is not implemented by locals3.", http.StatusNotImplemented)
	errInternal           = newAPIError("InternalError", "We encountered an internal error. Please try again.", http.StatusInternalServerError)
)

// withMessage returns a copy carrying a caller-supplied message.
func (e *APIError) withMessage(format string, args ...any) *APIError {
	c := *e
	c.Message = fmt.Sprintf(format, args...)
	return &c
}

// fromStore translates a store sentinel error into its S3 equivalent. Any
// error it does not recognise becomes InternalError, which is the honest
// answer: an unmapped failure is a locals3 bug, not a client mistake.
func fromStore(err error) *APIError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNoSuchBucket):
		return errNoSuchBucket
	case errors.Is(err, store.ErrNoSuchKey):
		return errNoSuchKey
	case errors.Is(err, store.ErrBucketAlreadyExists):
		return errBucketExists
	case errors.Is(err, store.ErrBucketNotEmpty):
		return errBucketNotEmpty
	case errors.Is(err, store.ErrInvalidBucketName):
		return errInvalidBucket.withMessage("%s", err.Error())
	case errors.Is(err, store.ErrKeyDirConflict):
		// Not an S3 condition: the mirror layout cannot represent this key.
		c := *errInvalidRequest
		c.HTTPStatus = http.StatusConflict
		c.Message = err.Error()
		return &c
	case errors.Is(err, store.ErrNoSuchUpload):
		return errNoSuchUpload.withMessage("%s", err.Error())
	case errors.Is(err, store.ErrInvalidPartMeta):
		return errInvalidPartOrder
	case errors.Is(err, store.ErrInvalidPart):
		return errInvalidPart.withMessage("%s", err.Error())
	case errors.Is(err, store.ErrEntityTooSmall):
		return errEntityTooSmall.withMessage("%s", err.Error())
	case errors.Is(err, store.ErrKeyTooLong):
		return errKeyTooLong.withMessage("%s", err.Error())
	case errors.Is(err, store.ErrInvalidKey):
		return errInvalidArgument.withMessage("%s", err.Error())
	case errors.Is(err, store.ErrInvalidArgument):
		return errInvalidArgument.withMessage("%s", err.Error())
	default:
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return errInternal
	}
}

// errorResponse is the S3 XML error envelope.
type errorResponse struct {
	XMLName    xml.Name `xml:"Error"`
	Code       string   `xml:"Code"`
	Message    string   `xml:"Message"`
	Resource   string   `xml:"Resource,omitempty"`
	BucketName string   `xml:"BucketName,omitempty"`
	Key        string   `xml:"Key,omitempty"`
	RequestID  string   `xml:"RequestId"`
}

// writeError renders err as an S3 error response. HEAD requests get the status
// and headers only, since S3 omits the body there.
func writeError(w http.ResponseWriter, r *http.Request, rid string, err error) {
	apiErr := fromStore(err)
	if apiErr == nil {
		apiErr = errInternal
	}
	bucket, key := parsedTarget(r)
	body := errorResponse{
		Code:       apiErr.Code,
		Message:    apiErr.Message,
		Resource:   r.URL.Path,
		BucketName: bucket,
		Key:        key,
		RequestID:  rid,
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(apiErr.HTTPStatus)
		return
	}
	writeXML(w, apiErr.HTTPStatus, body)
}
