package services

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
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

func TestServiceLockIsScopedByUID(t *testing.T) {
	serviceLock := NewServiceLock()

	t.Run("different Services proceed concurrently", func(t *testing.T) {
		serviceLock.Lock(types.UID("service-a"))
		acquired := make(chan struct{})
		go func() {
			serviceLock.Lock(types.UID("service-b"))
			close(acquired)
			if err := serviceLock.Unlock(types.UID("service-b")); err != nil {
				t.Errorf("Unlock() error = %v", err)
			}
		}()

		select {
		case <-acquired:
		case <-time.After(time.Second):
			t.Fatal("different Service UID was blocked by another Service lock")
		}
		if err := serviceLock.Unlock(types.UID("service-a")); err != nil {
			t.Fatalf("Unlock() error = %v", err)
		}
	})

	t.Run("same Service remains serialized", func(t *testing.T) {
		uid := types.UID("service-a")
		serviceLock.Lock(uid)
		acquired := make(chan struct{})
		go func() {
			serviceLock.Lock(uid)
			close(acquired)
			if err := serviceLock.Unlock(uid); err != nil {
				t.Errorf("Unlock() error = %v", err)
			}
		}()

		select {
		case <-acquired:
			t.Fatal("same Service UID acquired the lock concurrently")
		case <-time.After(50 * time.Millisecond):
		}
		if err := serviceLock.Unlock(uid); err != nil {
			t.Fatalf("Unlock() error = %v", err)
		}

		select {
		case <-acquired:
		case <-time.After(time.Second):
			t.Fatal("same Service UID remained blocked after unlock")
		}
	})
}

type failingKeyMutex struct {
	err error
}

func (f failingKeyMutex) LockKey(string)         {}
func (f failingKeyMutex) UnlockKey(string) error { return f.err }

type lifecycleBlockingKeyMutex struct {
	mutex            sync.Mutex
	lockCalls        int
	configureLock    chan struct{}
	releaseConfigure chan struct{}
	cleanupLock      chan struct{}
	releaseCleanup   chan struct{}
}

func (m *lifecycleBlockingKeyMutex) LockKey(string) {
	m.mutex.Lock()
	m.lockCalls++
	lockCall := m.lockCalls
	m.mutex.Unlock()
	switch lockCall {
	case 2:
		close(m.configureLock)
		<-m.releaseConfigure
	case 3:
		if m.cleanupLock != nil {
			close(m.cleanupLock)
			<-m.releaseCleanup
		}
	}
}

func (m *lifecycleBlockingKeyMutex) UnlockKey(string) error { return nil }

func TestServiceLockReturnsUnlockError(t *testing.T) {
	want := errors.New("unlock failed")
	serviceLock := &ServiceLock{mutex: failingKeyMutex{err: want}}
	err := serviceLock.Unlock(types.UID("service"))
	if !errors.Is(err, want) {
		t.Fatalf("Unlock() error = %v, want %v", err, want)
	}
	if got, want := err.Error(), `unlock service "service": unlock failed`; got != want {
		t.Fatalf("Unlock() error = %q, want %q", got, want)
	}
}

