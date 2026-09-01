package locals3

import (
	"net/http/httptest"
	"os"
)

// TestingT is the subset of *testing.T that NewTestServer needs. Declaring it
// here keeps the testing package out of the import graph of anything that links
// this library.
type TestingT interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
}

// TestServer is a running locals3 instance over a throwaway directory, both of
// which are torn down when the test ends.
type TestServer struct {
	// URL is the endpoint to point an S3 client at. Clients must use
	// path-style addressing.
	URL string
	// Dir is the storage root, for asserting on the files on disk.
	Dir string
	// Server is the underlying instance.
	Server *Server

	http *httptest.Server
}

// NewTestServer starts an in-process server on a temporary directory.
//
// Build a client against it with the AWS SDK, which locals3 does not depend on:
//
//	client := s3.NewFromConfig(aws.Config{
//		Region:      "us-east-1",
//		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
//	}, func(o *s3.Options) {
//		o.BaseEndpoint = aws.String(srv.URL)
//		o.UsePathStyle = true
//	})
func NewTestServer(t TestingT, opts ...func(*Options)) *TestServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "locals3-*")
	if err != nil {
		t.Fatalf("locals3: create temp dir: %v", err)
	}
	o := Options{Dir: dir}
	for _, fn := range opts {
		fn(&o)
	}
	srv, err := New(o)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("locals3: new server: %v", err)
	}
	hs := httptest.NewServer(srv)
	ts := &TestServer{URL: hs.URL, Dir: srv.Dir(), Server: srv, http: hs}
	t.Cleanup(func() {
		hs.Close()
		os.RemoveAll(dir)
	})
	return ts
}

// Close stops the server. Tests normally rely on the registered cleanup.
func (ts *TestServer) Close() { ts.http.Close() }
