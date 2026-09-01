package s3api

import (
	"encoding/xml"
	"net/http"
	"time"
)

// xmlNS is the namespace every S3 response body carries.
const xmlNS = "http://s3.amazonaws.com/doc/2006-03-01/"

// ownerID/ownerName identify the single synthetic account locals3 exposes.
const (
	ownerID   = "locals3"
	ownerName = "locals3"
)

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

func defaultOwner() owner { return owner{ID: ownerID, DisplayName: ownerName} }

type bucketEntry struct {
	Name         string    `xml:"Name"`
	CreationDate time.Time `xml:"CreationDate"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   owner    `xml:"Owner"`
	Buckets struct {
		Bucket []bucketEntry `xml:"Bucket"`
	} `xml:"Buckets"`
}

// locationConstraint answers GET /<bucket>?location.
type locationConstraint struct {
	XMLName  xml.Name `xml:"LocationConstraint"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:",chardata"`
}

// versioningConfiguration answers GET /<bucket>?versioning. locals3 is
// unversioned, which S3 signals with an empty Status.
type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
	Status  string   `xml:"Status,omitempty"`
}

// writeXML renders v as an S3 XML response body.
func writeXML(w http.ResponseWriter, status int, v any) {
	buf, err := xml.Marshal(v)
	if err != nil {
		http.Error(w, "marshal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(buf)
}

// objectEntry is one <Contents> row of a listing.
type objectEntry struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// listBucketResult serves both ListObjectsV2 and V1. The two differ only in
// their cursor fields, and omitempty keeps the unused pair out of each
// response.
type listBucketResult struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Xmlns          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []objectEntry  `xml:"Contents"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`

	// V2 only.
	KeyCount              int    `xml:"KeyCount,omitempty"`
	ContinuationToken     string `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string `xml:"NextContinuationToken,omitempty"`
	StartAfter            string `xml:"StartAfter,omitempty"`

	// V1 only.
	Marker     string `xml:"Marker,omitempty"`
	NextMarker string `xml:"NextMarker,omitempty"`
}

// copyObjectResult is the body of a successful CopyObject.
type copyObjectResult struct {
	XMLName      xml.Name  `xml:"CopyObjectResult"`
	Xmlns        string    `xml:"xmlns,attr"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
}

// deleteRequest is the body of POST /<bucket>?delete.
type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Quiet   bool     `xml:"Quiet"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

type deletedEntry struct {
	Key string `xml:"Key"`
}

type deleteErrorEntry struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// deleteResult is the body of a DeleteObjects response.
type deleteResult struct {
	XMLName xml.Name           `xml:"DeleteResult"`
	Xmlns   string             `xml:"xmlns,attr"`
	Deleted []deletedEntry     `xml:"Deleted"`
	Errors  []deleteErrorEntry `xml:"Error"`
}

// initiateMultipartUploadResult answers POST /<bucket>/<key>?uploads.
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// completeMultipartUploadRequest is the body naming the parts to assemble.
type completeMultipartUploadRequest struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type partEntry struct {
	PartNumber   int       `xml:"PartNumber"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
}

type listPartsResult struct {
	XMLName      xml.Name    `xml:"ListPartsResult"`
	Xmlns        string      `xml:"xmlns,attr"`
	Bucket       string      `xml:"Bucket"`
	Key          string      `xml:"Key"`
	UploadID     string      `xml:"UploadId"`
	StorageClass string      `xml:"StorageClass"`
	MaxParts     int         `xml:"MaxParts"`
	IsTruncated  bool        `xml:"IsTruncated"`
	Parts        []partEntry `xml:"Part"`
	Owner        owner       `xml:"Owner"`
	Initiator    owner       `xml:"Initiator"`
}

type uploadEntry struct {
	Key       string    `xml:"Key"`
	UploadID  string    `xml:"UploadId"`
	Initiated time.Time `xml:"Initiated"`
}

type listMultipartUploadsResult struct {
	XMLName     xml.Name      `xml:"ListMultipartUploadsResult"`
	Xmlns       string        `xml:"xmlns,attr"`
	Bucket      string        `xml:"Bucket"`
	IsTruncated bool          `xml:"IsTruncated"`
	Uploads     []uploadEntry `xml:"Upload"`
}
