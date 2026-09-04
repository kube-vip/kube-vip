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
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/keymutex"
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

func TestAdmitServiceInstanceDoesNotRelockCollidingShard(t *testing.T) {
	tracked := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "tracked", Namespace: "default", UID: types.UID("tracked"),
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10", ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal}}
	candidate := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "candidate", Namespace: "default", UID: types.UID("candidate"),
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10", ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal}}
	processor := &Processor{
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{{ServiceUID: tracked.UID, ServiceSnapshot: tracked}},
		serviceLocks:     keymutex.NewHashed(1),
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := processor.admitServiceInstance(context.Background(), candidate, &sync.WaitGroup{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("admitServiceInstance() error = nil, want shared VIP policy error")
		}
	case <-time.After(time.Second):
		t.Fatal("admitServiceInstance() deadlocked on colliding Service lock shard")
	}
}

func TestAdmissionDoesNotSerializeUnrelatedInstanceConstruction(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	processor := &Processor{
		config:       &kubevip.Config{},
		serviceLocks: keymutex.NewHashed(concurrentServiceLocks),
		instanceFactory: func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
			if svc.Name == "slow" {
				close(started)
				<-release
			}
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		},
	}
	slow := admissionTestService("slow", "192.0.2.10")
	fast := admissionTestService("fast", "192.0.2.11")

	slowDone := make(chan error, 1)
	go func() {
		_, _, err := processor.admitServiceInstance(context.Background(), slow, &sync.WaitGroup{})
		slowDone <- err
	}()
	<-started

	fastDone := make(chan error, 1)
	go func() {
		_, _, err := processor.admitServiceInstance(context.Background(), fast, &sync.WaitGroup{})
		fastDone <- err
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("unrelated admission error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated admission waited for slow instance construction")
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-slowDone; err != nil {
		t.Fatalf("slow admission error = %v", err)
	}
}

func TestAdmissionRejectsConflictWithPendingConstruction(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	processor := &Processor{
		config:       &kubevip.Config{},
		serviceLocks: keymutex.NewHashed(concurrentServiceLocks),
		instanceFactory: func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
			if svc.Name == "slow" {
				close(started)
				<-release
			}
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		},
	}
	slow := admissionTestService("slow", "192.0.2.10")
	conflicting := admissionTestService("conflicting", "192.0.2.10")

	slowDone := make(chan error, 1)
	go func() {
		_, _, err := processor.admitServiceInstance(context.Background(), slow, &sync.WaitGroup{})
		slowDone <- err
	}()
	<-started

	conflictDone := make(chan error, 1)
	go func() {
		_, _, err := processor.admitServiceInstance(context.Background(), conflicting, &sync.WaitGroup{})
		conflictDone <- err
	}()
	select {
	case err := <-conflictDone:
		if err == nil {
			t.Fatal("conflicting admission ignored the pending VIP reservation")
		}
	case <-time.After(time.Second):
		t.Fatal("conflicting admission waited for pending construction")
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-slowDone; err != nil {
		t.Fatalf("slow admission error = %v", err)
	}
}

func TestAdmissionReleasesReservationAfterConstructionFailure(t *testing.T) {
	processor := &Processor{
		config:       &kubevip.Config{},
		serviceLocks: keymutex.NewHashed(concurrentServiceLocks),
		instanceFactory: func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
			if svc.Name == "failed" {
				return nil, errors.New("construction failed")
			}
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		},
	}
	failed := admissionTestService("failed", "192.0.2.10")
	replacement := admissionTestService("replacement", "192.0.2.10")

	if _, _, err := processor.admitServiceInstance(context.Background(), failed, &sync.WaitGroup{}); err == nil {
		t.Fatal("failed construction returned no error")
	}
	if _, added, err := processor.admitServiceInstance(context.Background(), replacement, &sync.WaitGroup{}); err != nil {
		t.Fatalf("replacement admission error = %v", err)
	} else if !added {
		t.Fatal("replacement was not added after failed reservation cleanup")
	}
}

func TestAdmissionDoesNotWaitForTrackedServiceLock(t *testing.T) {
	processor := &Processor{config: &kubevip.Config{}}
	trackedService := admissionTestService("tracked", "192.0.2.10")
	processor.ServiceInstances = []*instance.Instance{{
		ServiceUID:       trackedService.UID,
		ServiceAddresses: []string{"192.0.2.10"},
		ServiceSnapshot:  trackedService,
	}}

	unlockTracked := processor.lockService(trackedService.UID)
	defer unlockTracked()
	admitted := make(chan error, 1)
	go func() {
		release, err := processor.reserveServiceAdmission(admissionTestService("other", "192.0.2.20"))
		if err == nil {
			release()
		}
		admitted <- err
	}()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("unrelated admission returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated admission waited for the tracked Service lock")
	}
}