func TestServiceContextCurrentLocked(t *testing.T) {
	uid := types.UID("service-a")
	active := servicecontext.New(context.Background())
	replacement := servicecontext.New(context.Background())
	cancelled := servicecontext.New(context.Background())
	cancelled.Cancel()

	tests := []struct {
		name      string
		published any
		expected  *servicecontext.Context
		want      bool
		wantErr   bool
	}{
		{name: "current active context", published: active, expected: active, want: true},
		{name: "different context", published: replacement, expected: active},
		{name: "cancelled context", published: cancelled, expected: cancelled},
		{name: "missing context", expected: active},
		{name: "nil expected context", published: active},
		{name: "invalid stored value", published: "not a service context", expected: active, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &Processor{serviceLock: newTestServiceLocks()}
			if tt.published != nil {
				processor.svcMap.Store(uid, tt.published)
			}

			processor.serviceLock.Lock(uid)
			got, err := processor.serviceContextCurrentLocked(uid, tt.expected)
			if unlockErr := processor.serviceLock.Unlock(uid); unlockErr != nil {
				t.Fatalf("Unlock() error = %v", unlockErr)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("serviceContextCurrentLocked() error = %v, wantErr %t", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("serviceContextCurrentLocked() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestAdmissionDoesNotSerializeUnrelatedInstanceConstruction(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	processor := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
			if svc.Name == "slow" {
				close(started)
				<-release
			}
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		}),
	}
	initializeTestElectionCoordinators(processor)
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
	processor := &Processor{serviceLock: newTestServiceLocks()}
	initializeTestElectionCoordinators(processor)

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

func TestDeleteServiceCleansUpAfterContextRemoval(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid, Name: "service-a", Namespace: "default"}}
	processor := &Processor{
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{{ServiceUID: uid, ServiceSnapshot: service}},
	}
	initializeTestElectionCoordinators(processor)

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
	processor := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}}
	initializeTestElectionCoordinators(processor)
	processor.svcMap.Store(uid, svcCtx)

	processor.serviceLock.Lock(uid)
	done := make(chan error, 1)
	go func() {
		_, _, err := processor.retireServiceContext(service)
		done <- err
	}()

	select {
	case <-svcCtx.Ctx.Done():
	case <-time.After(time.Second):
		if err := processor.serviceLock.Unlock(uid); err != nil {
			t.Fatalf("Unlock() error = %v", err)
		}
		t.Fatal("retireServiceContext waited for the Service lock before cancelling")
	}
	if err := processor.serviceLock.Unlock(uid); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}

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
	processor := &Processor{serviceLock: newTestServiceLocks()}
	initializeTestElectionCoordinators(processor)
	if err := processor.deleteService(context.Background(), types.UID("missing-service"), nil); err != nil {
		t.Fatalf("deleteService() error = %v, want nil", err)
	}
}

func TestDeleteServiceForContextRejectsStaleServiceContext(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	serviceInstance := &instance.Instance{ServiceUID: uid, ServiceSnapshot: service}
	processor := &Processor{
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{serviceInstance},
	}
	initializeTestElectionCoordinators(processor)
	staleCtx := servicecontext.New(context.Background())
	publishTestServiceContext(processor, service)

	if err := processor.deleteServiceForContext(context.Background(), uid, staleCtx); err != nil {
		t.Fatalf("deleteServiceForContext() error = %v", err)
	}
	if got := processor.findServiceInstance(service); got != serviceInstance {
		t.Fatal("stale Service context deleted the current instance")
	}
}

func TestAddServiceMarksPreTrackedInstanceAdded(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	serviceInstance := &instance.Instance{ServiceUID: uid, ServiceSnapshot: service}
	processor := &Processor{
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{serviceInstance},
		nodeLabelManager: &testLabeler{},
	}
	initializeTestElectionCoordinators(processor)
	svcCtx := publishTestServiceContext(processor, service)

	if err := processor.addService(context.Background(), svcCtx, service, &sync.WaitGroup{}); err != nil {
		t.Fatalf("addService() error = %v", err)
	}
	if !serviceInstance.AddCalled {
		t.Fatal("pre-tracked Service instance was not marked added")
	}
	if err := processor.addService(context.Background(), svcCtx, service, &sync.WaitGroup{}); err != nil {
		t.Fatalf("second addService() error = %v", err)
	}
}

func TestPrepareServiceInstanceRejectsCancelledContext(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: types.UID("cancelled-service"), Name: "cancelled-service", Namespace: "default",
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	processor := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}}
	initializeTestElectionCoordinators(processor)
	svcCtx := publishTestServiceContext(processor, service)

	created, err := processor.prepareServiceInstance(ctx, svcCtx, service, &sync.WaitGroup{})
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

func TestPrepareServiceInstanceRejectsStaleServiceContext(t *testing.T) {
	service := admissionTestService("stale-service", "192.0.2.10")
	staleCtx := servicecontext.New(context.Background())
	factoryCalls := 0
	processor := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, svc *v1.Service,
			_ *sync.WaitGroup) (*instance.Instance, error) {
			factoryCalls++
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		}),
	}
	initializeTestElectionCoordinators(processor)
	publishTestServiceContext(processor, service)

	created, err := processor.prepareServiceInstance(context.Background(), staleCtx, service, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("prepareServiceInstance() error = %v", err)
	}
	if created != nil {
		t.Fatal("prepareServiceInstance() returned an instance for a stale Service context")
	}
	if factoryCalls != 0 {
		t.Fatalf("instance factory calls = %d, want 0", factoryCalls)
	}
}

