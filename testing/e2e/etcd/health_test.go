//go:build e2e
// +build e2e

package etcd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitForEtcdHealth(t *testing.T) {
	tests := []struct {
		name    string
		context func() context.Context
		check   func(context.Context) error
		wantErr string
	}{
		{
			name:    "retries transient errors",
			context: context.Background,
			check: func() func(context.Context) error {
				attempts := 0
				return func(context.Context) error {
					attempts++
					if attempts < 3 {
						return errors.New("not ready")
					}
					return nil
				}
			}(),
		},
		{
			name: "preserves parent cancellation",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			check:   func(context.Context) error { return errors.New("probe failed") },
			wantErr: "context canceled",
		},
		{
			name: "does not probe after parent cancellation",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			check: func(context.Context) error {
				t.Fatal("check called with a canceled parent context")
				return nil
			},
			wantErr: "context canceled",
		},
		{
			name:    "reports last probe on timeout",
			context: context.Background,
			check:   func(context.Context) error { return errors.New("member is still a learner") },
			wantErr: "last probe: member is still a learner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := waitForEtcdHealth(tt.context(), 20*time.Millisecond, time.Millisecond, tt.check)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("waitForEtcdHealth() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("waitForEtcdHealth() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