func admissionTestService(name, address string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
		Spec: v1.ServiceSpec{
			LoadBalancerIP:        address,
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
		},
	}
}

func TestReconcileAndDeleteRejectTypedNilService(t *testing.T) {
	var service *v1.Service
	event := watch.Event{Object: service}
	processor := &Processor{}

	if err := processor.Reconcile(context.Background(), event, nil, false, nil, nil); err == nil {
		t.Fatal("Reconcile() accepted a typed-nil Service")
	}
	if err := processor.Delete(event, false); err == nil {
		t.Fatal("Delete() accepted a typed-nil Service")
	}
}

func TestServiceChangedHandlesNilIPFamilyPolicy(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: "service"}}
	instance := &instance.Instance{ServiceSnapshot: service.DeepCopy()}
	if serviceChanged(instance, service) {
		t.Fatal("identical Services with nil IP family policies were considered changed")
	}
	policy := v1.IPFamilyPolicySingleStack
	service.Spec.IPFamilyPolicy = &policy
	if !serviceChanged(instance, service) {
		t.Fatal("Service IP family policy change was not detected")
	}
}

func TestServiceVIPSharingRequiresClusterPolicyAndElectionLease(t *testing.T) {
	sharedVIP := "192.0.2.10"
	tracked := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "tracked", Namespace: "default", UID: types.UID("tracked"),
	}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP}}
	processor := &Processor{
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{{ServiceUID: tracked.UID, ServiceSnapshot: tracked}},
	}

	tests := []struct {
		name                   string
		service                *v1.Service
		wantErr                bool
		annotations            map[string]string
		enableServicesElection bool
		trackedTrafficPolicy   v1.ServiceExternalTrafficPolicy
	}{
		{
			name: "cluster policy without service election",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
			}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}},
		},
		{
			name: "common lease without service election",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
				Annotations: map[string]string{kubevip.ServiceLease: "shared"},
			}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}},
			annotations: map[string]string{kubevip.ServiceLease: "shared"},
		},
		{
			name: "common lease with service election",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
				Annotations: map[string]string{kubevip.ServiceLease: "shared"},
			}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}},
			annotations:            map[string]string{kubevip.ServiceLease: "shared"},
			enableServicesElection: true,
		},
		{
			name: "different lease with service election",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
				Annotations: map[string]string{kubevip.ServiceLease: "different"},
			}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}},
			annotations:            map[string]string{kubevip.ServiceLease: "shared"},
			enableServicesElection: true,
			wantErr:                true,
		},
		{
			name: "local traffic policy without service election",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
			}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal}},
			wantErr: true,
		},
		{
			name: "candidate local and tracked cluster without service election",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
			}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal}},
			trackedTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
			wantErr:              true,
		},
		{
			name: "candidate cluster and tracked local without service election",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
			}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}},
			trackedTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
			wantErr:              true,
		},
		{
			name: "common lease with local traffic policy",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
				Annotations: map[string]string{kubevip.ServiceLease: "shared"},
			}, Spec: v1.ServiceSpec{LoadBalancerIP: sharedVIP, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal}},
			annotations:            map[string]string{kubevip.ServiceLease: "shared"},
			enableServicesElection: true,
			wantErr:                true,
		},
		{
			name: "different VIP",
			service: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "default", UID: types.UID("other"),
			}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.11"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracked.Annotations = test.annotations
			tracked.Spec.ExternalTrafficPolicy = test.trackedTrafficPolicy
			if tracked.Spec.ExternalTrafficPolicy == "" {
				tracked.Spec.ExternalTrafficPolicy = test.service.Spec.ExternalTrafficPolicy
			}
			processor.config.EnableServicesElection = test.enableServicesElection
			err := processor.validateServiceVIPLease(test.service)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateServiceVIPLease() error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func TestDeleteServiceCleansUpAfterContextRemoval(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid, Name: "service-a", Namespace: "default"}}
	processor := &Processor{
		config:           &kubevip.Config{EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{{ServiceUID: uid, ServiceSnapshot: service}},
	}

	if err := processor.deleteService(context.Background(), uid, servicecontext.New(context.Background())); err != nil {
		t.Fatalf("deleteService() error = %v", err)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("deleted Service instance remained tracked after leader cleanup")
	}
}

