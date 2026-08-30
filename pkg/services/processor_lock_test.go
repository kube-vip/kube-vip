package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type testLabeler struct {
	addErr       error
	addErrors    []error
	addCalls     int
	removeErr    error
	removeErrors []error
	removeCalls  int
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

func TestModifiedPendingElectionUsesNewSnapshot(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	installEndpointWatchClient(t, p)
	p.config.DebounceTime = "0s"
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldService := electionTestService("pending-modified", "192.0.2.10")
	oldService.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
	newService := oldService.DeepCopy()
	newService.Spec.LoadBalancerIP = "192.0.2.20"
	newService.Annotations[kubevip.ServiceLease] = "replacement"
	oldNamespace, oldName := lease.ServiceName(oldService)
	p.leaseMgr.Add(parent, lease.NewID(p.config.LeaderElectionType, oldNamespace, oldName))
	svcCtx := servicecontext.New(parent)
	p.svcMap.Store(oldService.UID, svcCtx)
	svcCtx.SignalReadiness()
	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, oldService, nil, true) }()
	await(t, runner.started, "pending old campaign")
	if p.findServiceInstance(oldService) != nil {
		t.Fatal("old service unexpectedly activated before acquisition")
	}

	var watchWG sync.WaitGroup
	if err := p.Reconcile(parent, watch.Event{Type: watch.Modified, Object: newService}, NewCallback(p.StartServicesLeaderElection, true), false, &watchWG, func(error) {}); err != nil {
		t.Fatalf("Reconcile modified pending service: %v", err)
	}
	if err := awaitError(t, done, "old callback retirement"); err != nil {
		t.Fatalf("old callback retirement: %v", err)
	}
	currentCtx, err := p.getServiceContext(newService.UID)
	if err != nil || currentCtx == nil || currentCtx == svcCtx {
		t.Fatalf("replacement service context = %v, err = %v", currentCtx, err)
	}
	newNamespace, newName := lease.ServiceName(newService)
	if p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, newNamespace, newName)) == nil {
		t.Fatal("replacement lease was not created")
	}
	currentCtx.SignalReadiness()
	await(t, runner.secondStarted, "replacement campaign")
	runner.acquire()
	awaitCondition(t, func() bool {
		inst := p.findServiceInstance(newService)
		return inst != nil && inst.ServiceSnapshot.Spec.LoadBalancerIP == newService.Spec.LoadBalancerIP && inst.ServiceSnapshot.Annotations[kubevip.ServiceLease] == "replacement"
	}, "replacement snapshot activation")
	if p.findServiceInstance(oldService) != nil {
		inst := p.findServiceInstance(oldService)
		if inst.ServiceSnapshot.Spec.LoadBalancerIP == oldService.Spec.LoadBalancerIP {
			t.Fatal("old snapshot activated after pending modification")
		}
	}
	currentCtx.Cancel()
	watchWG.Wait()
}

func TestModifiedBeforeReadinessReplacesDesiredService(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	installEndpointWatchClient(t, p)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldService := electionTestService("pre-readiness-modified", "192.0.2.10")
	oldService.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
	newService := oldService.DeepCopy()
	newService.Spec.LoadBalancerIP = "192.0.2.20"
	newService.Annotations[kubevip.ServiceLease] = "replacement"
	sharedNetwork := &processorTestNetwork{ip: newService.Spec.LoadBalancerIP}
	activeSibling := testServiceInstance(t, "active-sibling", []vip.Network{sharedNetwork})
	startTestWorkers(t, activeSibling.Clusters[0], activeSibling.VIPConfigs[0])
	defer activeSibling.Clusters[0].StopWorkersAndWaitPreserving(nil)
	sharedNetwork.resetDeleted()
	p.appendServiceInstance(activeSibling)
	garbageCollectCalls := 0
	p.garbageCollect = func(string, string, *networkinterface.Manager) (bool, error) {
		garbageCollectCalls++
		return false, nil
	}
	callback := NewCallback(p.StartServicesLeaderElection, true)
	var watchWG sync.WaitGroup

	if err := p.Reconcile(parent, watch.Event{Type: watch.Added, Object: oldService}, callback, false, &watchWG, func(error) {}); err != nil {
		t.Fatalf("Reconcile added pending service: %v", err)
	}
	oldCtx, err := p.getServiceContext(oldService.UID)
	if err != nil || oldCtx == nil {
		t.Fatalf("pending service context = %v, err = %v", oldCtx, err)
	}
	if snapshot, _ := p.desiredService(oldService.UID); snapshot == nil || snapshot.Spec.LoadBalancerIP != oldService.Spec.LoadBalancerIP {
		t.Fatalf("initial desired snapshot = %#v", snapshot)
	}
	assertNotSignalled(t, runner.started, "campaign started before endpoint readiness")

	if err := p.Reconcile(parent, watch.Event{Type: watch.Modified, Object: newService}, callback, false, &watchWG, func(error) {}); err != nil {
		t.Fatalf("Reconcile modified pre-readiness service: %v", err)
	}
	currentCtx, err := p.getServiceContext(newService.UID)
	if err != nil || currentCtx == nil || currentCtx == oldCtx {
		t.Fatalf("replacement context = %v, err = %v", currentCtx, err)
	}
	if oldCtx.Ctx.Err() == nil {
		t.Fatal("meaningful modification did not cancel the old callback context")
	}
	if garbageCollectCalls != 0 {
		t.Fatalf("GarbageCollect calls = %d, want 0 without a locally tracked old instance", garbageCollectCalls)
	}
	if sharedNetwork.wasDeleted() {
		t.Fatal("inactive member modification removed the active sibling VIP")
	}
	currentCtx.SignalReadiness()
	await(t, runner.started, "replacement readiness campaign")
	runner.acquire()
	awaitCondition(t, func() bool {
		inst := p.findServiceInstance(newService)
		return inst != nil && inst.ServiceSnapshot.Spec.LoadBalancerIP == newService.Spec.LoadBalancerIP &&
			inst.ServiceSnapshot.Annotations[kubevip.ServiceLease] == "replacement"
	}, "replacement desired snapshot activation")
	if runner.count.Load() != 1 {
		t.Fatalf("campaign count = %d, want only replacement campaign", runner.count.Load())
	}
	currentCtx.Cancel()
	await(t, runner.done, "replacement campaign completion")
	watchWG.Wait()
}

