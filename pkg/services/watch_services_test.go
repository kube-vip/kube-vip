package services

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newTestProcessor creates a minimal Processor for testing ServicesWatcher.
func newTestProcessor(clientset *fake.Clientset) *Processor {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_service_watch_events",
		Help: "test",
	}, []string{"type"})
	reg.MustRegister(counter)
	return &Processor{
		config: &kubevip.Config{
			ServiceNamespace: v1.NamespaceAll,
		},
		lbClassFilter:          lbClassFilter,
		rwClientSet:            clientset,
		CountServiceWatchEvent: counter,
	}
}

// noopServiceFunc is a no-op service function for use in tests.
func noopServiceFunc(_ *servicecontext.Context, _ *v1.Service, _ *sync.WaitGroup) error {
	return nil
}

// TestServicesWatcher_AuthErrorRetrySucceeds verifies that transient 403/401
// auth errors are retried and the watcher recovers once RBAC becomes available.
func TestServicesWatcher_AuthErrorRetrySucceeds(t *testing.T) {
	tcs := []struct {
		name    string
		makeErr func() error
	}{
		{
			name: "403 Forbidden retried",
			makeErr: func() error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", nil)
			},
		},
		{
			name: "401 Unauthorized retried",
			makeErr: func() error {
				return apierrors.NewUnauthorized("not authorized yet")
			},
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			fakeWatcher := watch.NewFake()

			var attempts atomic.Int32
			clientset.PrependWatchReactor("services", func(action k8stesting.Action) (bool, watch.Interface, error) {
				n := attempts.Add(1)
				if n <= 2 {
					return true, nil, tc.makeErr()
				}
				return true, fakeWatcher, nil
			})

			p := newTestProcessor(clientset)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			done := make(chan error, 1)
			go func() {
				done <- p.ServicesWatcher(ctx, noopServiceFunc)
			}()

			// Wait long enough for 2 retries to complete.
			// Initial backoff: 2s * (1+jitter), second: 4s * (1+jitter).
			// With jitter=0.1, worst case is 2.2s + 4.4s = 6.6s, so 15s is safe.
			time.Sleep(15 * time.Second)

			// Close the watcher to let ServicesWatcher exit cleanly
			fakeWatcher.Stop()
			cancel()

			select {
			case err := <-done:
				if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
					t.Errorf("unexpected error: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("ServicesWatcher did not exit in time")
			}

			if attempts.Load() < 3 {
				t.Errorf("expected at least 3 Watch attempts (2 auth errors + 1 success), got %d", attempts.Load())
			}
		})
	}
}

// TestServicesWatcher_ContextCancelDuring403Retry verifies that context cancellation
// during a 403 retry loop causes ServicesWatcher to exit cleanly.
func TestServicesWatcher_ContextCancelDuring403Retry(t *testing.T) {
	clientset := fake.NewClientset()

	clientset.PrependWatchReactor("services", func(action k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "services"}, "", nil)
	})

	p := newTestProcessor(clientset)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.ServicesWatcher(ctx, noopServiceFunc)
	}()

	// Cancel context after first retry starts
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Logf("ServicesWatcher returned: %v (acceptable)", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("ServicesWatcher did not exit after context cancellation")
	}
}
