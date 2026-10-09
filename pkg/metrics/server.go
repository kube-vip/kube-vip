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

// ServerConfig defines an observability HTTP server configuration.
type ServerConfig struct {
	// Addr sets the http server address used to expose the endpoints
	Addr string
}

// Serve exposes the Prometheus metrics endpoint on the configured address.
func Serve(ctx context.Context, config ServerConfig) error {
	return listenAndServe(ctx, config, newMetricsMux(), "prometheus")
}

// listenAndServe binds config.Addr and serves handler on it until the context
// is cancelled. name identifies the server in log messages and errors.
func listenAndServe(ctx context.Context, config ServerConfig, handler http.Handler, name string) error {
	ln, err := net.Listen("tcp", config.Addr)
	if err != nil {
		return fmt.Errorf("listening on %q for the %s HTTP server: %w", config.Addr, name, err)
	}

	return serve(ctx, ln, handler, name)
}

// serve starts handler on the provided listener
func serve(ctx context.Context, ln net.Listener, handler http.Handler, name string) error {
	srv := &http.Server{
		Handler:           handler,
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

	log.Info(name+" HTTP server started", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serving %s HTTP server: %w", name, err)
		}
		return nil
	case <-ctx.Done():
	}

	// shut down on an independent context, the caller's is already cancelled
	ctxShutDown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctxShutDown); err != nil {
		return fmt.Errorf("shutting down %s HTTP server: %w", name, err)
	}

	log.Info(name + " HTTP server stopped")

	return nil
}

// newMetricsMux builds the handler served on the metrics address. It never
// carries the profiling endpoints; those live on their own server, see
// ServePprof.
func newMetricsMux() *http.ServeMux {
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
