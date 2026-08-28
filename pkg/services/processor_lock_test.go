package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type testLabeler struct {
	addErr    error
	addErrors []error
	addCalls  int
	removeErr error
}

func (l *testLabeler) AddLabel(map[string]string) error {
	l.addCalls++
	if len(l.addErrors) > 0 {
		err := l.addErrors[0]
		l.addErrors = l.addErrors[1:]
		return err
	}
	return l.addErr
}

func (l *testLabeler) RemoveLabel(map[string]string) error {
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

	if err := processor.addService(context.Background(), service, &sync.WaitGroup{}); err != nil {
		t.Fatalf("addService() error = %v", err)
	}
	if !serviceInstance.AddCalled {
		t.Fatal("pre-tracked Service instance was not marked added")
	}
	if err := processor.addService(context.Background(), service, &sync.WaitGroup{}); err != nil {
		t.Fatalf("second addService() error = %v", err)
	}
}

func TestStopMarksServiceInstanceForReconfiguration(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	processor := &Processor{ServiceInstances: []*instance.Instance{{
		ServiceSnapshot: service,
		AddCalled:       true,
	}}}

	processor.Stop()
	action := processor.getServiceInstanceAction(service)
	if action != ActionAdd {
		t.Fatalf("action after Stop() = %q, want %q", action, ActionAdd)
	}
}

func TestAddServiceDoesNotReattachDetachedInstance(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	staleInstance := &instance.Instance{ServiceSnapshot: service}
	processor := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{staleInstance},
		nodeLabelManager: &testLabeler{},
	}

	action := processor.getServiceInstanceAction(service)
	if action != ActionAdd {
		t.Fatalf("getServiceInstanceAction() = %q, want ActionAdd", action)
	}
	if err := processor.deleteService(context.Background(), uid); err != nil {
		t.Fatalf("deleteService() error = %v", err)
	}
	if err := processor.addService(context.Background(), service, &sync.WaitGroup{}); err != nil {
		t.Fatalf("addService() error = %v", err)
	}
	current := processor.findServiceInstance(service)
	if current == nil {
		t.Fatal("addService() did not track a replacement instance")
	}
	if current == staleInstance {
		t.Fatal("addService() reattached an instance detached by concurrent deletion")
	}
	if got := len(processor.ServiceInstances); got != 1 {
		t.Fatalf("tracked instance count = %d, want 1", got)
	}
	if !current.AddCalled {
		t.Fatal("replacement instance was not marked added")
	}
}

func TestAddServiceCleansUpAfterConfigurationFailure(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	serviceInstance := &instance.Instance{ServiceSnapshot: service}
	labeler := &testLabeler{addErr: errors.New("add label")}
	processor := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{serviceInstance},
		nodeLabelManager: labeler,
	}

	if err := processor.addService(context.Background(), service, &sync.WaitGroup{}); err == nil {
		t.Fatal("addService() error = nil, want configuration failure")
	}
	if labeler.addCalls != 1 {
		t.Fatalf("AddLabel calls = %d, want 1", labeler.addCalls)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("configuration failure left a partial instance tracked")
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

func TestDeleteServiceInstanceDoesNotDeleteReplacement(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	failedInstance := &instance.Instance{ServiceSnapshot: service}
	replacement := &instance.Instance{ServiceSnapshot: service.DeepCopy()}
	processor := &Processor{
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{replacement},
		nodeLabelManager: &testLabeler{},
	}

	if err := processor.deleteServiceInstance(context.Background(), failedInstance); err != nil {
		t.Fatalf("deleteServiceInstance() error = %v", err)
	}
	if got := processor.findServiceInstance(service); got != replacement {
		t.Fatal("failed-add cleanup removed a replacement instance")
	}
}

func TestDeleteTrackedServiceCleansUpElectedServiceImmediately(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	processor := &Processor{
		config:           &kubevip.Config{EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{{ServiceSnapshot: service}},
		leaseMgr:         lease.NewManager(),
	}
	svcCtx := servicecontext.New(context.Background())
	processor.svcMap.Store(uid, svcCtx)
	leaseNamespace, serviceLease := lease.ServiceName(service)
	leaseID := lease.NewID(processor.config.LeaderElectionType, leaseNamespace, serviceLease)
	processor.leaseMgr.Add(context.Background(), leaseID).Add(lease.ServiceClaimID(service))

	if err := processor.deleteTrackedService(service); err != nil {
		t.Fatalf("deleteTrackedService() error = %v", err)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("deleted elected Service instance remained tracked")
	}
	if _, ok := processor.svcMap.Load(uid); ok {
		t.Fatal("deleted Service context remained tracked after cleanup")
	}
	if processor.leaseMgr.Get(leaseID) != nil {
		t.Fatal("deleted Service lease remained available for a replacement")
	}
}

func TestDeleteTrackedServiceReturnsPersistentCleanupFailure(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	labeler := &testLabeler{removeErr: errors.New("permanent remove label")}
	processor := &Processor{
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{{ServiceSnapshot: service, LabelAdded: true}},
		nodeLabelManager: labeler,
		leaseMgr:         lease.NewManager(),
	}
	svcCtx := servicecontext.New(context.Background())
	processor.svcMap.Store(uid, svcCtx)
	leaseNamespace, serviceLease := lease.ServiceName(service)
	leaseID := lease.NewID(processor.config.LeaderElectionType, leaseNamespace, serviceLease)
	processor.leaseMgr.Add(context.Background(), leaseID).Add(lease.ServiceClaimID(service))

	if err := processor.deleteTrackedService(service); err == nil {
		t.Fatal("deleteTrackedService() error = nil, want cleanup failure")
	}
	if got := processor.findServiceInstance(service); got == nil {
		t.Fatal("persistent cleanup failure removed the Service instance")
	}
	if got, err := processor.getServiceContext(uid); err != nil || got != svcCtx {
		t.Fatalf("failed cleanup did not retain its context for retry: got %v, err %v", got, err)
	}
	if processor.leaseMgr.Get(leaseID) != nil {
		t.Fatal("failed cleanup left the retired lease available to a replacement")
	}

	labeler.removeErr = nil
	if err := processor.deleteTrackedService(service); err != nil {
		t.Fatalf("deleteTrackedService() retry error = %v", err)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("retry did not remove the Service instance")
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
