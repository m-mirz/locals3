// Package locals3 is a filesystem-backed S3 mock server for local development
// and tests.
//
// Objects are stored as plain files at <dir>/<bucket>/<key>, so the storage
// directory can be inspected, edited and version-controlled with ordinary
// tools, and a file dropped into it is served as an object. Metadata lives in a
// parallel tree under <dir>/.locals3.
//
// Signatures are never verified: any AWS credentials are accepted.
package locals3

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/mmirz/locals3/internal/s3api"
	"github.com/mmirz/locals3/internal/store"
)

// Options configures a Server. The zero value is usable except for Dir.
type Options struct {
	// Dir is the storage root. Created if absent.
	Dir string
	// Region is advertised to clients. Defaults to us-east-1.
	Region string
	// Domain enables virtual-host addressing ("<bucket>.<Domain>"). Empty
	// disables it, leaving path-style ("/bucket/key") only.
	Domain string
	// AutoCreateBuckets creates a bucket on first write instead of failing.
	AutoCreateBuckets bool
	// Logger receives request logs. Defaults to a discarding logger.
	Logger *slog.Logger
	// Latency delays every request, and FailRate is the fraction of requests
	// answered with 503 SlowDown. Both exist to exercise client retry paths.
	Latency  time.Duration
	FailRate float64
}

// Server is an http.Handler serving the S3 API. Safe for concurrent use.
type Server struct {
	handler http.Handler
	store   *store.FSStore
	opts    Options
}

// New opens the storage directory and builds a Server.
func New(opts Options) (*Server, error) {
	if opts.Dir == "" {
		opts.Dir = "data"
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	st, err := store.NewFS(opts.Dir, store.Options{AutoCreateBuckets: opts.AutoCreateBuckets})
	if err != nil {
		return nil, err
	}
	h := s3api.New(s3api.Config{
		Store:    st,
		Region:   opts.Region,
		Domain:   opts.Domain,
		Logger:   opts.Logger,
		Latency:  opts.Latency,
		FailRate: opts.FailRate,
	})
	return &Server{handler: h, store: st, opts: opts}, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Dir returns the absolute storage root.
func (s *Server) Dir() string { return s.store.Root() }
