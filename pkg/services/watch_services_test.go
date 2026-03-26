package services

import (
	"context"
	"fmt"
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

func TestWatchWithAuthRetry(t *testing.T) {
	svcResource := schema.GroupResource{Resource: "services"}
	fw := newFakeWatchInterface()

	tcs := []struct {
		name         string
		watchFn      func(int) (watch.Interface, error)
		wantErr      bool
		wantAttempts int
	}{
		{
			name: "403 Forbidden retried then succeeds",
			watchFn: func(attempt int) (watch.Interface, error) {
				if attempt <= 2 {
					return nil, apierrors.NewForbidden(svcResource, "", nil)
				}
				return fw, nil
			},
			wantAttempts: 3,
		},
		{
			name: "401 Unauthorized retried then succeeds",
			watchFn: func(attempt int) (watch.Interface, error) {
				if attempt <= 2 {
					return nil, apierrors.NewUnauthorized("not authorized yet")
				}
				return fw, nil
			},
			wantAttempts: 3,
		},
		{
			name: "non-auth error fails immediately",
			watchFn: func(_ int) (watch.Interface, error) {
				return nil, fmt.Errorf("connection refused")
			},
			wantErr:      true,
			wantAttempts: 1,
		},
		{
			name: "immediate success no retry",
			watchFn: func(_ int) (watch.Interface, error) {
				return fw, nil
			},
			wantAttempts: 1,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			w, err := watchWithAuthRetry(ctx, func(_ context.Context) (watch.Interface, error) {
				attempts++
				return tc.watchFn(attempts)
			})

			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected success, got: %v", err)
			}
			if !tc.wantErr && w != fw {
				t.Fatal("returned watcher does not match expected")
			}
			if attempts != tc.wantAttempts {
				t.Errorf("expected %d attempts, got %d", tc.wantAttempts, attempts)
			}
		})
	}
}
