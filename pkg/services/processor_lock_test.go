package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type testLabeler struct {
	addErr       error
	removeErr    error
	removeErrors []error
	removeCalls  int
}

func (l *testLabeler) AddLabel(map[string]string) error {
	return l.addErr
}

func (l *testLabeler) RemoveLabel(map[string]string) error {
	l.removeCalls++
	if len(l.removeErrors) > 0 {
		err := l.removeErrors[0]
		l.removeErrors = l.removeErrors[1:]
		return err
	}
	return l.removeErr
}

func TestServiceLocksAreScopedByUID(t *testing.T) {
	processor := &Processor{}

	t.Run("different Services proceed concurrently", func(t *testing.T) {
		unlockFirst := processor.lockService(types.UID("service-a"))
		acquired := make(chan struct{})
		go func() {
			unlockSecond := processor.lockService(types.UID("service-b"))
			close(acquired)
			unlockSecond()
		}()

		select {
		case <-acquired:
		case <-time.After(time.Second):
			t.Fatal("different Service UID was blocked by another Service lock")
		}
		unlockFirst()
	})

	t.Run("same Service remains serialized", func(t *testing.T) {
		uid := types.UID("service-a")
		unlockFirst := processor.lockService(uid)
		acquired := make(chan struct{})
		go func() {
			unlockSecond := processor.lockService(uid)
			close(acquired)
			unlockSecond()
		}()

		select {
		case <-acquired:
			t.Fatal("same Service UID acquired the lock concurrently")
		case <-time.After(50 * time.Millisecond):
		}
		unlockFirst()

		select {
		case <-acquired:
		case <-time.After(time.Second):
			t.Fatal("same Service UID remained blocked after unlock")
		}
	})
}

func TestDeleteServiceCleansUpAfterContextRemoval(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid, Name: "service-a", Namespace: "default"}}
	processor := &Processor{
		config:           &kubevip.Config{EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{{ServiceSnapshot: service}},
	}

	if err := processor.deleteService(context.Background(), uid, servicecontext.New(context.Background())); err != nil {
		t.Fatalf("deleteService() error = %v", err)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("deleted Service instance remained tracked after leader cleanup")
	}
}

func TestAddServiceMarksPreTrackedInstanceAdded(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	serviceInstance := &instance.Instance{ServiceSnapshot: service}
	processor := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{serviceInstance},
		nodeLabelManager: &testLabeler{},
	}

	if err := processor.addService(context.Background(), serviceInstance, service, &sync.WaitGroup{}); err != nil {
		t.Fatalf("addService() error = %v", err)
	}
	if !serviceInstance.AddCalled {
		t.Fatal("pre-tracked Service instance was not marked added")
	}
}

func TestDeleteServiceKeepsInstanceWhenLabelRemovalFails(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	serviceInstance := &instance.Instance{ServiceSnapshot: service, LabelAdded: true}
	processor := &Processor{
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{serviceInstance},
		nodeLabelManager: &testLabeler{removeErr: errors.New("remove label")},
	}

	if err := processor.deleteService(context.Background(), uid); err == nil {
		t.Fatal("deleteService() error = nil, want label removal error")
	}
	if got := processor.findServiceInstance(service); got != serviceInstance {
		t.Fatal("failed deletion removed the Service instance, preventing cleanup retry")
	}
}

func TestDeleteTrackedServiceRetriesTransientCleanupFailure(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	labeler := &testLabeler{removeErrors: []error{errors.New("temporary remove label"), nil}}
	processor := &Processor{
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{{ServiceSnapshot: service, LabelAdded: true}},
		nodeLabelManager: labeler,
	}
	processor.svcMap.Store(uid, servicecontext.New(context.Background()))

	if err := processor.deleteTrackedService(service); err != nil {
		t.Fatalf("deleteTrackedService() error = %v", err)
	}
	if labeler.removeCalls != 2 {
		t.Fatalf("RemoveLabel calls = %d, want 2", labeler.removeCalls)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("transient cleanup failure left Service instance tracked after retry")
	}
}

func TestDeleteTrackedServiceReturnsPersistentCleanupFailure(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	processor := &Processor{
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{{ServiceSnapshot: service, LabelAdded: true}},
		nodeLabelManager: &testLabeler{removeErr: errors.New("permanent remove label")},
	}
	processor.svcMap.Store(uid, servicecontext.New(context.Background()))

	if err := processor.deleteTrackedService(service); err == nil {
		t.Fatal("deleteTrackedService() error = nil, want cleanup failure")
	}
	if got := processor.findServiceInstance(service); got == nil {
		t.Fatal("persistent cleanup failure removed the Service instance")
	}
}

func TestServiceSnapshotsCopiesMutableServiceState(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	processor := &Processor{ServiceInstances: []*instance.Instance{{
		ServiceUID:      uid,
		ServiceSnapshot: service,
	}}}

	snapshots := processor.ServiceSnapshots()
	if len(snapshots) != 1 {
		t.Fatalf("ServiceSnapshots() count = %d, want 1", len(snapshots))
	}
	service.Namespace = "changed"
	if snapshots[0].Namespace != "default" {
		t.Fatal("ServiceSnapshots() returned mutable Service state")
	}
}

func TestServiceSnapshotsSerializesSnapshotReplacement(t *testing.T) {
	uid := types.UID("service-a")
	serviceInstance := &instance.Instance{
		ServiceUID: uid,
		ServiceSnapshot: &v1.Service{ObjectMeta: metav1.ObjectMeta{
			UID: uid, Name: "service-a", Namespace: "default",
		}},
	}
	processor := &Processor{ServiceInstances: []*instance.Instance{serviceInstance}}

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			unlockService := processor.lockService(uid)
			serviceInstance.ServiceSnapshot = &v1.Service{ObjectMeta: metav1.ObjectMeta{
				UID: uid, Name: "service-a", Namespace: "default",
			}}
			unlockService()
		}
	})
	wg.Go(func() {
		for range 100 {
			snapshots := processor.ServiceSnapshots()
			if len(snapshots) != 1 || snapshots[0].UID != uid {
				t.Errorf("ServiceSnapshots() = %+v, want one snapshot for %q", snapshots, uid)
				return
			}
		}
	})
	wg.Wait()
}
