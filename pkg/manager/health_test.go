package manager

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "net/http/pprof" //nolint:gosec // G108: the default mux is never served here, added for test purposes

	"github.com/stretchr/testify/assert"
)

const testHealthCheckPort = 8080

func TestHealthServerListensOnConfiguredPort(t *testing.T) {
	if got, want := newHealthServer(testHealthCheckPort).Addr, fmt.Sprintf(":%d", testHealthCheckPort); got != want {
		t.Errorf("healthcheck server Addr = %q, want %q", got, want)
	}
}

// TestHealthServerServesHealthz checks if server serves /healthz endpoint.
func TestHealthServerServesHealthz(t *testing.T) {
	rec := serveHealth(t, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", rec.Code, http.StatusOK)
	}

	if got := rec.Body.String(); got != "OK" {
		t.Errorf("GET /healthz body = %q, want %q", got, "OK")
	}
}

// TestHealthServerDoesNotServePprof checks if Heatlz server does not serve pprof which registers on the default mux.
func TestHealthServerDoesNotServePprof(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)

	// Guard against a vacuous test: if the default mux ever stops carrying the
	// pprof routes, the assertion below would pass for the wrong reason.
	if _, pattern := http.DefaultServeMux.Handler(req); pattern == "" {
		t.Fatal("http.DefaultServeMux has no /debug/pprof/ route, cannot prove the healthcheck server is isolated from it")
	}

	rec := serveHealth(t, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /debug/pprof/ status = %d, want %d - the healthcheck server is exposing the default mux", rec.Code, http.StatusNotFound)
	}
}

// TestHealthServerSecondStartFailsToBind starts two health servers on one
// port. Second server should return error on start, but should not break the first one.
func TestHealthServerSecondStartFailsToBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}

	first := newHealthServer(port)
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- first.ListenAndServe()
	}()
	t.Cleanup(func() {
		_ = first.Close()
		if err := <-firstErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("first healthcheck server: %v", err)
		}
	})
	if got := retryHealtz(t, port); got != "OK" {
		t.Errorf("GET /healthz body = %q, want %q", got, "OK")
	}

	second := newHealthServer(port)

	err = second.ListenAndServe()
	if err == nil {
		_ = second.Close()
		t.Fatal("second healthcheck server bound an occupied port instead of failing")
	}

	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "listen" {
		t.Fatalf("second healthcheck server error = %v, want a listen error", err)
	}

	if got := retryHealtz(t, port); got != "OK" {
		t.Errorf("after the failed bind, GET /healthz body = %q, want %q", got, "OK")
	}
}

// serveHealth routes a request through the healthcheck server's own handler.
func serveHealth(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	srv := newHealthServer(testHealthCheckPort)
	if srv.Handler == nil {
		t.Fatal("healthcheck server has a nil Handler, so it would serve http.DefaultServeMux")
	}

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	return rec
}

// retryHealtz polls /healthz until the server answers, and reports what it
// answered with.
func retryHealtz(t *testing.T, port int) string {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)

	var body string

	if success := assert.Eventually(t, func() bool {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}

		bytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("reading /healthz body: %v", readErr)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
		}

		body = string(bytes)

		return true
	}, time.Second*10, time.Millisecond*10); !success {
		t.Fatalf("healthcheck server on port %d never answered /healthz", port)
	}

	return body
}
