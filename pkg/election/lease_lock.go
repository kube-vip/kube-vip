package election

import (
	"context"

	log "log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
)

type annotatedLeaseLock struct {
	resourcelock.Interface
	leases      coordinationv1client.LeaseInterface
	name        string
	annotations map[string]string
}

func newAnnotatedLeaseLock(lock resourcelock.Interface, leases coordinationv1client.LeaseInterface,
	name string, annotations map[string]string) resourcelock.Interface {
	return &annotatedLeaseLock{Interface: lock, leases: leases, name: name, annotations: annotations}
}

func (lock *annotatedLeaseLock) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	if err := lock.Interface.Create(ctx, record); err != nil {
		return err
	}
	lock.ensure(ctx, record)
	return nil
}

func (lock *annotatedLeaseLock) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	if err := lock.Interface.Update(ctx, record); err != nil {
		return err
	}
	lock.ensure(ctx, record)
	return nil
}

// ensure applies the configured annotations once this process holds the lease. Failures are
// logged rather than returned: the lease write already succeeded, so reporting an error would
// make the elector stand down while it still holds the lease.
func (lock *annotatedLeaseLock) ensure(ctx context.Context, record resourcelock.LeaderElectionRecord) {
	if record.HolderIdentity != lock.Identity() {
		return
	}
	changed, err := lock.ensureAnnotations(ctx)
	if err != nil {
		log.Warn("failed to annotate lease", "lease", lock.name, "err", err)
		return
	}
	if !changed {
		return
	}
	// Annotating out of band bumps the resourceVersion, so refresh the wrapped lock's
	// cached lease or its next optimistic Update conflicts.
	if _, _, err := lock.Interface.Get(ctx); err != nil {
		log.Warn("failed to refresh lease after annotating", "lease", lock.name, "err", err)
	}
}

func (lock *annotatedLeaseLock) ensureAnnotations(ctx context.Context) (bool, error) {
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		resource, err := lock.leases.Get(ctx, lock.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if resource.Annotations == nil {
			resource.Annotations = make(map[string]string, len(lock.annotations))
		}
		resourceChanged := false
		for key, value := range lock.annotations {
			if resource.Annotations[key] == value {
				continue
			}
			resource.Annotations[key] = value
			resourceChanged = true
		}
		if !resourceChanged {
			return nil
		}
		_, err = lock.leases.Update(ctx, resource, metav1.UpdateOptions{})
		if err == nil {
			changed = true
		}
		return err
	})
	return changed, err
}
