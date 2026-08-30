package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/prometheus/client_golang/prometheus/testutil"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestSharedElectionDeletedCandidateNeverActivates(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	var candidateInstances, siblingInstances atomic.Int64
	p.newInstance = func(_ context.Context, service *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		switch service.Name {
		case "candidate":
			candidateInstances.Add(1)
		case "sibling":
			siblingInstances.Add(1)
		}
		return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	candidate := electionTestService("candidate", "10.0.0.1")
	sibling := electionTestService("sibling", "10.0.0.2")

	addElectionTestMember(t, p, parent, candidate)
	siblingCtx := addElectionTestMember(t, p, parent, sibling)
	await(t, runner.started, "campaign start")
	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: candidate}, false); err != nil {
		t.Fatalf("Delete candidate: %v", err)
	}
	awaitCondition(t, func() bool { return p.findServiceInstance(candidate) == nil }, "candidate cleanup")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(sibling) != nil }, "sibling activation")
	if got := p.findServiceInstance(candidate); got != nil {
		t.Fatal("deleted candidate activated")
	}
	if candidateInstances.Load() != 0 {
		t.Fatalf("deleted candidate instance creations = %d, want 0", candidateInstances.Load())
	}
	if siblingInstances.Load() != 1 {
		t.Fatalf("sibling instance creations = %d, want 1", siblingInstances.Load())
	}
	if runner.count.Load() != 1 {
		t.Fatalf("campaign count = %d, want 1", runner.count.Load())
	}
	siblingCtx.Cancel()
	await(t, runner.done, "campaign completion")
}

func TestSharedElectionCleanupRetriesFailOnce(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	labeler := &testLabeler{removeErrors: []error{errors.New("fail once"), nil}}
	p.nodeLabelManager = labeler
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := electionTestService("service", "10.0.0.1")
	svcCtx := addElectionTestMember(t, p, parent, service)
	await(t, runner.started, "campaign start")
	runner.acquire()
	awaitCondition(t, func() bool {
		unlock := p.lockService(service.UID)
		defer unlock()
		inst := p.findServiceInstance(service)
		return inst != nil && inst.LabelAdded
	}, "completed activation")

	svcCtx.ResetReadiness()
	if err := p.waitAndRetryServiceElectionMember(svcCtx); err != nil {
		t.Fatalf("cleanup member: %v", err)
	}
	await(t, runner.done, "campaign completion")
	if labeler.removeCalls != 2 {
		t.Fatalf("label cleanup calls = %d, want 2", labeler.removeCalls)
	}
	if p.findServiceInstance(service) != nil {
		t.Fatal("fail-once cleanup left the service instance active")
	}
}

func TestSharedElectionCleanupReturnsDeterministicErrorAfterParentCancellation(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	cleanupFailure := errors.New("persistent cleanup failure")
	p.nodeLabelManager = &testLabeler{removeErr: cleanupFailure}
	parent, cancel := context.WithCancel(context.Background())
	cleanup := p.registerCleanupGroup(parent)
	service := electionTestService("service", "10.0.0.1")
	namespace, name := lease.ServiceName(service)
	p.leaseMgr.Add(parent, lease.NewID(p.config.LeaderElectionType, namespace, name))
	svcCtx := servicecontext.New(parent)
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	done := make(chan error, 1)
	go func() { done <- p.startLeaderElection(parent, svcCtx, service, nil) }()
	await(t, runner.started, "campaign start")
	runner.acquire()
	awaitCondition(t, func() bool {
		unlock := p.lockService(service.UID)
		defer unlock()
		inst := p.findServiceInstance(service)
		return inst != nil && inst.LabelAdded
	}, "completed activation")

	c, member := p.serviceElectionMember(service, svcCtx)
	finalized := make(chan struct{})
	p.finalizeServiceElectionMember(c, member, func() { close(finalized) })
	svcCtx.ResetReadiness()
	cancel()
	if err := awaitError(t, done, "leader loop shutdown"); !errors.Is(err, cleanupFailure) {
		t.Fatalf("leader loop shutdown error = %v, want %v", err, cleanupFailure)
	}
	await(t, runner.done, "campaign completion")
	await(t, finalized, "member finalizer")
	if err := p.finishCleanupGroup(cleanup); !errors.Is(err, errServiceCleanupShutdown) {
		t.Fatalf("tracked cleanup error = %v, want %v", err, errServiceCleanupShutdown)
	}
	if p.leaseMgr.Get(c.id) == nil {
		t.Fatal("failed shutdown cleanup voluntarily released the lease claim")
	}
}

