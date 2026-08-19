package utils

import (
	"context"
	"fmt"
	log "log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

// WatchError converts a Kubernetes watch error object into a safe Go error.
func WatchError(object runtime.Object) error {
	errObject := apierrors.FromObject(object)
	if statusErr, ok := errObject.(*apierrors.StatusError); ok {
		return statusErr
	}
	return fmt.Errorf("unknown watch error object of type %T: %v", object, object)
}

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
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if lastErr != nil {
			log.Error("watch auth retries exhausted", "err", lastErr)
			return nil, lastErr
		}
		return nil, WrapPanicError(err, "watch failed after retries (last: %v)", lastErr)
	}
	return w, nil
}
