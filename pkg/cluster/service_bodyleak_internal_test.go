package cluster

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackedResponseBody struct {
	reader  *bytes.Reader
	readEOF bool
	closed  chan<- bool
}

func (b *trackedResponseBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF {
		b.readEOF = true
	}
	return n, err
}

func (b *trackedResponseBody) Close() error {
	b.closed <- b.readEOF
	return nil
}

type testHealthRouteManager struct{}

func (testHealthRouteManager) AddHost(context.Context, string, string) error { return nil }

func (testHealthRouteManager) DelHost(context.Context, string, string) error { return nil }

func TestBGPHealthCheckLoopDrainsAndClosesResponseBodyPerPoll(t *testing.T) {
	closed := make(chan bool, 1)
	client := &http.Client{
		Transport: testRoundTripper(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: &trackedResponseBody{
					reader: bytes.NewReader([]byte("health response")),
					closed: closed,
				},
				Header:  make(http.Header),
				Request: req,
			}, nil
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Cluster{healthCheckHTTPClient: client}).bgpHealthCheckLoop(ctx, testHealthConfig("http://health.test/readyz"), testHealthRouteManager{}, "10.0.0.34/32")
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case drained := <-closed:
		if !drained {
			t.Fatal("health-check response body was closed before reaching EOF")
		}
	case <-time.After(time.Second):
		t.Fatal("health-check response body was not closed")
	}
}

func TestBGPHealthCheckLoopReusesConnection(t *testing.T) {
	requests := make(chan struct{}, 2)
	var connections atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "health response")
		requests <- struct{}{}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Cluster{healthCheckHTTPClient: server.Client()}).bgpHealthCheckLoop(ctx, testHealthConfig(server.URL), testHealthRouteManager{}, "10.0.0.34/32")
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	for range 2 {
		select {
		case <-requests:
		case <-time.After(2 * time.Second):
			t.Fatal("health check did not complete two requests")
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("health checks used %d connections, want 1", got)
	}
}

func testHealthConfig(address string) *kubevip.Config {
	return &kubevip.Config{
		NodeName: "cp-node-1",
		ControlPlaneHealthCheck: kubevip.HealthCheck{
			Address:          address,
			PeriodSeconds:    1,
			FailureThreshold: 1,
		},
	}
}