func TestSharedElectionCleanupFinalAttemptSucceedsAfterParentCancellation(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	p.nodeLabelManager = &testLabeler{removeErrors: []error{
		errors.New("one"), errors.New("two"), errors.New("three"), errors.New("four"), errors.New("five"), nil,
	}}
	parent, cancel := context.WithCancel(context.Background())
	cleanup := p.registerCleanupGroup(parent)
	service := electionTestService("shutdown-success", "10.0.0.1")
	svcCtx := addElectionTestMember(t, p, parent, service)
	await(t, runner.started, "campaign start")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(service) != nil }, "activation")

	svcCtx.ResetReadiness()
	cancel()
	if err := p.finishCleanupGroup(cleanup); err != nil {
		t.Fatalf("tracked cleanup error = %v", err)
	}
	awaitCondition(t, func() bool { return p.findServiceInstance(service) == nil }, "final cleanup success")
}

func TestSharedElectionStoppedDuringActivationCreatesNothing(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	activationStarted := make(chan struct{})
	releaseActivation := make(chan struct{})
	p.newInstance = func(_ context.Context, service *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		close(activationStarted)
		<-releaseActivation
		return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := electionTestService("service", "10.0.0.1")
	svcCtx := addElectionTestMember(t, p, parent, service)
	await(t, runner.started, "campaign start")
	runner.acquire()
	await(t, activationStarted, "activation start")

	svcCtx.Cancel()
	runner.stop()
	close(releaseActivation)
	await(t, runner.done, "campaign completion")
	awaitCondition(t, func() bool { return !p.serviceElectionMemberExists(service, svcCtx) }, "member retirement")
	if p.findServiceInstance(service) != nil {
		t.Fatal("stopped activation left a service instance active")
	}
}

func TestSharedElectionStopWaitsForActivationRollbackBeforeRestart(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	activationStarted := make(chan struct{})
	releaseActivation := make(chan struct{})
	p.newInstance = func(ctx context.Context, service *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		close(activationStarted)
		<-releaseActivation
		if ctx.Err() == nil {
			t.Error("activation context was not canceled by campaign stop")
		}
		return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
	}
	service := electionTestService("service", "10.0.0.1")
	svcCtx := addElectionTestMember(t, p, context.Background(), service)
	await(t, runner.started, "campaign start")
	runner.acquire()
	await(t, activationStarted, "activation start")

	runner.stop()
	assertNotSignalled(t, runner.secondStarted, "successor campaign started during activation")
	close(releaseActivation)
	await(t, runner.secondStarted, "successor campaign after rollback")
	if inst := p.findServiceInstance(service); inst != nil {
		t.Fatal("stopped activation remained active or labeled")
	}
	svcCtx.Cancel()
	runner.stop()
}

func TestGenericLeaseCancellationStopsCoordinatorRunner(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := electionTestService("service", "10.0.0.1")
	svcCtx := addElectionTestMember(t, p, parent, service)
	await(t, runner.started, "campaign start")

	namespace, name := lease.ServiceName(service)
	p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, namespace, name)).Cancel()
	await(t, runner.cancelled, "generic lease cancellation")
	await(t, runner.done, "old generic lease runner completion")
	await(t, runner.secondStarted, "replacement generic lease runner")
	if svcCtx.Ctx.Err() != nil {
		t.Fatal("generic lease cancellation canceled the service context")
	}
	svcCtx.Cancel()
	runner.stop()
}

func TestPublicCallbackRecoversAfterEndpointLoss(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	service := electionTestService("callback-recovery", "10.0.0.1")
	service.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
	service.ResourceVersion = "2"
	service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if strings.Contains(r.URL.Path, "endpointslices") {
			slice := &discoveryv1.EndpointSlice{
				TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSlice"},
				ObjectMeta: metav1.ObjectMeta{Name: "callback-recovery", Namespace: service.Namespace,
					ResourceVersion: "2", Labels: map[string]string{discoveryv1.LabelServiceName: service.Name}},
				AddressType: discoveryv1.AddressTypeIPv4,
				Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.2"}}},
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"type": watch.Added, "object": slice}); err != nil {
				return
			}
		} else {
			if err := json.NewEncoder(w).Encode(map[string]any{"type": watch.Added, "object": service}); err != nil {
				return
			}
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("new test client: %v", err)
	}
	p.clientSet, p.rwClientSet = client, client
	p.config.DebounceTime = "0s"
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ServicesWatcher(parent, NewCallback(p.StartServicesLeaderElection, true), false) }()
	var svcCtx *servicecontext.Context
	awaitCondition(t, func() bool {
		svcCtx, _ = p.getServiceContext(service.UID)
		return svcCtx != nil
	}, "public watcher service context")
	svcCtx.SignalReadiness()
	await(t, runner.started, "public callback campaign")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(service) != nil }, "public callback activation")

	svcCtx.ResetReadiness()
	awaitCondition(t, func() bool { return p.findServiceInstance(service) == nil }, "endpoint-loss cleanup")
	svcCtx.SignalReadiness()
	await(t, runner.secondStarted, "restored endpoint campaign")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(service) != nil }, "restored endpoint activation")

	cancel()
	if err := awaitError(t, done, "public watcher cancellation"); err != nil {
		t.Fatalf("public ServicesWatcher error = %v", err)
	}
}

