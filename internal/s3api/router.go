package s3api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/mmirz/locals3/internal/store"
)

// Config configures a Handler.
type Config struct {
	Store  store.Store
	Region string
	// Domain enables virtual-host addressing: a request to
	// "<bucket>.<Domain>" is routed to that bucket. Empty disables it.
	Domain string
	Logger *slog.Logger
	// Latency delays every request, and FailRate is the fraction of requests
	// answered with 503 SlowDown. Both exist to exercise SDK retry behaviour.
	Latency  time.Duration
	FailRate float64
}

// Handler serves the S3 HTTP API. Safe for concurrent use.
type Handler struct {
	store  store.Store
	region string
	domain string
	log    *slog.Logger
	cfg    Config
}

// New builds a Handler. Store and Region must be set.
func New(cfg Config) *Handler {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{
		store:  cfg.Store,
		region: cfg.Region,
		domain: cfg.Domain,
		log:    cfg.Logger,
		cfg:    cfg,
	}
}

type ctxKey int

const targetKey ctxKey = iota

type target struct{ Bucket, Key string }

// parsedTarget returns the bucket and key the router resolved for a request.
func parsedTarget(r *http.Request) (string, string) {
	if t, ok := r.Context().Value(targetKey).(target); ok {
		return t.Bucket, t.Key
	}
	return "", ""
}

// requestID mints the identifier echoed in x-amz-request-id and error bodies.
func requestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rid := requestID()
	w.Header().Set("x-amz-request-id", rid)
	w.Header().Set("x-amz-id-2", rid)
	w.Header().Set("Server", "locals3")

	if h.cfg.Latency > 0 {
		time.Sleep(h.cfg.Latency)
	}

	bucket, key := h.resolveTarget(r)
	r = r.WithContext(context.WithValue(r.Context(), targetKey, target{bucket, key}))

	status := http.StatusOK
	sw := &statusWriter{ResponseWriter: w, status: &status}

	if err := checkPresignExpiry(r, time.Now()); err != nil {
		writeError(sw, r, rid, err)
		h.log.Debug("request", "method", r.Method, "bucket", bucket, "key", key,
			"status", status, "reason", "presigned URL expired")
		return
	}

	switch {
	case h.cfg.FailRate > 0 && mrand.Float64() < h.cfg.FailRate:
		writeError(sw, r, rid, newAPIError("SlowDown", "Please reduce your request rate.", http.StatusServiceUnavailable))
	case bucket == "":
		h.serveService(sw, r, rid)
	case key == "":
		h.serveBucket(sw, r, rid, bucket)
	default:
		h.serveObject(sw, r, rid, bucket, key)
	}

	h.log.Debug("request",
		"method", r.Method, "bucket", bucket, "key", key,
		"query", r.URL.RawQuery, "status", status,
		"access_key", accessKey(r), "duration", time.Since(start))
}

// resolveTarget splits a request into bucket and key, honouring both path-style
// (/bucket/key) and virtual-host (bucket.domain/key) addressing.
func (h *Handler) resolveTarget(r *http.Request) (string, string) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if h.domain != "" {
		host := r.Host
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		if b, ok := strings.CutSuffix(host, "."+h.domain); ok && b != "" {
			return b, path
		}
	}
	bucket, key, _ := strings.Cut(path, "/")
	return bucket, key
}

// serveService handles requests with no bucket: GET / lists buckets.
func (h *Handler) serveService(w http.ResponseWriter, r *http.Request, rid string) {
	if r.Method != http.MethodGet {
		writeError(w, r, rid, errMethodNotAllowed)
		return
	}
	h.listBuckets(w, r, rid)
}

// statusWriter records the status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status      *int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		*s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}
