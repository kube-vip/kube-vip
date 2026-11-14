
package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestARPHealthCheckLoop_RelinquishesAfterThreshold(t *testing.T) {
	t.Parallel()

	statuses := []int{
		http.StatusServiceUnavailable,
		http.StatusOK,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}

	var checks atomic.Int32
	handler := func(w http.ResponseWriter, _ *http.Request) {
		i := int(checks.Add(1)) - 1
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		w.WriteHeader(statuses[i])
	}
	healthcheck := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(healthcheck.Close)

	cfg := newARPHealthCheckConfig(healthcheck.URL)
	cfg.ControlPlaneHealthCheck.FailureThreshold = 2
	c, err := InitCluster(cfg, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relinquished := make(chan struct{})
	go c.arpHealthCheckLoop(ctx, cfg, func() { close(relinquished) })

	select {
	case <-relinquished:
		if got := checks.Load(); got != int32(len(statuses)) {
			t.Fatalf("health checks before relinquishing = %d, want %d", got, len(statuses))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leadership was not relinquished after health check failures")
	}
}

func TestARPHealthCheckLoop_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	handler := func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}
	healthcheck := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(healthcheck.Close)

	cfg := newARPHealthCheckConfig(healthcheck.URL)
	c, err := InitCluster(cfg, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relinquished := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		c.arpHealthCheckLoop(ctx, cfg, func() { relinquished <- struct{}{} })
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("health check did not start")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("health check did not stop after context cancellation")
	}

	select {
	case <-relinquished:
		t.Fatal("health check relinquished leadership after context cancellation")
	default:
	}
}

func newARPHealthCheckConfig(url string) *kubevip.Config {
	return &kubevip.Config{
		EnableARP:          true,
		EnableControlPlane: true,
		ControlPlaneHealthCheck: kubevip.HealthCheck{
			Address:          url,
			PeriodSeconds:    1,
			TimeoutSeconds:   2,
			FailureThreshold: 1,
		},
	}
}