func TestServiceElectionAttemptMetricWaitsForReadiness(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	service := electionTestService("attempt-waits", "10.0.0.1")
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	namespace, name := lease.ServiceName(service)
	p.leaseMgr.Add(parent, lease.NewID(p.config.LeaderElectionType, namespace, name))
	svcCtx := servicecontext.New(parent)
	p.svcMap.Store(service.UID, svcCtx)
	before := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name))
	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil, true) }()

	time.Sleep(50 * time.Millisecond)
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != before {
		t.Fatalf("attempt metric while waiting = %v, want %v", got, before)
	}
	svcCtx.SignalReadiness()
	await(t, runner.started, "metric campaign")
	awaitCondition(t, func() bool {
		return testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)) == before+1
	}, "post-claim attempt metric")
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != before+1 {
		t.Fatalf("attempt metric after claim = %v, want %v", got, before+1)
	}
	svcCtx.Cancel()
	if err := awaitError(t, done, "metric loop cancellation"); err != nil {
		t.Fatalf("metric loop cancellation: %v", err)
	}
}

func TestSharedElectionDifferentMemberParentsUseLeaseContext(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	leaseParent, cancelLease := context.WithCancel(context.Background())
	defer cancelLease()
	firstParent, cancelFirst := context.WithCancel(context.Background())
	secondParent, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	first := electionTestService("different-parent-first", "10.0.0.1")
	second := electionTestService("different-parent-second", "10.0.0.2")
	namespace, name := lease.ServiceName(first)
	p.leaseMgr.Add(leaseParent, lease.NewID(p.config.LeaderElectionType, namespace, name))
	firstCtx := servicecontext.New(firstParent)
	secondCtx := servicecontext.New(secondParent)
	p.svcMap.Store(first.UID, firstCtx)
	p.svcMap.Store(second.UID, secondCtx)
	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- p.StartServicesLeaderElection(firstCtx, first, nil, true) }()
	go func() { secondDone <- p.StartServicesLeaderElection(secondCtx, second, nil, true) }()
	awaitCondition(t, func() bool {
		return p.serviceElectionMemberExists(first, firstCtx) && p.serviceElectionMemberExists(second, secondCtx)
	}, "different-parent members")
	await(t, runner.started, "different-parent campaign")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(second) != nil }, "second member activation")

	cancelFirst()
	if err := awaitError(t, firstDone, "first parent cancellation"); err != nil {
		t.Fatalf("first parent cancellation: %v", err)
	}
	if secondCtx.Ctx.Err() != nil || p.findServiceInstance(second) == nil {
		t.Fatal("first member parent canceled the shared campaign or sibling")
	}
	cancelSecond()
	if err := awaitError(t, secondDone, "second parent cancellation"); err != nil {
		t.Fatalf("second parent cancellation: %v", err)
	}
}

func TestDeleteEventuallyFinalizesAfterCleanupRetries(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	p.nodeLabelManager = &testLabeler{removeErrors: []error{
		errors.New("one"), errors.New("two"), errors.New("three"), errors.New("four"), errors.New("five"),
		errors.New("six"), errors.New("seven"), nil,
	}}
	service := electionTestService("service", "10.0.0.1")
	svcCtx := addElectionTestMember(t, p, context.Background(), service)
	await(t, runner.started, "campaign start")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(service) != nil }, "activation")

	err := p.Delete(watch.Event{Type: watch.Deleted, Object: service}, false)
	if err == nil {
		t.Fatalf("Delete() error = %v, want cleanup failure while retry continues", err)
	}
	awaitCondition(t, func() bool {
		_, tracked := p.svcMap.Load(service.UID)
		return !tracked && svcCtx.Ctx.Err() != nil && p.findServiceInstance(service) == nil
	}, "eventual delete finalization")
	if got := testutil.ToFloat64(metrics.ActiveServices.WithLabelValues(service.Namespace)); got != 0 {
		t.Fatalf("active services after eventual cleanup = %v, want 0", got)
	}
	await(t, runner.done, "campaign completion")
}

