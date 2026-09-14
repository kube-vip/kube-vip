package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	log "log/slog"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// shutdownTimeout bounds how long the server waits for in-flight requests to
// finish once the context is cancelled.
const shutdownTimeout = 5 * time.Second

// ServerConfig defines the Prometheus server configuration.
type ServerConfig struct {
	// Addr sets the http server address used to expose the metric endpoint
	Addr string
}

// Serve exposes the Prometheus metrics endpoint on the configured address.
func Serve(ctx context.Context, config ServerConfig) error {
	ln, err := net.Listen("tcp", config.Addr)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", config.Addr, err)
	}

	return serve(ctx, ln)
}

// serve starts the metrics endpoint on the provided listener
func serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           newServeMux(),
		ReadHeaderTimeout: 2 * time.Second,
	}

	wg := sync.WaitGroup{}
	defer wg.Wait()

	serveErr := make(chan error, 1)
	wg.Go(func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	})

	log.Info("prometheus HTTP server started", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serving prometheus metrics: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	// create prometheus shutdown context (independent of other contexts)
	ctxShutDown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctxShutDown); err != nil {
		return fmt.Errorf("shutting down prometheus HTTP server: %w", err)
	}

	log.Info("prometheus HTTP server stopped")

	return nil
}

func newServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>
			<head><title>kube-vip</title></head>
			<body>
			<h1>kube-vip Metrics</h1>
			<p><a href="/metrics">Metrics</a></p>
			</body>
			</html>`))
	})

	return mux
}