func TestLatestModifiedDuringCleanupEventuallyApplies(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	installEndpointWatchClient(t, p)
	p.nodeLabelManager = &testLabeler{removeErrors: []error{
		errors.New("one"), errors.New("two"), errors.New("three"), errors.New("four"), errors.New("five"),
		errors.New("six"), errors.New("seven"), nil,
	}}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldService := electionTestService("slow-modified", "192.0.2.10")
	oldService.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
	newService := oldService.DeepCopy()
	newService.Spec.LoadBalancerIP = "192.0.2.20"
	latestService := newService.DeepCopy()
	latestService.Spec.LoadBalancerIP = "192.0.2.30"
	var createdMu sync.Mutex
	var created []string
	p.newInstance = func(_ context.Context, service *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		createdMu.Lock()
		created = append(created, service.Spec.LoadBalancerIP)
		createdMu.Unlock()
		return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
	}
	namespace, name := lease.ServiceName(oldService)
	p.leaseMgr.Add(parent, lease.NewID(p.config.LeaderElectionType, namespace, name))
	svcCtx := servicecontext.New(parent)
	p.svcMap.Store(oldService.UID, svcCtx)
	svcCtx.SignalReadiness()
	oldDone := make(chan error, 1)
	go func() { oldDone <- p.StartServicesLeaderElection(svcCtx, oldService, nil, true) }()
	await(t, runner.started, "slow-cleanup campaign")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(oldService) != nil }, "old service activation")

	var watchWG sync.WaitGroup
	err := p.Reconcile(parent, watch.Event{Type: watch.Modified, Object: newService}, NewCallback(p.StartServicesLeaderElection, true), false, &watchWG, func(error) {})
	if err == nil {
		t.Fatal("slow cleanup did not report its initial failure")
	}
	if err := p.Reconcile(parent, watch.Event{Type: watch.Modified, Object: latestService}, NewCallback(p.StartServicesLeaderElection, true), false, &watchWG, func(error) {}); err == nil {
		t.Fatal("latest event did not observe pending cleanup")
	}
	awaitCondition(t, func() bool {
		current, getErr := p.getServiceContext(latestService.UID)
		return getErr == nil && current != nil && current != svcCtx
	}, "eventual replacement context")
	currentCtx, _ := p.getServiceContext(latestService.UID)
	currentCtx.SignalReadiness()
	await(t, runner.secondStarted, "slow-cleanup replacement campaign")
	runner.acquire()
	awaitCondition(t, func() bool {
		inst := p.findServiceInstance(latestService)
		return inst != nil && inst.ServiceSnapshot.Spec.LoadBalancerIP == latestService.Spec.LoadBalancerIP
	}, "slow-cleanup replacement activation")
	createdMu.Lock()
	for _, address := range created {
		if address == newService.Spec.LoadBalancerIP {
			t.Fatalf("superseded pending address %s was activated", address)
		}
	}
	createdMu.Unlock()
	if inst := p.findServiceInstance(oldService); inst != nil && inst.ServiceSnapshot.Spec.LoadBalancerIP == oldService.Spec.LoadBalancerIP {
		t.Fatal("old snapshot reactivated during slow cleanup")
	}
	currentCtx.Cancel()
	if err := awaitError(t, oldDone, "old slow-cleanup callback"); err == nil {
		t.Fatal("old slow-cleanup callback did not report its transient cleanup failure")
	}
	watchWG.Wait()
}

