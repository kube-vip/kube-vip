package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	metricsLandingMarker = "<h1>kube-vip Metrics</h1>"
	pprofLandingMarker   = "<h1>kube-vip Profiling</h1>"
	pprofIndexMarker     = "Types of profiles available"
	prometheusMarker     = "# HELP"
)

// TestServePprofServesTheProfilingMux covers the exported ServePprof function
func TestServePprofServesTheProfilingMux(t *testing.T) {
	// find and reserve addr
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}

	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServePprof(ctx, ServerConfig{Addr: addr})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveErr:
			if err != nil {
				t.Errorf("ServePprof returned an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("ServePprof did not return after the context was cancelled")
		}
	})

	base := "http://" + addr
	body, code := retryGet(t, base+"/debug/pprof/")
	if code != http.StatusOK {
		t.Fatalf("ServePprof was expected to return a status code %d, but returned: %d\n", http.StatusOK, code)
	}
	if !strings.Contains(body, pprofIndexMarker) {
		t.Fatalf("ServePprof is not serving the pprof index, got:\n%s", body)
	}

	if body, _ := retryGet(t, base+"/metrics"); strings.Contains(body, prometheusMarker) {
		t.Errorf("ServePprof is also serving Prometheus metrics, got:\n%s", body)
	}
}

// TestPprofMuxServesProfilingEndpoints checks if pprof endpoints are working OK
func TestPprofMuxServesProfilingEndpoints(t *testing.T) {
	base, stop := startServerWithHandler(t, newTestListener(t), newPprofMux())

	body, code := get(t, base+"/debug/pprof/")
	if code != http.StatusOK {
		t.Errorf("GET /debug/pprof/ status = %d, want %d", code, http.StatusOK)
	}
	if !strings.Contains(body, pprofIndexMarker) {
		t.Errorf("GET /debug/pprof/ is not the pprof index, got:\n%s", body)
	}

	for _, path := range []string{"/debug/pprof/cmdline", "/debug/pprof/heap?debug=1"} {
		body, code := get(t, base+path)
		if code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, code, http.StatusOK)
		}
		if body == "" {
			t.Errorf("GET %s returned an empty body", path)
		}
		if strings.Contains(body, pprofLandingMarker) {
			t.Errorf("GET %s fell through to the landing page instead of a pprof handler", path)
		}
	}

	if err := stop(); err != nil {
		t.Errorf("serve returned an error on shutdown: %v", err)
	}
}

// TestPprofMuxDoesNotServeMetrics and TestMetricsMuxDoesNotServePprof test if metrics and pprof servers are separated.
func TestPprofMuxDoesNotServeMetrics(t *testing.T) {
	base, stop := startServerWithHandler(t, newTestListener(t), newPprofMux())

	body, _ := get(t, base+"/metrics")
	if strings.Contains(body, prometheusMarker) {
		t.Errorf("GET /metrics on the pprof server returned Prometheus metrics, got:\n%s", body)
	}
	if !strings.Contains(body, pprofLandingMarker) {
		t.Errorf("GET /metrics on the pprof server did not fall through to its landing page, got:\n%s", body)
	}

	if err := stop(); err != nil {
		t.Errorf("serve returned an error on shutdown: %v", err)
	}
}

// TestMetricsMuxDoesNotServePprof and TestPprofMuxDoesNotServeMetrics test if pprof and metrics servers are separated.
func TestMetricsMuxDoesNotServePprof(t *testing.T) {
	RegisterPrometheusMetrics()

	metricsBase, stopMetrics := startServer(t, newTestListener(t))
	pprofBase, stopPprof := startServerWithHandler(t, newTestListener(t), newPprofMux())

	if body, _ := get(t, pprofBase+"/debug/pprof/"); !strings.Contains(body, pprofIndexMarker) {
		t.Fatalf("the pprof server did not serve its index, cannot prove the metrics server excludes it, got:\n%s", body)
	}

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/profile"} {
		body, _ := get(t, metricsBase+path)
		if !strings.Contains(body, metricsLandingMarker) {
			t.Errorf("GET %s on the metrics server did not fall through to its landing page, got:\n%s", path, body)
		}
	}

	if err := stopPprof(); err != nil {
		t.Errorf("pprof serve returned an error on shutdown: %v", err)
	}
	if err := stopMetrics(); err != nil {
		t.Errorf("metrics serve returned an error on shutdown: %v", err)
	}
}

// retryGet uses a HTTP get with retry
func retryGet(t *testing.T, url string) (string, int) {
	t.Helper()
	var body []byte
	var status int
	if success := assert.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("building request for %s: %v", url, err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			status = 0
			return false
		}
		defer resp.Body.Close()

		body, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading body of %s: %v", url, err)
		}
		status = resp.StatusCode
		return true
	}, time.Second*5, 10*time.Millisecond); !success {
		t.Fatalf("retryGet on URL %q did not succeed", url)
	}

	return string(body), status
}
