package services

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// fakeWatchInterface is a minimal watch.Interface for testing.
type fakeWatchInterface struct {
	ch chan watch.Event
}

func newFakeWatchInterface() *fakeWatchInterface {
	return &fakeWatchInterface{ch: make(chan watch.Event)}
}

func (f *fakeWatchInterface) Stop()                          { close(f.ch) }
func (f *fakeWatchInterface) ResultChan() <-chan watch.Event { return f.ch }

// TestWatchWithAuthRetry_ForbiddenThenSuccess verifies that transient 403
// errors are retried and the function recovers once the watchFn succeeds.
func TestWatchWithAuthRetry_ForbiddenThenSuccess(t *testing.T) {
	fw := newFakeWatchInterface()
	var attempts atomic.Int32

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, err := watchWithAuthRetry(ctx, func(_ context.Context) (watch.Interface, error) {
		n := attempts.Add(1)
		if n <= 2 {
			return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", nil)
		}
		return fw, nil
	})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if w != fw {
		t.Fatal("expected returned watcher to be the fake watcher")
	}
	if attempts.Load() < 3 {
		t.Errorf("expected at least 3 attempts (2 failures + 1 success), got %d", attempts.Load())
	}
}

// TestWatchWithAuthRetry_UnauthorizedThenSuccess verifies that transient 401
// errors are retried the same way as 403.
func TestWatchWithAuthRetry_UnauthorizedThenSuccess(t *testing.T) {
	fw := newFakeWatchInterface()
	var attempts atomic.Int32

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, err := watchWithAuthRetry(ctx, func(_ context.Context) (watch.Interface, error) {
		n := attempts.Add(1)
		if n <= 2 {
			return nil, apierrors.NewUnauthorized("not authorized yet")
		}
		return fw, nil
	})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if w != fw {
		t.Fatal("expected returned watcher to be the fake watcher")
	}
	if attempts.Load() < 3 {
		t.Errorf("expected at least 3 attempts, got %d", attempts.Load())
	}
}

// TestWatchWithAuthRetry_NonAuthErrorFailsFast verifies that non-auth errors
// (e.g. network errors) are returned immediately without retry.
func TestWatchWithAuthRetry_NonAuthErrorFailsFast(t *testing.T) {
	var attempts atomic.Int32
	networkErr := fmt.Errorf("dial tcp 127.0.0.1:6443: connect: connection refused")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := watchWithAuthRetry(ctx, func(_ context.Context) (watch.Interface, error) {
		attempts.Add(1)
		return nil, networkErr
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attempts.Load() != 1 {
		t.Errorf("expected exactly 1 attempt for non-auth error, got %d", attempts.Load())
	}
}

// TestWatchWithAuthRetry_ContextCancelDuringRetry verifies that context
// cancellation during the retry loop causes a clean exit.
func TestWatchWithAuthRetry_ContextCancelDuringRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var attempts atomic.Int32
	go func() {
		// Cancel after the first retry attempt starts
		for attempts.Load() < 1 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()

	_, err := watchWithAuthRetry(ctx, func(_ context.Context) (watch.Interface, error) {
		attempts.Add(1)
		return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", nil)
	})
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
}

// TestWatchWithAuthRetry_ImmediateSuccess verifies the happy path where
// the first call succeeds with no retries needed.
func TestWatchWithAuthRetry_ImmediateSuccess(t *testing.T) {
	fw := newFakeWatchInterface()
	var attempts atomic.Int32

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w, err := watchWithAuthRetry(ctx, func(_ context.Context) (watch.Interface, error) {
		attempts.Add(1)
		return fw, nil
	})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if w != fw {
		t.Fatal("expected returned watcher to be the fake watcher")
	}
	if attempts.Load() != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", attempts.Load())
	}
}