func TestPendingLoadBalancerReplayCannotPassTerminalEvent(t *testing.T) {
	for _, terminalType := range []watch.EventType{watch.Modified, watch.Deleted} {
		t.Run(string(terminalType), func(t *testing.T) {
			p := &Processor{
				config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
				leaseMgr:         lease.NewManager(),
				nodeLabelManager: &testLabeler{},
			}
			service := testService("pending-terminal", "192.0.2.10")
			service.Spec.Type = v1.ServiceTypeLoadBalancer
			terminal := service.DeepCopy()
			if terminalType == watch.Modified {
				terminal.Spec.Type = v1.ServiceTypeClusterIP
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			svcCtx := servicecontext.New(parent)
			p.svcMap.Store(service.UID, svcCtx)
			group := p.registerCleanupGroup(parent, &sync.WaitGroup{})
			version := p.recordDesiredEvent(watch.Modified, service)
			var creations atomic.Int64
			p.newInstance = func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error) {
				creations.Add(1)
				return nil, errors.New("unexpected instance creation")
			}
			pending := &pendingReconcile{
				version: version, ctx: parent,
				event: watch.Event{Type: watch.Modified, Object: service.DeepCopy()}, group: group,
			}
			captured := make(chan struct{})
			release := make(chan struct{})
			pending.beforeReplay = func() {
				close(captured)
				<-release
			}
			p.pending = map[types.UID]*pendingReconcile{service.UID: pending}

			replayed := make(chan struct{})
			go func() {
				p.replayPendingReconcile(service.UID, pending)
				close(replayed)
			}()
			await(t, captured, "pending replay capture")
			if terminalType == watch.Deleted {
				if err := p.Delete(watch.Event{Type: terminalType, Object: terminal}, false); err != nil {
					t.Fatalf("Delete terminal event: %v", err)
				}
			} else if err := p.Reconcile(parent, watch.Event{Type: terminalType, Object: terminal}, nil, false, &sync.WaitGroup{}, func(error) {}); err != nil {
				t.Fatalf("Reconcile terminal event: %v", err)
			}
			close(release)
			await(t, replayed, "stale pending replay completion")

			if creations.Load() != 0 {
				t.Fatalf("instance creations = %d, want 0", creations.Load())
			}
			if p.findServiceInstance(service) != nil {
				t.Fatal("terminal event left a service instance")
			}
			if _, ok := p.svcMap.Load(service.UID); ok {
				t.Fatal("terminal event left a service context")
			}
			if p.pendingReconcileCount() != 0 {
				t.Fatalf("pending reconciles = %d, want 0", p.pendingReconcileCount())
			}
		})
	}
}

func TestPendingLoadBalancerReplayUsesLatestVersion(t *testing.T) {
	p := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		leaseMgr:         lease.NewManager(),
		nodeLabelManager: &testLabeler{},
	}
	first := testService("pending-latest", "192.0.2.10")
	first.Spec.Type = v1.ServiceTypeLoadBalancer
	latest := first.DeepCopy()
	latest.Spec.LoadBalancerIP = "192.0.2.20"
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	group := p.registerCleanupGroup(parent, &sync.WaitGroup{})
	firstVersion := p.recordDesiredEvent(watch.Modified, first)
	firstPending := &pendingReconcile{version: firstVersion, ctx: parent, event: watch.Event{Type: watch.Modified, Object: first.DeepCopy()}, group: group}
	latestVersion := p.recordDesiredEvent(watch.Modified, latest)
	latestPending := &pendingReconcile{version: latestVersion, ctx: parent, event: watch.Event{Type: watch.Modified, Object: latest.DeepCopy()}, group: group}
	p.pending = map[types.UID]*pendingReconcile{first.UID: latestPending}

	p.replayPendingReconcile(first.UID, firstPending)
	if p.pendingReconcileCount() != 1 {
		t.Fatalf("stale replay removed latest pending event")
	}
	p.discardPendingReconcile(first.UID)
}

func TestDesiredEventRejectsOlderNumericResourceVersionAfterDelete(t *testing.T) {
	p := &Processor{}
	service := testService("desired-rv", "192.0.2.10")
	service.Spec.Type = v1.ServiceTypeLoadBalancer
	service.ResourceVersion = "10"
	deleteVersion := p.recordDesiredEvent(watch.Deleted, service)

	stale := service.DeepCopy()
	stale.ResourceVersion = "9"
	if version := p.recordDesiredEvent(watch.Modified, stale); version != 0 {
		t.Fatalf("older Modified version = %d, want rejected", version)
	}
	if !p.desiredTerminalEventCurrent(service.UID, deleteVersion) {
		t.Fatal("older Modified replaced the delete tombstone")
	}
}