func TestEmptyCoordinatorRemovedWhileCampaignBackoffPending(t *testing.T) {
	p := newElectionTestProcessor(nil)
	service := electionTestService("backoff-empty", "10.0.0.1")
	ctx := servicecontext.New(context.Background())
	ctx.SignalReadiness()
	p.svcMap.Store(service.UID, ctx)
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcLease := p.leaseMgr.Add(context.Background(), id)
	_, lost := ctx.ReadinessGeneration()
	key := electionMemberKey{claim: "claim", svcCtx: ctx, lost: lost}
	svcLease.Add(key.claim)
	member := &serviceElectionMember{
		key: key, service: service.DeepCopy(), lease: svcLease,
		retired: make(chan struct{}), cleanupAttempted: make(chan struct{}),
	}
	c := &serviceElection{
		p: p, id: id, members: map[electionMemberKey]*serviceElectionMember{key: member},
		changed: make(chan struct{}),
	}
	p.electionsMu.Lock()
	p.elections[id.NamespacedName()] = c
	p.electionsMu.Unlock()
	c.mu.Lock()
	c.scheduleCampaignLocked()
	c.mu.Unlock()

	c.retireMember(member)
	awaitCondition(t, func() bool {
		p.electionsMu.Lock()
		_, exists := p.elections[id.NamespacedName()]
		p.electionsMu.Unlock()
		return !exists
	}, "empty coordinator removal")
}

func TestDeleteRejectsTypedNilService(t *testing.T) {
	p := newElectionTestProcessor(nil)
	var service *v1.Service
	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: service}, false); err == nil {
		t.Fatal("typed nil service was accepted")
	}
}

func TestSharedElectionStaleSameUIDClaimCannotRemoveReplacement(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := electionTestService("service", "10.0.0.1")
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	sharedLease := p.leaseMgr.Add(parent, id)
	sharedLease.Add("sibling")

	oldCtx := servicecontext.New(parent)
	p.svcMap.Store(service.UID, oldCtx)
	oldCtx.SignalReadiness()
	oldDone := make(chan error, 1)
	go func() { oldDone <- p.runServiceElectionMember(oldCtx, service) }()
	await(t, runner.started, "old campaign start")
	oldCtx.ResetReadiness()
	if err := awaitError(t, oldDone, "old member completion"); err != nil {
		t.Fatalf("old member completion: %v", err)
	}

	freshCtx := servicecontext.New(parent)
	p.svcMap.Store(service.UID, freshCtx)
	freshCtx.SignalReadiness()
	freshDone := make(chan error, 1)
	go func() { freshDone <- p.runServiceElectionMember(freshCtx, service) }()
	awaitCondition(t, func() bool { return p.serviceElectionMemberExists(service, freshCtx) }, "replacement registration")
	p.leaseMgr.Delete(id, "sibling", sharedLease)
	if p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("stale same-UID release removed the replacement generation claim")
	}

	freshCtx.Cancel()
	if err := awaitError(t, freshDone, "replacement completion"); err != nil {
		t.Fatalf("replacement completion: %v", err)
	}
	await(t, runner.done, "campaign completion")
	oldCtx.Cancel()
}

func TestSharedElectionRegistrationIsAtomicWithEmptyCancellation(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := electionTestService("first", "10.0.0.1")
	second := electionTestService("second", "10.0.0.2")
	firstCtx := addElectionTestMember(t, p, parent, first)
	await(t, runner.started, "campaign start")

	firstCtx.ResetReadiness()
	secondCtx := addElectionTestMember(t, p, parent, second)
	awaitCondition(t, func() bool { return p.serviceElectionMemberExists(second, secondCtx) }, "second member registration")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(second) != nil }, "second member activation")
	secondCtx.Cancel()
	await(t, runner.done, "campaign completion")
}

func TestSharedElectionReadinessIsMemberLocal(t *testing.T) {
	for _, lostName := range []string{"candidate", "follower"} {
		t.Run(lostName, func(t *testing.T) {
			runner := newElectionTestRunner()
			p := newElectionTestProcessor(runner.run)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			services := []*v1.Service{electionTestService("candidate", "10.0.0.1"), electionTestService("follower", "10.0.0.2")}
			contexts := []*servicecontext.Context{addElectionTestMember(t, p, parent, services[0]), addElectionTestMember(t, p, parent, services[1])}
			await(t, runner.started, "campaign start")
			runner.acquire()
			awaitCondition(t, func() bool {
				return p.findServiceInstance(services[0]) != nil && p.findServiceInstance(services[1]) != nil
			}, "members active")

			lost := 0
			if lostName == "follower" {
				lost = 1
			}
			contexts[lost].ResetReadiness()
			awaitCondition(t, func() bool { return p.findServiceInstance(services[lost]) == nil }, "lost member detached")
			if p.findServiceInstance(services[1-lost]) == nil {
				t.Fatal("sibling was detached")
			}
			contexts[lost].SignalReadiness()
			awaitCondition(t, func() bool { return p.findServiceInstance(services[lost]) != nil }, "lost member restored")
			if runner.count.Load() != 1 {
				t.Fatalf("campaign count = %d, want 1", runner.count.Load())
			}
			contexts[0].Cancel()
			contexts[1].Cancel()
			await(t, runner.done, "campaign completion")
		})
	}
}