func TestRetireServiceContextCancelsBeforeServiceLockIsAvailable(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid, Name: "service-a", Namespace: "default"}}
	svcCtx := servicecontext.New(context.Background())
	processor := &Processor{config: &kubevip.Config{}}
	processor.svcMap.Store(uid, svcCtx)

	unlockService := processor.lockService(uid)
	done := make(chan error, 1)
	go func() {
		_, _, err := processor.retireServiceContext(service)
		done <- err
	}()

	select {
	case <-svcCtx.Ctx.Done():
	case <-time.After(time.Second):
		unlockService()
		t.Fatal("retireServiceContext waited for the Service lock before cancelling")
	}
	unlockService()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("retireServiceContext() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retireServiceContext did not finish after the Service lock was released")
	}
}

func TestDeleteServiceIsIdempotentWhenInstanceIsMissing(t *testing.T) {
	processor := &Processor{}
	if err := processor.deleteService(context.Background(), types.UID("missing-service")); err != nil {
		t.Fatalf("deleteService() error = %v, want nil", err)
	}
}

func TestAddServiceMarksPreTrackedInstanceAdded(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	serviceInstance := &instance.Instance{ServiceUID: uid, ServiceSnapshot: service}
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

func TestPrepareServiceInstanceRejectsCancelledContext(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: types.UID("cancelled-service"), Name: "cancelled-service", Namespace: "default",
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	processor := &Processor{config: &kubevip.Config{}}

	created, err := processor.prepareServiceInstance(ctx, service, &sync.WaitGroup{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prepareServiceInstance() error = %v, want context cancellation", err)
	}
	if created != nil {
		t.Fatal("prepareServiceInstance() returned an instance for a cancelled context")
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("cancelled Service context left a tracked instance")
	}
}

func TestPrepareServiceInstanceUsesSharedFactory(t *testing.T) {
	service := admissionTestService("service", "192.0.2.10")
	called := false
	processor := &Processor{
		config: &kubevip.Config{},
		instanceFactory: func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
			called = true
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		},
	}

	created, err := processor.prepareServiceInstance(context.Background(), service, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("prepareServiceInstance() error = %v", err)
	}
	if !called {
		t.Fatal("prepareServiceInstance() bypassed the shared instance factory")
	}
	if created == nil || !created.AddCalled {
		t.Fatal("prepareServiceInstance() did not return an added instance")
	}
}

func TestStopMarksServiceInstanceForReconfiguration(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	processor := &Processor{ServiceInstances: []*instance.Instance{{
		ServiceUID:      uid,
		ServiceSnapshot: service,
		AddCalled:       true,
	}}}
	svcCtx := servicecontext.New(context.Background())
	processor.svcMap.Store(uid, svcCtx)

	processor.Stop()
	if svcCtx.Ctx.Err() == nil {
		t.Fatal("Stop did not cancel the Service context")
	}
	if svcCtx.StartWatching() {
		t.Fatal("stopped Service context reacquired watcher ownership")
	}
	action := processor.getServiceInstanceAction(service)
	if action != ActionAdd {
		t.Fatalf("action after Stop() = %q, want %q", action, ActionAdd)
	}
}

func TestAddServiceAfterDeleteTracksOneFreshInstance(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	processor := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{{ServiceUID: uid, ServiceSnapshot: service}},
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
	serviceInstance := &instance.Instance{ServiceUID: uid, ServiceSnapshot: service}
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
	serviceInstance := &instance.Instance{ServiceUID: uid, ServiceSnapshot: service, LabelAdded: true}
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
	failedInstance := &instance.Instance{ServiceUID: uid, ServiceSnapshot: service}
	replacement := &instance.Instance{ServiceUID: uid, ServiceSnapshot: service.DeepCopy()}
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
		ServiceInstances: []*instance.Instance{{ServiceUID: uid, ServiceSnapshot: service}},
		leaseMgr:         lease.NewManager(),
	}
	svcCtx := servicecontext.New(context.Background())
	processor.svcMap.Store(uid, svcCtx)
	leaseNamespace, serviceLease := lease.ServiceName(service)
	leaseID := lease.NewID(processor.config.LeaderElectionType, leaseNamespace, serviceLease)
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	if _, joined := processor.joinServiceElection(svcCtx, service, generation); !joined {
		t.Fatal("service did not join its election")
	}

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
		ServiceInstances: []*instance.Instance{{ServiceUID: uid, ServiceSnapshot: service, LabelAdded: true}},
		nodeLabelManager: labeler,
		leaseMgr:         lease.NewManager(),
	}
	svcCtx := servicecontext.New(context.Background())
	processor.svcMap.Store(uid, svcCtx)
	leaseNamespace, serviceLease := lease.ServiceName(service)
	leaseID := lease.NewID(processor.config.LeaderElectionType, leaseNamespace, serviceLease)
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	if _, joined := processor.joinServiceElection(svcCtx, service, generation); !joined {
		t.Fatal("service did not join its election")
	}

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