func TestDesiredEventEmptyResourceVersionUsesArrivalOrder(t *testing.T) {
	p := &Processor{}
	service := testService("desired-empty-rv", "192.0.2.10")
	service.Spec.Type = v1.ServiceTypeLoadBalancer
	first := p.recordDesiredEvent(watch.Modified, service)
	service.Spec.LoadBalancerIP = "192.0.2.20"
	second := p.recordDesiredEvent(watch.Modified, service)
	if second <= first {
		t.Fatalf("arrival versions = %d, %d, want increasing", first, second)
	}
}

func TestIgnoredForcedWatcherCannotReplaceDesiredState(t *testing.T) {
	p := &Processor{config: &kubevip.Config{}, lbClassFilter: func(*v1.Service, *kubevip.Config) bool { return false }}
	forced := testService("forced-owner", "192.0.2.10")
	forced.Spec.Type = v1.ServiceTypeLoadBalancer
	forced.ResourceVersion = "10"
	forced.Annotations = map[string]string{kubevip.ForcePerServiceElection: "true"}
	if err := p.Reconcile(context.Background(), watch.Event{Type: watch.Modified, Object: forced}, nil, false, &sync.WaitGroup{}, func(error) {}); err != nil {
		t.Fatalf("ignored Reconcile: %v", err)
	}
	if desired, _ := p.desiredService(forced.UID); desired != nil {
		t.Fatal("non-owning watcher recorded forced Service")
	}

	version := p.recordDesiredEvent(watch.Modified, forced)
	ignoredDelete := forced.DeepCopy()
	ignoredDelete.ResourceVersion = "11"
	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: ignoredDelete}, false); err != nil {
		t.Fatalf("ignored Delete: %v", err)
	}
	if !p.desiredEventCurrent(forced.UID, version) {
		t.Fatal("non-owning watcher overwrote desired Service with Delete")
	}
}

func TestStatusOnlyDesiredUpdateKeepsActivationCurrent(t *testing.T) {
	p := &Processor{}
	service := testService("status-only", "192.0.2.10")
	service.Spec.Type = v1.ServiceTypeLoadBalancer
	service.ResourceVersion = "10"
	version := p.recordDesiredEvent(watch.Modified, service)
	expected := &serviceExpectation{version: version, lifecycle: serviceLifecycleFor(service)}

	updated := service.DeepCopy()
	updated.ResourceVersion = "11"
	updated.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: service.Spec.LoadBalancerIP}}
	if got := p.recordDesiredEvent(watch.Modified, updated); got != version {
		t.Fatalf("status-only update lifecycle version = %d, want %d", got, version)
	}
	if !p.desiredLifecycleCurrent(service.UID, expected.version, expected.lifecycle) {
		t.Fatal("status-only update invalidated in-flight activation")
	}
	latest, _ := p.desiredService(service.UID)
	if latest == nil || latest.ResourceVersion != "11" || len(latest.Status.LoadBalancer.Ingress) != 1 {
		t.Fatalf("latest desired Service = %#v", latest)
	}
}

func TestDesiredDeleteTombstonesAreCompactAndBounded(t *testing.T) {
	p := &Processor{}
	for i := range maxDesiredDeleteTombstones + 1 {
		service := testService(fmt.Sprintf("deleted-%d", i), "192.0.2.10")
		service.Spec.Type = v1.ServiceTypeLoadBalancer
		service.ResourceVersion = "1"
		p.recordDesiredEvent(watch.Deleted, service)
	}
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	if got := len(p.desiredEvents); got != maxDesiredDeleteTombstones {
		t.Fatalf("desired tombstones = %d, want %d", got, maxDesiredDeleteTombstones)
	}
	for _, desired := range p.desiredEvents {
		if desired.service != nil {
			t.Fatal("terminal desired entry retained a Service deep copy")
		}
	}
}

func installEndpointWatchClient(t *testing.T, p *Processor) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("new test client: %v", err)
	}
	p.clientSet, p.rwClientSet = client, client
	p.config.DebounceTime = "0s"
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
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			unlockService := processor.lockService(uid)
			serviceInstance.ServiceSnapshot = &v1.Service{ObjectMeta: metav1.ObjectMeta{
				UID: uid, Name: "service-a", Namespace: "default",
			}}
			unlockService()
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			snapshots := processor.ServiceSnapshots()
			if len(snapshots) != 1 || snapshots[0].UID != uid {
				t.Errorf("ServiceSnapshots() = %+v, want one snapshot for %q", snapshots, uid)
				return
			}
		}
	}()
	wg.Wait()
}