func TestSharedElectionFinalCleanupPrecedesCampaignCancel(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := electionTestService("service", "10.0.0.1")
	svcCtx := addElectionTestMember(t, p, parent, service)
	await(t, runner.started, "campaign start")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(service) != nil }, "activation")

	c, member := p.serviceElectionMember(service, svcCtx)
	member.activationWG.Add(1)
	svcCtx.ResetReadiness()
	assertNotSignalled(t, runner.cancelled, "campaign cancelled before workers drained")
	member.activationWG.Done()
	await(t, runner.cancelled, "campaign cancellation")
	await(t, runner.done, "campaign completion")
	awaitCondition(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.campaign == nil
	}, "coordinator campaign retirement")
}

func TestSharedElectionInvoluntaryStopDrainsBeforeRestart(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := electionTestService("service", "10.0.0.1")
	svcCtx := addElectionTestMember(t, p, parent, service)
	await(t, runner.started, "campaign start")
	runner.acquire()
	awaitCondition(t, func() bool { return p.findServiceInstance(service) != nil }, "activation")

	_, member := p.serviceElectionMember(service, svcCtx)
	member.activationWG.Add(1)
	runner.stop()
	assertNotSignalled(t, runner.secondStarted, "successor campaign before cleanup")
	member.activationWG.Done()
	await(t, runner.secondStarted, "successor campaign")
	svcCtx.Cancel()
	runner.stop()
}

func TestSharedVIPPreparedBeforeReadinessAndCampaignCancellation(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	const sharedVIP = "192.0.2.10"
	departingNetwork := &processorTestNetwork{ip: sharedVIP}
	siblingNetwork := &processorTestNetwork{ip: sharedVIP}
	departing := testServiceInstance(t, "departing", []vip.Network{departingNetwork})
	sibling := testServiceInstance(t, "sibling", []vip.Network{siblingNetwork})
	service := departing.ServiceSnapshot
	service.Annotations[kubevip.ServiceLease] = "departing-lease"
	startTestWorkers(t, sibling.Clusters[0], sibling.VIPConfigs[0])
	p.ServiceInstances = append(p.ServiceInstances, sibling)
	p.newInstance = func(_ context.Context, service *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		return departing, nil
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	svcCtx := addElectionTestMember(t, p, parent, service)
	await(t, runner.started, "shared VIP campaign")
	runner.acquire()
	awaitCondition(t, func() bool {
		return departing.Clusters[0].WorkersRunning() && sibling.Clusters[0].WorkersRunning()
	}, "shared VIP workers")
	departingNetwork.resetDeleted()

	start := make(chan struct{})
	var raceWG sync.WaitGroup
	raceWG.Add(2)
	go func() {
		defer raceWG.Done()
		<-start
		svcCtx.ResetReadiness()
	}()
	go func() {
		defer raceWG.Done()
		<-start
		runner.stop()
	}()
	close(start)
	raceWG.Wait()
	await(t, runner.done, "shared VIP campaign stop")
	if departingNetwork.wasDeleted() {
		t.Fatal("departing service deleted a VIP still owned by its active sibling")
	}
	svcCtx.Cancel()
	sibling.Clusters[0].StopWorkersAndWaitPreserving(nil)
}

func TestSharedVIPCampaignStopDeletesFinalAddressOnce(t *testing.T) {
	const sharedVIP = "192.0.2.10"
	firstNetwork := &processorTestNetwork{ip: sharedVIP}
	secondNetwork := &processorTestNetwork{ip: sharedVIP}
	first := testServiceInstance(t, "first", []vip.Network{firstNetwork})
	second := testServiceInstance(t, "second", []vip.Network{secondNetwork})
	p := testCleanupProcessor(first, second)
	startTestWorkers(t, first.Clusters[0], first.VIPConfigs[0])
	startTestWorkers(t, second.Clusters[0], second.VIPConfigs[0])
	firstNetwork.resetDeleted()
	secondNetwork.resetDeleted()

	stopping := map[*instance.Instance]struct{}{first: {}, second: {}}
	forceDeletes := p.prepareServiceInstancesStop([]*instance.Instance{first, second})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = p.deleteServiceInstanceExcluding(context.Background(), first, stopping, forceDeletes[first])
	}()
	go func() {
		defer wg.Done()
		_ = p.deleteServiceInstanceExcluding(context.Background(), second, stopping, forceDeletes[second])
	}()
	wg.Wait()

	if got := firstNetwork.deleteCount() + secondNetwork.deleteCount(); got != 1 {
		t.Fatalf("shared VIP DeleteIP calls = %d, want 1", got)
	}
}