func TestPrepareServiceInstanceCleansUpWhenContextIsCancelledDuringConstruction(t *testing.T) {
	service := admissionTestService("cancelled-during-construction", "192.0.2.10")
	svcCtx := servicecontext.New(context.Background())
	dhcpClient := newTestDHCPClient()
	processor := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, svc *v1.Service,
			_ *sync.WaitGroup) (*instance.Instance, error) {
			svcCtx.Cancel()
			return &instance.Instance{
				ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy(),
				IsDHCPv4: true, DHCPv4Client: dhcpClient,
			}, nil
		}),
	}
	initializeTestElectionCoordinators(processor)
	processor.svcMap.Store(service.UID, svcCtx)

	created, err := processor.prepareServiceInstance(context.Background(), svcCtx, service, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("prepareServiceInstance() error = %v", err)
	}
	if created != nil {
		t.Fatal("prepareServiceInstance() returned an instance after its Service context was cancelled")
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("cancelled Service context left a constructed instance tracked")
	}
	if !dhcpClient.stopped {
		t.Fatal("discarding the constructed instance did not stop its DHCP client")
	}
}

func TestPrepareServiceInstanceUsesSharedFactory(t *testing.T) {
	service := admissionTestService("service", "192.0.2.10")
	called := false
	processor := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
			called = true
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		}),
	}
	initializeTestElectionCoordinators(processor)
	svcCtx := publishTestServiceContext(processor, service)

	created, err := processor.prepareServiceInstance(context.Background(), svcCtx, service, &sync.WaitGroup{})
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
	processor := &Processor{serviceLock: newTestServiceLocks(), ServiceInstances: []*instance.Instance{{
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
	replacementCtx := publishTestServiceContext(processor, service)
	action, current, err := processor.getServiceInstanceAction(replacementCtx, service)
	if err != nil {
		t.Fatalf("getServiceInstanceAction() error = %v", err)
	}
	if !current {
		t.Fatal("replacement Service context was not current")
	}
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
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{{ServiceUID: uid, ServiceSnapshot: service}},
		nodeLabelManager: &testLabeler{},
	}
	initializeTestElectionCoordinators(processor)
	svcCtx := publishTestServiceContext(processor, service)

	action, contextCurrent, err := processor.getServiceInstanceAction(svcCtx, service)
	if err != nil {
		t.Fatalf("getServiceInstanceAction() error = %v", err)
	}
	if !contextCurrent {
		t.Fatal("Service context was not current")
	}
	if action != ActionAdd {
		t.Fatalf("getServiceInstanceAction() = %q, want ActionAdd", action)
	}
	if err := processor.deleteService(context.Background(), uid, nil); err != nil {
		t.Fatalf("deleteService() error = %v", err)
	}
	if err := processor.addService(context.Background(), svcCtx, service, &sync.WaitGroup{}); err != nil {
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
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{serviceInstance},
		nodeLabelManager: labeler,
	}
	initializeTestElectionCoordinators(processor)
	svcCtx := publishTestServiceContext(processor, service)

	if err := processor.addService(context.Background(), svcCtx, service, &sync.WaitGroup{}); err == nil {
		t.Fatal("addService() error = nil, want configuration failure")
	}
	if labeler.addCalls != 1 {
		t.Fatalf("AddLabel calls = %d, want 1", labeler.addCalls)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("configuration failure left a partial instance tracked")
	}
}

func TestAddServiceTreatsContextLossBeforeConfigurationAsNormalCompletion(t *testing.T) {
	service := admissionTestService("cancelled-before-configuration", "192.0.2.10")
	svcCtx := servicecontext.New(context.Background())
	configureLock := make(chan struct{})
	releaseConfigure := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseConfigure) }) })
	keyMutex := &lifecycleBlockingKeyMutex{
		configureLock: configureLock, releaseConfigure: releaseConfigure,
	}
	labeler := &testLabeler{}
	processor := &Processor{
		serviceLock: &ServiceLock{mutex: keyMutex},
		config: &kubevip.Config{
			DisableServiceUpdates: true, EnableServicesElection: true,
		},
		nodeLabelManager: labeler,
		instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, svc *v1.Service,
			_ *sync.WaitGroup) (*instance.Instance, error) {
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		}),
	}
	initializeTestElectionCoordinators(processor)
	processor.svcMap.Store(service.UID, svcCtx)

	done := make(chan error, 1)
	go func() {
		done <- processor.addService(context.Background(), svcCtx, service, &sync.WaitGroup{})
	}()
	select {
	case <-configureLock:
	case <-time.After(time.Second):
		t.Fatal("addService() did not reach configuration")
	}
	svcCtx.Cancel()
	releaseOnce.Do(func() { close(releaseConfigure) })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("addService() error after normal Service context loss = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("addService() did not finish after configuration was released")
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("Service context loss left the prepared instance tracked")
	}
	if labeler.addCalls != 0 {
		t.Fatalf("node label additions after Service context loss = %d, want 0", labeler.addCalls)
	}
}