func TestDeleteServicePreservesOnlyActiveSiblingAddresses(t *testing.T) {
	const shared = "192.0.2.10"
	tests := []struct {
		name          string
		activeSibling bool
		wantDeleted   bool
	}{
		{name: "inactive sibling", wantDeleted: true},
		{name: "active sibling", activeSibling: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			departingNetwork := &processorTestNetwork{ip: shared}
			departing := testServiceInstance(t, "departing", []vip.Network{departingNetwork})
			sibling := testServiceInstance(t, "sibling", []vip.Network{&processorTestNetwork{ip: shared}})
			p := testCleanupProcessor(departing, sibling)
			startTestWorkers(t, departing.Clusters[0], departing.VIPConfigs[0])
			departingNetwork.resetDeleted()
			if tt.activeSibling {
				startTestWorkers(t, sibling.Clusters[0], sibling.VIPConfigs[0])
				defer sibling.Clusters[0].StopWorkersAndWaitPreserving(nil)
			}

			if err := p.deleteService(context.Background(), departing.UID()); err != nil {
				t.Fatalf("deleteService() error = %v", err)
			}
			if got := departingNetwork.wasDeleted(); got != tt.wantDeleted {
				t.Fatalf("VIP deleted = %v, want %v", got, tt.wantDeleted)
			}
		})
	}
}

func TestDeleteServicePreservesSharedVIPAndDeletesUniqueVIP(t *testing.T) {
	sharedNetwork := &processorTestNetwork{ip: "192.0.2.10"}
	uniqueNetwork := &processorTestNetwork{ip: "192.0.2.11"}
	departing := testServiceInstance(t, "departing", []vip.Network{sharedNetwork, uniqueNetwork})
	sibling := testServiceInstance(t, "sibling", []vip.Network{&processorTestNetwork{ip: sharedNetwork.ip}})
	p := testCleanupProcessor(departing, sibling)
	startTestWorkers(t, departing.Clusters[0], departing.VIPConfigs[0])
	sharedNetwork.resetDeleted()
	uniqueNetwork.resetDeleted()
	startTestWorkers(t, sibling.Clusters[0], sibling.VIPConfigs[0])
	defer sibling.Clusters[0].StopWorkersAndWaitPreserving(nil)

	if err := p.deleteService(context.Background(), departing.UID()); err != nil {
		t.Fatalf("deleteService() error = %v", err)
	}
	if sharedNetwork.wasDeleted() {
		t.Fatal("shared VIP was deleted")
	}
	if !uniqueNetwork.wasDeleted() {
		t.Fatal("unique VIP was preserved")
	}
}

func TestActiveSiblingPreservesOnlyRunningClusterAddresses(t *testing.T) {
	sharedNetwork := &processorTestNetwork{ip: "192.0.2.10"}
	inactiveNetwork := &processorTestNetwork{ip: "192.0.2.11"}
	departing := testServiceInstance(t, "departing", []vip.Network{sharedNetwork, inactiveNetwork})
	sibling := testServiceInstance(t, "sibling", []vip.Network{&processorTestNetwork{ip: sharedNetwork.ip}})
	inactiveClusterInstance := testServiceInstance(t, "inactive-cluster", []vip.Network{&processorTestNetwork{ip: inactiveNetwork.ip}})
	sibling.ServiceAddresses = append(sibling.ServiceAddresses, inactiveNetwork.ip)
	sibling.Clusters = append(sibling.Clusters, inactiveClusterInstance.Clusters[0])
	sibling.VIPConfigs = append(sibling.VIPConfigs, inactiveClusterInstance.VIPConfigs[0])
	p := testCleanupProcessor(departing, sibling)
	startTestWorkers(t, departing.Clusters[0], departing.VIPConfigs[0])
	sharedNetwork.resetDeleted()
	inactiveNetwork.resetDeleted()
	startTestWorkers(t, sibling.Clusters[0], sibling.VIPConfigs[0])
	defer sibling.Clusters[0].StopWorkersAndWaitPreserving(nil)

	if err := p.deleteService(context.Background(), departing.UID()); err != nil {
		t.Fatalf("deleteService() error = %v", err)
	}
	if sharedNetwork.wasDeleted() {
		t.Fatal("running sibling cluster VIP was deleted")
	}
	if !inactiveNetwork.wasDeleted() {
		t.Fatal("inactive sibling cluster VIP was preserved")
	}
}

func TestDeleteServiceKeepsReferencedVLANForDistinctVIP(t *testing.T) {
	departing := testServiceInstance(t, "departing", nil)
	departing.IsVLAN, departing.VLANInterface = true, "eth0.100"
	sibling := testServiceInstance(t, "sibling", nil)
	sibling.IsVLAN, sibling.VLANInterface = true, departing.VLANInterface
	p := testCleanupProcessor(departing, sibling)

	if err := p.deleteService(context.Background(), departing.UID()); err != nil {
		t.Fatalf("deleteService() attempted to remove referenced VLAN: %v", err)
	}
}