func TestCampaignStopAddressOwnership(t *testing.T) {
	const sharedVIP = "192.0.2.10"
	for _, test := range []struct {
		name                 string
		firstVIP             string
		secondVIP            string
		firstLeadershipLost  bool
		secondLeadershipLost bool
		wantDeletes          int
	}{
		{
			name:                 "shared VIP with removing and leadership-lost owners",
			firstVIP:             sharedVIP,
			secondVIP:            sharedVIP,
			secondLeadershipLost: true,
		},
		{
			name:        "shared VIP with both owners removing",
			firstVIP:    sharedVIP,
			secondVIP:   sharedVIP,
			wantDeletes: 1,
		},
		{
			name:                 "unique VIP with removing owner",
			firstVIP:             "192.0.2.20",
			secondVIP:            "192.0.2.30",
			secondLeadershipLost: true,
			wantDeletes:          1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstNetwork := &processorTestNetwork{ip: test.firstVIP}
			secondNetwork := &processorTestNetwork{ip: test.secondVIP}
			first := testServiceInstance(t, "first", []vip.Network{firstNetwork})
			second := testServiceInstance(t, "second", []vip.Network{secondNetwork})
			p := testCleanupProcessor(first, second)
			p.config.PreserveVIPOnLeadershipLoss = true
			startTestWorkers(t, first.Clusters[0], first.VIPConfigs[0])
			startTestWorkers(t, second.Clusters[0], second.VIPConfigs[0])
			firstNetwork.resetDeleted()
			secondNetwork.resetDeleted()

			instances := []*instance.Instance{first, second}
			stopping := map[*instance.Instance]struct{}{first: {}, second: {}}
			leadershipLost := map[*instance.Instance]bool{
				first:  test.firstLeadershipLost,
				second: test.secondLeadershipLost,
			}
			forceDeletes := p.prepareServiceInstancesCampaignStop(instances, leadershipLost)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_ = p.deleteServiceInstanceWithMode(context.Background(), first, stopping, test.firstLeadershipLost, forceDeletes[first])
			}()
			go func() {
				defer wg.Done()
				_ = p.deleteServiceInstanceWithMode(context.Background(), second, stopping, test.secondLeadershipLost, forceDeletes[second])
			}()
			wg.Wait()

			if got := firstNetwork.deleteCount() + secondNetwork.deleteCount(); got != test.wantDeletes {
				t.Fatalf("DeleteIP calls = %d, want %d", got, test.wantDeletes)
			}
		})
	}
}

func TestCampaignLeadershipLossPreservesOnlyIPv4(t *testing.T) {
	for _, test := range []struct {
		name        string
		address     string
		wantDeleted bool
	}{
		{name: "IPv4", address: "192.0.2.10"},
		{name: "IPv6", address: "2001:db8::10", wantDeleted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			network := &processorTestNetwork{ip: test.address}
			inst := testServiceInstance(t, "campaign-preserve-"+test.name, []vip.Network{network})
			p := testCleanupProcessor(inst)
			p.config.PreserveVIPOnLeadershipLoss = true
			startTestWorkers(t, inst.Clusters[0], inst.VIPConfigs[0])
			network.resetDeleted()

			stopping := map[*instance.Instance]struct{}{inst: {}}
			forceDeletes := p.prepareServiceInstancesStop([]*instance.Instance{inst}, true)
			if err := p.deleteServiceInstanceWithMode(context.Background(), inst, stopping, true, forceDeletes[inst]); err != nil {
				t.Fatalf("leadership-loss cleanup: %v", err)
			}
			if got := network.wasDeleted(); got != test.wantDeleted {
				t.Fatalf("DeleteIP called = %v, want %v", got, test.wantDeleted)
			}
		})
	}
}

func TestElectionCampaignStopPreservesIPv4VIP(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	p.config.PreserveVIPOnLeadershipLoss = true
	service := electionTestService("campaign-stop-preserve", "192.0.2.10")
	network := &processorTestNetwork{ip: service.Spec.LoadBalancerIP}
	inst := testServiceInstance(t, string(service.UID), []vip.Network{network})
	inst.ServiceSnapshot = service.DeepCopy()
	p.newInstance = func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error) {
		return inst, nil
	}
	svcCtx := addElectionTestMember(t, p, context.Background(), service)
	await(t, runner.started, "preserve campaign")
	runner.acquire()
	awaitCondition(t, func() bool {
		unlock := p.lockService(service.UID)
		defer unlock()
		return inst.Clusters[0].WorkersRunning() && inst.LabelAdded
	}, "completed preserve campaign activation")
	network.resetDeleted()

	runner.stop()
	await(t, runner.secondStarted, "successor campaign after preserved cleanup")
	if network.wasDeleted() {
		t.Fatal("involuntary campaign stop deleted preserved IPv4 VIP")
	}
	svcCtx.Cancel()
}