func TestStaleActivationCleanupPreservesReplacementInstance(t *testing.T) {
	service := admissionTestService("replaced-before-cleanup", "192.0.2.10")
	svcCtx := servicecontext.New(context.Background())
	configureLock := make(chan struct{})
	releaseConfigure := make(chan struct{})
	cleanupLock := make(chan struct{})
	releaseCleanup := make(chan struct{})
	var releaseConfigureOnce sync.Once
	var releaseCleanupOnce sync.Once
	t.Cleanup(func() {
		releaseConfigureOnce.Do(func() { close(releaseConfigure) })
		releaseCleanupOnce.Do(func() { close(releaseCleanup) })
	})
	keyMutex := &lifecycleBlockingKeyMutex{
		configureLock: configureLock, releaseConfigure: releaseConfigure,
		cleanupLock: cleanupLock, releaseCleanup: releaseCleanup,
	}
	processor := &Processor{
		serviceLock: &ServiceLock{mutex: keyMutex},
		config: &kubevip.Config{
			DisableServiceUpdates: true, EnableServicesElection: true,
		},
		nodeLabelManager: &testLabeler{},
		instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, svc *v1.Service,
			_ *sync.WaitGroup) (*instance.Instance, error) {
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		}),
	}
	initializeTestElectionCoordinators(processor)
	processor.svcMap.Store(service.UID, svcCtx)

	done := make(chan error, 1)
	go func() {
		done <- processor.addService(context.Background(), svcCtx, service, &sync.WaitGroup{})
	}()
	select {
	case <-configureLock:
	case <-time.After(time.Second):
		t.Fatal("addService() did not reach configuration")
	}
	svcCtx.Cancel()
	releaseConfigureOnce.Do(func() { close(releaseConfigure) })
	select {
	case <-cleanupLock:
	case <-time.After(time.Second):
		t.Fatal("stale activation did not reach cleanup")
	}

	removed, _ := processor.detachServiceInstance(service.UID)
	if removed == nil {
		t.Fatal("prepared instance was not tracked before cleanup")
	}
	replacementService := service.DeepCopy()
	replacement := &instance.Instance{ServiceUID: service.UID, ServiceSnapshot: replacementService, AddCalled: true}
	processor.appendServiceInstance(replacement)
	releaseCleanupOnce.Do(func() { close(releaseCleanup) })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("addService() error after replacement = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale activation cleanup did not finish")
	}
	if got := processor.findServiceInstance(service); got != replacement {
		t.Fatal("stale activation cleanup removed the replacement instance")
	}
}

func TestDeleteServiceKeepsInstanceWhenLabelRemovalFails(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	serviceInstance := &instance.Instance{ServiceUID: uid, ServiceSnapshot: service, LabelAdded: true}
	processor := &Processor{
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{serviceInstance},
		nodeLabelManager: &testLabeler{removeErr: errors.New("remove label")},
	}
	initializeTestElectionCoordinators(processor)

	if err := processor.deleteService(context.Background(), uid, nil); err == nil {
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
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{replacement},
		nodeLabelManager: &testLabeler{},
	}
	initializeTestElectionCoordinators(processor)

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
	runner := &electionTestRunner{started: make(chan struct{})}
	processor := &Processor{
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{{ServiceUID: uid, ServiceSnapshot: service}},
		leaseMgr:         lease.NewManager(),
	}
	initializeTestElectionCoordinators(processor, withTestCampaignRunner(runner))
	svcCtx := servicecontext.New(context.Background())
	processor.svcMap.Store(uid, svcCtx)
	leaseNamespace, serviceLease := lease.ServiceName(service)
	leaseID := lease.NewID(processor.config.LeaderElectionType, leaseNamespace, serviceLease)
	svcCtx.SignalReadiness()
	done := make(chan error, 1)
	go func() { done <- processor.StartServicesLeaderElection(svcCtx, service, nil) }()
	waitForElectionRunner(t, runner.started)

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
	if err := <-done; err != nil {
		t.Fatalf("service election returned error: %v", err)
	}
}