func TestDHCPPlaceholdersDoNotShareVIPLock(t *testing.T) {
	p := &Processor{}
	first := testService("first", "0.0.0.0")
	second := testService("second", "0.0.0.0")
	unlockFirst := p.lockServiceResources(first)
	acquired := make(chan struct{})
	go func() {
		unlockSecond := p.lockServiceResources(second)
		close(acquired)
		unlockSecond()
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("distinct DHCP placeholders shared a VIP lock")
	}
	unlockFirst()
}

func TestConcurrentDHCPPlaceholderDeletionStopsBothClients(t *testing.T) {
	firstClient := newProcessorTestDHCPClient()
	secondClient := newProcessorTestDHCPClient()
	first := &instance.Instance{
		ServiceSnapshot: testService("first", "0.0.0.0"),
		IsDHCPv4:        true,
		DHCPInterface:   "vip-shared",
		DHCPv4Client:    firstClient,
	}
	second := &instance.Instance{
		ServiceSnapshot: testService("second", "0.0.0.0"),
		IsDHCPv4:        true,
		DHCPInterface:   "vip-shared",
		DHCPv4Client:    secondClient,
	}
	p := testCleanupProcessor(first, second)

	done := make(chan error, 2)
	go func() { done <- p.deleteService(context.Background(), first.UID()) }()
	go func() { done <- p.deleteService(context.Background(), second.UID()) }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("deleteService() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent DHCP placeholder deletion deadlocked")
		}
	}
	if !firstClient.stopped() || !secondClient.stopped() {
		t.Fatal("departing DHCP clients were not both stopped")
	}
}

func TestVIPLockOrdersOldCleanupBeforeReplacementPublication(t *testing.T) {
	p := &Processor{}
	oldService := testService("old", "192.0.2.10")
	replacement := testService("replacement", oldService.Spec.LoadBalancerIP)
	unlockOld := p.lockServiceResources(oldService)
	published := make(chan struct{})
	go func() {
		unlockReplacement := p.lockServiceResources(replacement)
		close(published)
		unlockReplacement()
	}()
	select {
	case <-published:
		t.Fatal("replacement published before old cleanup completed")
	case <-time.After(25 * time.Millisecond):
	}
	unlockOld()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("replacement remained blocked after old cleanup")
	}
}

func TestResolvedHostnameVIPUsesAddressLock(t *testing.T) {
	p := &Processor{}
	hostnameService := testService("hostname", "")
	hostnameService.Annotations = map[string]string{kubevip.LoadbalancerIPAnnotation: "lb.example.test"}
	resolved := testServiceInstance(t, "resolved", []vip.Network{&processorTestNetwork{ip: "192.0.2.10"}})
	addressService := testService("address", "192.0.2.10")
	unlocked := p.lockInstanceResources(hostnameService, resolved)
	acquired := make(chan struct{})
	go func() {
		unlockAddress := p.lockServiceResources(addressService)
		close(acquired)
		unlockAddress()
	}()
	select {
	case <-acquired:
		t.Fatal("resolved hostname and address did not share a VIP lock")
	case <-time.After(25 * time.Millisecond):
	}
	unlocked()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("address lock remained blocked after hostname instance unlock")
	}
}

func TestMixedStaticHostnameServicesSerializeWithoutDNSLookup(t *testing.T) {
	p := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		nodeLabelManager: &testLabeler{},
	}
	first := testService("first", "")
	first.Annotations = map[string]string{kubevip.LoadbalancerIPAnnotation: "192.0.2.10,b.example.test"}
	second := testService("second", "")
	second.Annotations = map[string]string{kubevip.LoadbalancerIPAnnotation: "192.0.2.11,a.example.test"}

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	p.newInstance = func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		if svc.UID == first.UID {
			close(firstEntered)
			<-releaseFirst
		} else {
			close(secondEntered)
		}
		return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc}, nil
	}

	done := make(chan error, 2)
	go func() { done <- p.addService(context.Background(), first, &sync.WaitGroup{}) }()
	<-firstEntered
	go func() { done <- p.addService(context.Background(), second, &sync.WaitGroup{}) }()
	select {
	case <-secondEntered:
		t.Fatal("mixed static/hostname construction was not globally serialized")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("addService() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("mixed static/hostname service startup deadlocked")
		}
	}
	if got := p.resourceLocks.len(); got != 0 {
		t.Fatalf("resource lock entries after startup = %d, want 0", got)
	}
}

