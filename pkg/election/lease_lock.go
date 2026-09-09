package election

import (
	"context"

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

func (lock *annotatedLeaseLock) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	return lock.Interface.Get(ctx)
}

func (lock *annotatedLeaseLock) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	if err := lock.Interface.Create(ctx, record); err != nil {
		return err
	}
	if record.HolderIdentity != lock.Identity() {
		return nil
	}
	changed, err := lock.ensureAnnotations(ctx)
	if err != nil {
		return err
	}
	if changed {
		_, _, err = lock.Interface.Get(ctx)
	}
	return err
}

func (lock *annotatedLeaseLock) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	if err := lock.Interface.Update(ctx, record); err != nil {
		return err
	}
	if record.HolderIdentity != lock.Identity() {
		return nil
	}
	changed, err := lock.ensureAnnotations(ctx)
	if err != nil {
		return err
	}
	if changed {
		_, _, err = lock.Interface.Get(ctx)
	}
	return err
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
