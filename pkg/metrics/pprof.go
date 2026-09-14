package metrics

import (
	"context"
	log "log/slog"
	"net/http"
	"net/http/pprof" //nolint:gosec // G108: the default mux is never served
)

const (
	DefaultPprofHTTPServer = "127.0.0.1:6060"
)

// ServePprof exposes the pprof profiling endpoints on the configured address.
func ServePprof(ctx context.Context, config ServerConfig) error {
	log.Warn("pprof profiling endpoints enabled, they expose runtime internals and are unauthenticated",
		"addr", config.Addr, "path", "/debug/pprof/")

	return listenAndServe(ctx, config, newPprofMux(), "pprof")
}

// newPprofMux builds the handler served on the pprof address.
func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>
			<head><title>kube-vip</title></head>
			<body>
			<h1>kube-vip Profiling</h1>
			<p><a href="/debug/pprof/">pprof</a></p>
			</body>
			</html>`))
	})

	return mux
}