func TestUnresolvedDDNSReachesInstanceFactory(t *testing.T) {
	p := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		nodeLabelManager: &testLabeler{},
	}
	service := testService("ddns", "")
	service.Annotations = map[string]string{
		kubevip.LoadbalancerIPAnnotation: "unresolved.invalid",
		kubevip.ServiceDDNS:              "true",
	}
	called := make(chan struct{})
	p.newInstance = func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		close(called)
		return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc}, nil
	}

	if err := p.addService(context.Background(), service, &sync.WaitGroup{}); err != nil {
		t.Fatalf("addService() error = %v", err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("unresolved DDNS service did not reach instance factory")
	}
}

func TestServiceNameResourceKeySerializesReplacementUID(t *testing.T) {
	p := &Processor{}
	oldService := testService("old-uid", "192.0.2.10")
	replacement := testService("new-uid", "192.0.2.11")
	replacement.Name = oldService.Name

	unlockOld := p.lockServiceResources(oldService)
	acquired := make(chan struct{})
	go func() {
		unlockReplacement := p.lockServiceResources(replacement)
		close(acquired)
		unlockReplacement()
	}()
	select {
	case <-acquired:
		t.Fatal("replacement UID bypassed the Service-name resource lock")
	case <-time.After(25 * time.Millisecond):
	}
	unlockOld()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("replacement UID remained blocked after Service-name lock release")
	}
}

func TestResourceLockEntriesAreReclaimed(t *testing.T) {
	p := &Processor{}
	for range 100 {
		unlock := p.lockResources([]string{"vip:192.0.2.10", "vlan:eth0.100"})
		unlock()
	}
	if got := p.resourceLocks.len(); got != 0 {
		t.Fatalf("resource lock entries after release = %d, want 0", got)
	}
}

func TestAddServiceSerializesConstructionByVLAN(t *testing.T) {
	p := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		nodeLabelManager: &testLabeler{},
	}
	first := testService("first", "192.0.2.10")
	second := testService("second", "192.0.2.11")
	first.Annotations = map[string]string{kubevip.ServiceVlan: "eth0.100"}
	second.Annotations = map[string]string{kubevip.ServiceVlan: "eth0.100"}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	p.newInstance = func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		if svc.UID == first.UID {
			close(firstEntered)
			<-releaseFirst
		} else {
			close(secondEntered)
		}
		return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc, IsVLAN: true, VLANInterface: "eth0.100"}, nil
	}
	done := make(chan error, 2)
	go func() { done <- p.addService(context.Background(), first, &sync.WaitGroup{}) }()
	<-firstEntered
	go func() { done <- p.addService(context.Background(), second, &sync.WaitGroup{}) }()
	select {
	case <-secondEntered:
		t.Fatal("same VLAN was constructed concurrently")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second VLAN construction remained blocked")
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("addService() error = %v", err)
		}
	}
}

func TestCleanupWaitsForReplacementConstruction(t *testing.T) {
	old := testServiceInstance(t, "old", nil)
	old.ServiceSnapshot.Name = "service"
	p := testCleanupProcessor(old)
	replacement := testService("replacement", "192.0.2.10")
	replacement.Name = old.ServiceSnapshot.Name
	constructing := make(chan struct{})
	releaseConstruction := make(chan struct{})
	p.newInstance = func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		close(constructing)
		<-releaseConstruction
		return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc}, nil
	}

	addDone := make(chan error, 1)
	go func() { addDone <- p.addService(context.Background(), replacement, &sync.WaitGroup{}) }()
	<-constructing
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- p.deleteService(context.Background(), old.UID()) }()
	select {
	case err := <-deleteDone:
		t.Fatalf("cleanup completed during replacement construction: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseConstruction)
	if err := <-addDone; err != nil {
		t.Fatalf("addService() error = %v", err)
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("deleteService() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup remained blocked after replacement construction")
	}
}

func TestServiceResourceKeysIncludeDatapathResources(t *testing.T) {
	service := testService("service-uid-long", "0.0.0.0")
	service.Name = "service"
	service.Annotations = map[string]string{
		kubevip.LoadbalancerIPAnnotation: "0.0.0.0,lb.example.test",
		kubevip.ServiceVlan:              "eth0.100",
		kubevip.MacvlanName:              "vip-custom",
		kubevip.RequestedIP:              "192.0.2.20",
	}
	keys := strings.Join(serviceResourceKeys(service), ",")
	for _, want := range []string{
		"service:default/service",
		"hostname:lb.example.test",
		"vlan:eth0.100",
		"dhcp:vip-custom",
		"vip:192.0.2.20",
	} {
		if !strings.Contains(keys, want) {
			t.Fatalf("resource keys %q do not contain %q", keys, want)
		}
	}
}

func testCleanupProcessor(instances ...*instance.Instance) *Processor {
	return &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
		ServiceInstances: instances,
		nodeLabelManager: &testLabeler{},
	}
}

