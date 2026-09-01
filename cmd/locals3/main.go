// Command locals3 runs a filesystem-backed S3 mock server for local
// development.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mmirz/locals3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "locals3:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir        = flag.String("dir", "data", "object storage root")
		addr       = flag.String("addr", ":9000", "listen address")
		region     = flag.String("region", "us-east-1", "region advertised to clients")
		domain     = flag.String("domain", "localhost", "virtual-host suffix; empty for path-style only")
		autoCreate = flag.Bool("auto-create", false, "create buckets implicitly on first write")
		logLevel   = flag.String("log-level", "info", "error, info or debug")
		latency    = flag.Duration("latency", 0, "delay injected into every request")
		failRate   = flag.Float64("fail-rate", 0, "fraction of requests answered with 503 SlowDown")
	)
	flag.Parse()

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	srv, err := locals3.New(locals3.Options{
		Dir:               *dir,
		Region:            *region,
		Domain:            *domain,
		AutoCreateBuckets: *autoCreate,
		Logger:            log,
		Latency:           *latency,
		FailRate:          *failRate,
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
	}

	log.Info("locals3 listening",
		"addr", *addr, "dir", srv.Dir(), "region", *region,
		"auto_create", *autoCreate)
	fmt.Fprintf(os.Stderr, "\n  export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=%s\n"+
		"  aws --endpoint-url http://localhost%s s3 ls\n\n", *region, portOf(*addr))

	errCh := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		log.Info("shutting down")
		return httpSrv.Close()
	}
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "error":
		return slog.LevelError, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}

// portOf renders the ":9000" part of a listen address for the usage hint.
func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ":" + addr
}