func TestEndpointLossIgnoresLeadershipVIPPreserve(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	p.config.PreserveVIPOnLeadershipLoss = true
	service := electionTestService("endpoint-loss-delete", "192.0.2.10")
	network := &processorTestNetwork{ip: service.Spec.LoadBalancerIP}
	inst := testServiceInstance(t, string(service.UID), []vip.Network{network})
	inst.ServiceSnapshot = service.DeepCopy()
	p.newInstance = func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error) {
		return inst, nil
	}
	svcCtx := addElectionTestMember(t, p, context.Background(), service)
	await(t, runner.started, "endpoint-loss campaign")
	runner.acquire()
	awaitCondition(t, func() bool {
		unlock := p.lockService(service.UID)
		defer unlock()
		return inst.Clusters[0].WorkersRunning() && inst.LabelAdded
	}, "completed endpoint-loss activation")
	network.resetDeleted()

	svcCtx.ResetReadiness()
	awaitCondition(t, func() bool { return p.findServiceInstance(service) == nil }, "endpoint-loss cleanup")
	if !network.wasDeleted() {
		t.Fatal("endpoint loss preserved IPv4 VIP")
	}
	svcCtx.Cancel()
}

func TestExplicitServiceDeleteIgnoresLeadershipPreserve(t *testing.T) {
	network := &processorTestNetwork{ip: "192.0.2.10"}
	inst := testServiceInstance(t, "explicit-delete", []vip.Network{network})
	p := testCleanupProcessor(inst)
	p.config.PreserveVIPOnLeadershipLoss = true
	startTestWorkers(t, inst.Clusters[0], inst.VIPConfigs[0])
	network.resetDeleted()

	if err := p.deleteService(context.Background(), inst.UID()); err != nil {
		t.Fatalf("explicit cleanup: %v", err)
	}
	if !network.wasDeleted() {
		t.Fatal("explicit Service deletion preserved IPv4 VIP")
	}
}

func TestSyncServicesStaleGenerationDoesNotDeleteCurrentInstance(t *testing.T) {
	p := newElectionTestProcessor(nil)
	service := electionTestService("service", "10.0.0.1")
	service.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: "10.0.0.2"}}
	currentCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, currentCtx)
	current := &instance.Instance{ServiceSnapshot: service.DeepCopy(), AddCalled: true}
	p.ServiceInstances = []*instance.Instance{current}
	staleCtx := servicecontext.New(context.Background())
	lost := make(chan any)
	close(lost)

	if err := p.syncServices(staleCtx.Ctx, staleCtx, service, nil, true, &serviceExpectation{svcCtx: staleCtx, lost: lost}); err != nil {
		t.Fatalf("syncServices: %v", err)
	}
	if p.findServiceInstance(service) != current {
		t.Fatal("stale generation deleted current instance")
	}
}

func TestActivationStoppedAtLockedCheckCreatesNothing(t *testing.T) {
	p := newElectionTestProcessor(nil)
	service := electionTestService("service", "10.0.0.1")
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	lost := make(chan any)
	var made atomic.Int64
	p.newInstance = func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error) {
		made.Add(1)
		return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
	}

	err := p.addService(context.Background(), service, &sync.WaitGroup{}, &serviceExpectation{
		svcCtx: svcCtx,
		lost:   lost,
		valid:  func() bool { return false },
	})
	if err != nil {
		t.Fatalf("addService: %v", err)
	}
	if made.Load() != 0 || p.findServiceInstance(service) != nil {
		t.Fatal("stale activation created an instance")
	}
}

func TestStatusOnlyUpdateDoesNotSuppressActivation(t *testing.T) {
	p := newElectionTestProcessor(nil)
	service := electionTestService("status-activation", "192.0.2.10")
	service.ResourceVersion = "10"
	version := p.recordDesiredEvent(watch.Modified, service)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	lost := make(chan any)
	started := make(chan struct{})
	release := make(chan struct{})
	p.newInstance = func(_ context.Context, service *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
		close(started)
		<-release
		return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
	}
	expected := &serviceExpectation{
		svcCtx: svcCtx, lost: lost, version: version, lifecycle: serviceLifecycleFor(service),
		valid: func() bool { return true },
	}
	done := make(chan error, 1)
	go func() { done <- p.addService(context.Background(), service, &sync.WaitGroup{}, expected) }()
	await(t, started, "activation construction")
	status := service.DeepCopy()
	status.ResourceVersion = "11"
	status.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: service.Spec.LoadBalancerIP}}
	if got := p.recordDesiredEvent(watch.Modified, status); got != version {
		t.Fatalf("status update lifecycle version = %d, want %d", got, version)
	}
	close(release)
	if err := awaitError(t, done, "status-only activation"); err != nil {
		t.Fatalf("activation after status-only update: %v", err)
	}
	if inst := p.findServiceInstance(service); inst == nil || !inst.AddCalled {
		t.Fatal("status-only update suppressed activation")
	}
}

