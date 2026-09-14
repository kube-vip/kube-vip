package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestServeExposesKubeVipMetrics(t *testing.T) {
	RegisterPrometheusMetrics()
	version, build, node := "v1.2.3", "test-build", "node-1"
	BuildInfo.WithLabelValues(version, build, node)

	base, stop := startServer(t, newTestListener(t))

	body, code := get(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", code, http.StatusOK)
	}

	// Label names are exposed in alphabetical order.
	want := fmt.Sprintf("kube_vip_build_info{build=\"%s\",node=\"%s\",version=\"%s\"}", build, node, version)
	if !strings.Contains(body, want) {
		t.Errorf("GET /metrics body does not contain %s, got:\n%s", want, body)
	}

	if err := stop(); err != nil {
		t.Errorf("serve returned an error on shutdown: %v", err)
	}
}

func TestServeRootPageLinksToMetrics(t *testing.T) {
	base, stop := startServer(t, newTestListener(t))

	body, code := get(t, base+"/")
	if code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", code, http.StatusOK)
	}

	if !strings.Contains(body, `href="/metrics"`) {
		t.Errorf("GET / body does not link to /metrics, got:\n%s", body)
	}

	if err := stop(); err != nil {
		t.Errorf("serve returned an error on shutdown: %v", err)
	}
}

func TestServeStopsOnContextCancellation(t *testing.T) {
	ln := newTestListener(t)
	addr := ln.Addr().String()

	_, stop := startServer(t, ln)

	if err := stop(); err != nil {
		t.Fatalf("serve returned an error on shutdown: %v", err)
	}

	// Shutdown must have closed the listener, freeing the port.
	reopened, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener still bound to %s after shutdown: %v", addr, err)
	}
	_ = reopened.Close()
}

func TestServeWithAlreadyCancelledContext(t *testing.T) {
	ln := newTestListener(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, newMetricsMux(), "test")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve on an already cancelled context returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve hung on an already cancelled context")
	}
}

func TestServeReturnsErrorWhenAddressUnavailable(t *testing.T) {
	ln := newTestListener(t)
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The port is already used in newTestListener, so Serve should return an error
	if err := Serve(ctx, ServerConfig{Addr: ln.Addr().String()}); err == nil {
		t.Fatal("Serve on an address already in use returned no error")
	}
}

func TestRegisterPrometheusMetricsIsIdempotent(t *testing.T) {
	RegisterPrometheusMetrics()

	// Registering a collector that is already registered should cause panic.
	discardPanic(t, "repeated RegisterPrometheusMetrics call", RegisterPrometheusMetrics)

	// Confirm the collectors were actually registered.
	err := prometheus.DefaultRegisterer.Register(ActiveServices)

	var alreadyRegistered prometheus.AlreadyRegisteredError
	if !errors.As(err, &alreadyRegistered) {
		t.Fatalf("Register(ActiveServices) error = %v, want AlreadyRegisteredError", err)
	}
}

// newTestListener binds a loopback listener on an arbitrary free port.
func newTestListener(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on a free loopback port: %v", err)
	}

	return ln
}

// startServer runs serve on ln with the metrics handler and returns the base URL
// along with a stop function that cancels the context and reports what serve returned.
func startServer(t *testing.T, ln net.Listener) (string, func() error) {
	t.Helper()

	return startServerWithHandler(t, ln, newMetricsMux())
}

func startServerWithHandler(t *testing.T, ln net.Listener, handler http.Handler) (string, func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serve(ctx, ln, handler, "test")
	}()

	var (
		once sync.Once
		err  error
	)
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-serveErr:
			case <-time.After(10 * time.Second):
				err = errors.New("serve did not return after the context was cancelled")
			}
		})
		return err
	}
	t.Cleanup(func() {
		_ = stop()
	})

	return "http://" + ln.Addr().String(), stop
}

func get(t *testing.T, url string) (string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request for %s: %v", url, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body of %s: %v", url, err)
	}

	return string(body), resp.StatusCode
}

func discardPanic(t *testing.T, what string, fn func()) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()

	fn()
}
