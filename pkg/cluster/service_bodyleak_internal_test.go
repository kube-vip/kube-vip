package cluster

import (
	"context"
	"io"
	"net/http"
	"sync"
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
	closed *atomic.Int64
}

func (b *trackedResponseBody) Read([]byte) (int, error) { return 0, io.EOF }

func (b *trackedResponseBody) Close() error {
	b.closed.Add(1)
	return nil
}

type testHealthRouteManager struct{}

func (testHealthRouteManager) AddHost(context.Context, string, string) error { return nil }

func (testHealthRouteManager) DelHost(context.Context, string, string) error { return nil }

func TestBGPHealthCheckLoopClosesResponseBodyPerPoll(t *testing.T) {
	var calls atomic.Int64
	var closes atomic.Int64
	var firstPoll sync.Once
	firstPollDone := make(chan struct{})

	client := &http.Client{
		Transport: testRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			firstPoll.Do(func() { close(firstPollDone) })
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &trackedResponseBody{closed: &closes},
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	cluster := &Cluster{healthCheckHTTPClient: client}
	cfg := &kubevip.Config{
		NodeName: "cp-node-1",
		ControlPlaneHealthCheck: kubevip.HealthCheck{
			Address:          "http://health.test/readyz",
			PeriodSeconds:    1,
			FailureThreshold: 1,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cluster.bgpHealthCheckLoop(ctx, cfg, testHealthRouteManager{}, "10.0.0.34/32")
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("health check loop did not stop during test cleanup")
		}
	})

	select {
	case <-firstPollDone:
	case <-time.After(time.Second):
		t.Fatal("health check did not run")
	}

	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("expected one poll before the ticker period, got %d", calls.Load())
	}
	if closes.Load() != 1 {
		t.Fatalf("health-check response body was not closed before the next poll: got %d closes", closes.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health check loop did not stop after context cancellation")
	}
}