func TestFailedActivationRollbackAllowsFreshInstance(t *testing.T) {
	p := newElectionTestProcessor(nil)
	service := electionTestService("service", "10.0.0.1")
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	lost := make(chan any)
	var made atomic.Int64
	p.newInstance = func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error) {
		return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
	}
	p.nodeLabelManager = &testLabeler{addErrors: []error{errors.New("fail once"), nil}}
	expected := &serviceExpectation{svcCtx: svcCtx, lost: lost, valid: func() bool { return true }}
	if err := p.addService(context.Background(), service, &sync.WaitGroup{}, expected); err == nil {
		t.Fatal("first activation succeeded")
	}
	if p.findServiceInstance(service) != nil {
		t.Fatal("failed activation remained tracked")
	}
	p.newInstance = func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error) {
		made.Add(1)
		return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
	}
	if err := p.addService(context.Background(), service, &sync.WaitGroup{}, expected); err != nil {
		t.Fatalf("fresh activation: %v", err)
	}
	if made.Load() != 1 || !p.findServiceInstance(service).AddCalled {
		t.Fatal("fresh activation did not replace partial AddCalled state")
	}
}

func TestOwnedInstanceAddressesIgnoreDHCPPlaceholders(t *testing.T) {
	service := electionTestService("service", "0.0.0.0")
	service.Spec.ClusterIPs = []string{"::"}
	if addresses := ownedInstanceAddresses(&instance.Instance{ServiceSnapshot: service}); len(addresses) != 0 {
		t.Fatalf("placeholder addresses = %v, want none", addresses)
	}
}

type electionTestRunner struct {
	mu            sync.Mutex
	callbacks     []*election.RunConfig
	started       chan struct{}
	secondStarted chan struct{}
	cancelled     chan struct{}
	done          chan struct{}
	acquireCh     chan struct{}
	stopCh        chan struct{}
	count         atomic.Int64
}

func newElectionTestRunner() *electionTestRunner {
	return &electionTestRunner{started: make(chan struct{}), secondStarted: make(chan struct{}), cancelled: make(chan struct{}), done: make(chan struct{}), acquireCh: make(chan struct{}, 2), stopCh: make(chan struct{}, 2)}
}

func (r *electionTestRunner) run(ctx context.Context, run *election.RunConfig, _ *kubevip.Config) error {
	n := r.count.Add(1)
	r.mu.Lock()
	r.callbacks = append(r.callbacks, run)
	r.mu.Unlock()
	switch n {
	case 1:
		close(r.started)
	case 2:
		close(r.secondStarted)
	}
	select {
	case <-ctx.Done():
		if n == 1 {
			close(r.cancelled)
		}
		closeIfOpen(r.done)
		return nil
	case <-r.acquireCh:
		run.OnStartedLeading(ctx)
	}
	select {
	case <-ctx.Done():
		if n == 1 {
			closeIfOpen(r.cancelled)
		}
	case <-r.stopCh:
	}
	run.OnStoppedLeading()
	closeIfOpen(r.done)
	return nil
}

func (r *electionTestRunner) acquire() { r.acquireCh <- struct{}{} }
func (r *electionTestRunner) stop()    { r.stopCh <- struct{}{} }

func newElectionTestProcessor(run func(context.Context, *election.RunConfig, *kubevip.Config) error) *Processor {
	return &Processor{
		config: &kubevip.Config{EnableServicesElection: true, DisableServiceUpdates: true},
		lbClassFilter: func(*v1.Service, *kubevip.Config) bool {
			return false
		},
		leaseMgr: lease.NewManager(), nodeLabelManager: noop.NewManager(), elections: make(map[string]*serviceElection), electionRun: run,
		newInstance: func(_ context.Context, service *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
			return &instance.Instance{ServiceSnapshot: service.DeepCopy()}, nil
		},
	}
}

func electionTestService(name, address string) *v1.Service {
	return &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name), Annotations: map[string]string{kubevip.ServiceLease: "shared"}}, Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: address}}
}

func addElectionTestMember(t *testing.T, p *Processor, parent context.Context, service *v1.Service) *servicecontext.Context {
	t.Helper()
	namespace, name := lease.ServiceName(service)
	p.leaseMgr.Add(parent, lease.NewID(p.config.LeaderElectionType, namespace, name))
	svcCtx := servicecontext.New(parent)
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	go func() {
		if err := p.startLeaderElection(parent, svcCtx, service, nil); err != nil && parent.Err() == nil {
			t.Errorf("startLeaderElection: %v", err)
		}
	}()
	awaitCondition(t, func() bool { return p.serviceElectionMemberExists(service, svcCtx) }, "member registration")
	return svcCtx
}

func await(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func awaitError(t *testing.T, ch <-chan error, description string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
		return nil
	}
}

func assertNotSignalled(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(description)
	case <-time.After(50 * time.Millisecond):
	}
}

func awaitCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func closeIfOpen(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}