func testServiceInstance(t *testing.T, name string, networks []vip.Network) *instance.Instance {
	t.Helper()
	service := testService(name, "")
	serviceAddresses := make([]string, 0, len(networks))
	for _, network := range networks {
		serviceAddresses = append(serviceAddresses, network.IP())
	}
	service.Spec.LoadBalancerIP = ""
	service.Annotations = map[string]string{kubevip.LoadbalancerIPAnnotation: strings.Join(serviceAddresses, ",")}
	config := &kubevip.Config{EnableServicesElection: true, DisableServiceUpdates: true, VIPSubnet: "32"}
	c, err := cluster.InitCluster(config, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("InitCluster() error = %v", err)
	}
	c.Network = networks
	return &instance.Instance{
		ServiceUID:       service.UID,
		ServiceAddresses: serviceAddresses,
		ServiceSnapshot:  service,
		VIPConfigs:       []*kubevip.Config{config},
		Clusters:         []*cluster.Cluster{c},
	}
}

func testService(name, address string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(name), Name: name, Namespace: "default"},
		Spec:       v1.ServiceSpec{LoadBalancerIP: address},
	}
}

func startTestWorkers(t *testing.T, c *cluster.Cluster, config *kubevip.Config) {
	t.Helper()
	if err := c.StartLoadBalancerService(context.Background(), config, nil, "service", &sync.WaitGroup{}); err != nil {
		t.Fatalf("StartLoadBalancerService() error = %v", err)
	}
}

type processorTestNetwork struct {
	mu      sync.Mutex
	ip      string
	deleted bool
	deletes int
}

func (n *processorTestNetwork) AddIP(bool, bool, ...int) (bool, error) { return true, nil }
func (n *processorTestNetwork) AddRoute(bool) (bool, error)            { return false, nil }
func (n *processorTestNetwork) ReplaceRoute() error                    { return nil }
func (n *processorTestNetwork) DeleteIP() (bool, error) {
	n.mu.Lock()
	n.deleted = true
	n.deletes++
	n.mu.Unlock()
	return true, nil
}
func (n *processorTestNetwork) DeleteRoute() error            { return nil }
func (n *processorTestNetwork) UpdateRoutes() (bool, error)   { return false, nil }
func (n *processorTestNetwork) IsSet() (*netlink.Addr, error) { return nil, nil }
func (n *processorTestNetwork) IP() string                    { return n.ip }
func (n *processorTestNetwork) CIDR() string                  { return n.ip + "/32" }
func (n *processorTestNetwork) IPisLinkLocal() bool           { return false }
func (n *processorTestNetwork) PrepareRoute() *netlink.Route  { return nil }
func (n *processorTestNetwork) RouteHash() string             { return "" }
func (n *processorTestNetwork) SetIP(ip string) error         { n.ip = ip; return nil }
func (n *processorTestNetwork) SetServicePorts(*v1.Service)   {}
func (n *processorTestNetwork) Interface() string             { return "lo" }
func (n *processorTestNetwork) IsDADFAIL() bool               { return false }
func (n *processorTestNetwork) IsDNS() bool                   { return false }
func (n *processorTestNetwork) IsDDNS() bool                  { return false }
func (n *processorTestNetwork) DDNSHostName() string          { return "" }
func (n *processorTestNetwork) DNSName() string               { return "" }
func (n *processorTestNetwork) SetMask(string) error          { return nil }
func (n *processorTestNetwork) SetHasEndpoints(bool)          {}
func (n *processorTestNetwork) HasEndpoints() bool            { return false }
func (n *processorTestNetwork) ARPName() string               { return "" }
func (n *processorTestNetwork) GetPossibleSubnets() string    { return "" }
func (n *processorTestNetwork) DHCPFamily() string            { return "" }
func (n *processorTestNetwork) IPVSMark() uint32              { return 0 }
func (n *processorTestNetwork) wasDeleted() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.deleted
}

func (n *processorTestNetwork) resetDeleted() {
	n.mu.Lock()
	n.deleted = false
	n.deletes = 0
	n.mu.Unlock()
}

func (n *processorTestNetwork) deleteCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.deletes
}

type processorTestDHCPClient struct {
	mu       sync.Mutex
	stopCall bool
	ip       chan string
	err      chan error
}

func newProcessorTestDHCPClient() *processorTestDHCPClient {
	return &processorTestDHCPClient{ip: make(chan string), err: make(chan error)}
}

func (c *processorTestDHCPClient) ErrorChannel() chan error { return c.err }
func (c *processorTestDHCPClient) IPChannel() chan string   { return c.ip }
func (c *processorTestDHCPClient) Start(context.Context) error {
	return nil
}
func (c *processorTestDHCPClient) Stop() {
	c.mu.Lock()
	c.stopCall = true
	c.mu.Unlock()
}
func (c *processorTestDHCPClient) WithHostName(string) vip.DHCPClient { return c }
func (c *processorTestDHCPClient) stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopCall
}