func TestDeleteTrackedServiceReturnsPersistentCleanupFailure(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID: uid, Name: "service-a", Namespace: "default",
	}}
	labeler := &testLabeler{removeErr: errors.New("permanent remove label")}
	runner := &electionTestRunner{started: make(chan struct{})}
	processor := &Processor{
		serviceLock:      newTestServiceLocks(),
		config:           &kubevip.Config{},
		ServiceInstances: []*instance.Instance{{ServiceUID: uid, ServiceSnapshot: service, LabelAdded: true}},
		nodeLabelManager: labeler,
		leaseMgr:         lease.NewManager(),
	}
	initializeTestElectionCoordinators(processor, withTestCampaignRunner(runner))
	svcCtx := servicecontext.New(context.Background())
	processor.svcMap.Store(uid, svcCtx)
	leaseNamespace, serviceLease := lease.ServiceName(service)
	leaseID := lease.NewID(processor.config.LeaderElectionType, leaseNamespace, serviceLease)
	svcCtx.SignalReadiness()
	done := make(chan error, 1)
	go func() { done <- processor.StartServicesLeaderElection(svcCtx, service, nil) }()
	waitForElectionRunner(t, runner.started)

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
	if err := <-done; err != nil {
		t.Fatalf("service election returned error: %v", err)
	}

	labeler.removeErr = nil
	if err := processor.deleteTrackedService(service); err != nil {
		t.Fatalf("deleteTrackedService() retry error = %v", err)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("retry did not remove the Service instance")
	}
}

func TestOwnedServiceVIPsIncludesOnlyActiveDatapathsAndReturnsCopy(t *testing.T) {
	activeUID := types.UID("active")
	inactiveUID := types.UID("inactive")
	processor := &Processor{
		serviceLock: newTestServiceLocks(),
		ServiceInstances: []*instance.Instance{
			{
				ServiceUID: activeUID,
				ServiceSnapshot: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{UID: activeUID, Annotations: map[string]string{
						kubevip.LoadbalancerIPAnnotation: "192.0.2.10,192.0.2.11",
					}},
				},
				AddCalled: true,
			},
			{
				ServiceUID: inactiveUID,
				ServiceSnapshot: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{UID: inactiveUID},
					Spec:       v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"},
				},
			},
			{ServiceUID: "missing-snapshot", AddCalled: true},
			nil,
		},
	}

	processor.refreshOwnedServiceVIPs()
	want := []string{"192.0.2.10", "192.0.2.11"}
	got := processor.OwnedServiceVIPs()
	if !slices.Equal(got, want) {
		t.Fatalf("OwnedServiceVIPs() = %v, want active datapath VIPs %v", got, want)
	}

	got[0] = "198.51.100.1"
	if current := processor.OwnedServiceVIPs(); !slices.Equal(current, want) {
		t.Fatalf("mutating returned VIPs changed stored snapshot: got %v, want %v", current, want)
	}
}

// TestEndpointReconcileWaitsForServiceLock pins the wiring that stops a late
// endpoint event from reprogramming the datapath of a Service that is being torn
// down. The endpoint processor must take the same per-UID lock as deletion, so a
// reconcile cannot interleave with cleanup and re-add a route nobody owns.
func TestEndpointReconcileWaitsForServiceLock(t *testing.T) {
	uid := types.UID("service-a")
	config := &kubevip.Config{}
	processor := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      config,
	}
	initializeTestElectionCoordinators(processor)
	epProcessor := endpoints.NewEndpointProcessor(config, providers.NewEndpointslices(), nil,
		processor.findServiceInstance, nil, nil, processor.serviceLock)

	processor.serviceLock.Lock(uid)

	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default", UID: uid},
		Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster},
	}
	lastKnown := ""
	reconciled := make(chan error, 1)
	go func() {
		_, err := epProcessor.Reconcile(
			servicecontext.New(context.Background()),
			watch.Event{
				Type:   watch.Modified,
				Object: &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "slice-1"}},
			},
			&lastKnown,
			service,
			"node-1",
			&sync.WaitGroup{},
			nil,
			nil,
		)
		reconciled <- err
	}()

	select {
	case <-reconciled:
		if err := processor.serviceLock.Unlock(uid); err != nil {
			t.Fatalf("Unlock() error = %v", err)
		}
		t.Fatal("endpoint reconcile ignored the Service lock held by deletion")
	case <-time.After(50 * time.Millisecond):
	}

	if err := processor.serviceLock.Unlock(uid); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	select {
	case err := <-reconciled:
		if err != nil {
			t.Fatalf("Reconcile returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("endpoint reconcile did not resume after the Service lock was released")
	}
}
