package utils

import (
	"context"
	"fmt"
	log "log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

// watchWithAuthRetry retries watchFn with exponential backoff on transient 403 Forbidden
// and 401 Unauthorized errors. On joining control plane nodes with K8s 1.34+, the local
// etcd may still be a learner when kube-vip starts, causing RBAC data to be unavailable.
// These auth errors resolve once etcd is promoted to a full member (typically within seconds).
// Non-auth errors are returned immediately. Context cancellation stops the retry loop.
func WatchWithAuthRetry(ctx context.Context, watchFn func(context.Context) (watch.Interface, error)) (watch.Interface, error) {
	var w watch.Interface
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Duration: 2 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    10,
		Cap:      30 * time.Second,
	}, func(ctx context.Context) (bool, error) {
		var watchErr error
		w, watchErr = watchFn(ctx)
		if watchErr == nil {
			return true, nil
		}
		if !apierrors.IsForbidden(watchErr) && !apierrors.IsUnauthorized(watchErr) {
			return false, watchErr
		}
		lastErr = watchErr
		log.Warn("watch auth error, retrying", "err", watchErr)
		return false, nil
	})
	if err != nil {
		return nil, NewPanicError(fmt.Sprintf("watch failed after retries: %q (last: %v)", err.Error(), lastErr))
	}
	return w, nil
}
