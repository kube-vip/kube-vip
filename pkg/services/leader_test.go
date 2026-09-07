package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/prometheus/client_golang/prometheus/testutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func resetServiceReadiness(t *testing.T, svcCtx *servicecontext.Context) {
	t.Helper()
	generation := svcCtx.CurrentReadiness()
	if !svcCtx.ResetReadinessGeneration(generation) {
		t.Fatal("Service readiness generation was not reset")
	}
}

type immediateRestartScheduler struct{}

func (immediateRestartScheduler) ScheduleRestart(_ context.Context, _ time.Duration,
	_ *sync.WaitGroup, restart func()) {
	restart()
}

func TestStartServicesLeaderElectionTracksSharedMembersAcrossReadinessLoss(t *testing.T) {
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{EnableServicesElection: true},
		leaseMgr:    lease.NewManager(),
	}
	initializeTestElectionCoordinators(p)
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	firstService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", UID: types.UID("first"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	secondService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "second", Namespace: "default", UID: types.UID("second"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"}}
	namespace, name := lease.ServiceName(firstService)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	externalToken := lease.ObjectName(id, "external")
	sharedLease, added := p.leaseMgr.Acquire(context.Background(), id, externalToken, nil)
	if !added {
		t.Fatal("external election participant was not registered")
	}
	externalParticipation := sharedLease.JoinElection()
	externalElection := externalParticipation.Session
	t.Cleanup(func() {
		externalElection.Stopped()
		p.leaseMgr.Delete(id, externalToken, sharedLease)
	})
	if !externalParticipation.RunsCampaign() || !externalElection.Started() {
		t.Fatal("external election did not become leader")
	}

	firstCtx := servicecontext.New(context.Background())
	secondCtx := servicecontext.New(context.Background())
	p.svcMap.Store(firstService.UID, firstCtx)
	p.svcMap.Store(secondService.UID, secondCtx)

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	var wg sync.WaitGroup
	go func() { firstDone <- p.StartServicesLeaderElection(firstCtx, firstService, &wg) }()
	go func() { secondDone <- p.StartServicesLeaderElection(secondCtx, secondService, &wg) }()

	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()
	waitForLeaseVIPCount(t, p.leaseMgr, id, 2)

	resetServiceReadiness(t, firstCtx)
	waitForLeaseVIPCount(t, p.leaseMgr, id, 1)
	if !externalElection.IsLeading() || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("one shared member losing readiness ended the healthy sibling campaign")
	}

	firstCtx.SignalReadiness()
	waitForLeaseVIPCount(t, p.leaseMgr, id, 2)

	secondCtx.Cancel()
	waitForLeaseVIPCount(t, p.leaseMgr, id, 1)
	if firstCtx.Ctx.Err() != nil || !externalElection.IsLeading() || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("deleting one shared member ended the healthy sibling campaign")
	}

	firstCtx.Cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first member returned error: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second member returned error: %v", err)
	}
	externalElection.Stopped()
	p.leaseMgr.Delete(id, externalToken, sharedLease)
	if p.leaseMgr.Get(id) != nil {
		t.Fatal("final shared member withdrawal did not retire the lease")
	}
}

func TestServiceMemberLeavingDoesNotCancelControlPlaneLease(t *testing.T) {
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{}, leaseMgr: lease.NewManager(),
	}
	initializeTestElectionCoordinators(p)
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	controlPlaneToken := lease.ObjectName(id, "cp")
	sharedLease, _ := p.leaseMgr.Acquire(context.Background(), id, controlPlaneToken, nil)
	controlPlaneParticipation := sharedLease.JoinElection()
	controlPlaneElection := controlPlaneParticipation.Session
	if !controlPlaneParticipation.RunsCampaign() {
		t.Fatal("control-plane election did not start")
	}
	if !controlPlaneElection.Started() {
		t.Fatal("control-plane election did not become leader")
	}

	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil) }()
	svcCtx.SignalReadiness()
	waitForLeaseVIPCount(t, p.leaseMgr, id, 1)
	svcCtx.Cancel()
	if err := <-done; err != nil {
		t.Fatalf("service election returned error: %v", err)
	}

	if sharedLease.Ctx.Err() != nil || !controlPlaneElection.IsLeading() || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("leaving Service member cancelled the control-plane lease")
	}
	p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
}

func TestDelayedElectionActivationDoesNotRestoreDeletedService(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	metrics.ServiceElectionErrorsTotal.DeleteLabelValues(service.Namespace, service.Name, "service_sync")
	t.Cleanup(func() {
		metrics.ServiceElectionErrorsTotal.DeleteLabelValues(service.Namespace, service.Name, "service_sync")
	})
	labeler := &testLabeler{}
	var factoryCalls atomic.Int32
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config: &kubevip.Config{
			DisableServiceUpdates:  true,
			EnableServicesElection: true,
		},
		leaseMgr:         lease.NewManager(),
		nodeLabelManager: labeler,
		instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, svc *v1.Service,
			_ *sync.WaitGroup) (*instance.Instance, error) {
			factoryCalls.Add(1)
			return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
		}),
	}

	activationStarted := make(chan struct{})
	continueActivation := make(chan struct{})
	activationFinished := make(chan struct{})
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{})}
	var releaseActivation sync.Once
	t.Cleanup(func() {
		releaseActivation.Do(func() { close(continueActivation) })
	})
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner), withTestElectionActivation(
		func(ctx context.Context, svc *v1.Service, svcCtx *servicecontext.Context, wg *sync.WaitGroup) error {
			close(activationStarted)
			<-continueActivation
			defer close(activationFinished)
			return p.activateElectedService(ctx, svcCtx, svc, wg)
		},
	))

	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	electionDone := make(chan error, 1)
	go func() {
		electionDone <- p.StartServicesLeaderElection(svcCtx, service, nil)
	}()
	svcCtx.SignalReadiness()

	select {
	case <-activationStarted:
	case <-time.After(time.Second):
		t.Fatal("Service activation did not reach the delayed datapath call")
	}
	sharedLease := p.leaseMgr.Get(id)
	if sharedLease == nil {
		t.Fatal("Service-owned election did not acquire its lease")
	}
	observerParticipation := sharedLease.JoinElection()
	if observerParticipation.RunsCampaign() || !observerParticipation.Session.IsLeading() {
		t.Fatal("Service-owned election did not become leader")
	}
	controlPlaneToken := lease.ObjectName(id, "cp")
	if claimed, _ := p.leaseMgr.ClaimWithVIPProvider(id, controlPlaneToken, nil); claimed != sharedLease {
		t.Fatal("control plane did not join the Service-owned lease")
	}
	t.Cleanup(func() {
		p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
	})

	if err := p.deleteTrackedService(service); err != nil {
		t.Fatalf("deleteTrackedService() error = %v", err)
	}
	if sharedLease.Ctx.Err() != nil || !observerParticipation.Session.IsLeading() {
		t.Fatal("deleting Service stopped the shared control-plane election")
	}

	releaseActivation.Do(func() { close(continueActivation) })
	select {
	case <-activationFinished:
	case <-time.After(time.Second):
		t.Fatal("delayed Service activation did not finish")
	}
	select {
	case err := <-electionDone:
		if err != nil {
			t.Fatalf("Service election returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Service election did not stop after deletion")
	}

	if got := p.findServiceInstance(service); got != nil {
		t.Error("delayed activation restored a deleted Service instance")
	}
	if got := factoryCalls.Load(); got != 0 {
		t.Errorf("instance factory calls after deletion = %d, want 0", got)
	}
	if got := labeler.addCalls; got != 0 {
		t.Errorf("node label additions after deletion = %d, want 0", got)
	}
	if got := p.OwnedServiceVIPs(); len(got) != 0 {
		t.Errorf("owned Service VIPs after deletion = %v, want none", got)
	}
	if sharedLease.Ctx.Err() != nil || !observerParticipation.Session.IsLeading() || p.leaseMgr.Get(id) != sharedLease {
		t.Error("delayed activation stopped the shared control-plane campaign")
	}
	select {
	case <-runner.stopping:
		t.Error("delayed activation stopped the Service-owned campaign")
	default:
	}
	if got := testutil.ToFloat64(metrics.ServiceElectionErrorsTotal.WithLabelValues(
		service.Namespace, service.Name, "service_sync")); got != 0 {
		t.Errorf("Service election errors after stale activation = %v, want 0", got)
	}
}

func TestStartServicesLeaderElectionRejectsNilContext(t *testing.T) {
	p := &Processor{serviceLock: newTestServiceLocks()}
	initializeTestElectionCoordinators(p)
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", UID: types.UID("service")}}
	if err := p.StartServicesLeaderElection(nil, service, nil); err == nil {
		t.Fatal("nil service context started leader election")
	}
}

func TestStartServicesLeaderElectionStaleContextReturnsPromptly(t *testing.T) {
	p := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	initializeTestElectionCoordinators(p)
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: "default", UID: types.UID("stale")}}
	staleContext := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, servicecontext.New(context.Background()))

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(staleContext, service, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stale service context returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("stale service context did not return promptly")
	}
}

func TestStartServicesLeaderElectionRejectsTypedNilService(t *testing.T) {
	p := &Processor{serviceLock: newTestServiceLocks()}
	initializeTestElectionCoordinators(p)
	var service *v1.Service
	if err := p.StartServicesLeaderElection(servicecontext.New(context.Background()), service, nil); err == nil {
		t.Fatal("typed-nil service started leader election")
	}
}

func TestStartServicesLeaderElectionDoesNotRegisterCancelledContext(t *testing.T) {
	p := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	initializeTestElectionCoordinators(p)
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "cancelled", Namespace: "default", UID: types.UID("cancelled")}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.Cancel()

	if err := p.StartServicesLeaderElection(svcCtx, service, nil); err == nil {
		t.Fatal("cancelled service context started leader election")
	}
	namespace, name := lease.ServiceName(service)
	if p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, namespace, name)) != nil {
		t.Fatal("cancelled service context registered a lease")
	}
}

func TestStartServicesLeaderElectionRegistersOneMemberForConcurrentCalls(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{})}
	p := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "concurrent", Namespace: "default", UID: types.UID("concurrent")}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	const callers = 32
	start := make(chan struct{})
	errors := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			errors <- p.StartServicesLeaderElection(svcCtx, service, nil)
		}()
	}
	close(start)
	waitForElectionRunner(t, runner.started)
	if got := runner.starts.Load(); got != 1 {
		t.Fatalf("campaign starts = %d, want 1", got)
	}

	for range callers - 1 {
		if err := <-errors; err != nil {
			t.Fatalf("duplicate StartServicesLeaderElection() error = %v", err)
		}
	}
	svcCtx.Cancel()
	if err := <-errors; err != nil {
		t.Fatalf("owner StartServicesLeaderElection() error = %v", err)
	}
}

func TestStartServicesLeaderElectionRecreatesCancelledLease(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{})}
	p := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "recreate", Namespace: "default", UID: types.UID("recreate")}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	oldLease, added := p.leaseMgr.Acquire(context.Background(), id, lease.ServiceNamespacedName(service), nil)
	if !added {
		t.Fatal("old Service participant was not registered")
	}
	oldLease.Cancel()
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil) }()
	waitForElectionRunner(t, runner.started)
	if currentLease := p.leaseMgr.Get(id); currentLease == nil || currentLease == oldLease || currentLease.Ctx.Err() != nil {
		t.Fatal("cancelled lease was not replaced for the live service")
	}
	svcCtx.Cancel()
	if err := <-done; err != nil {
		t.Fatalf("StartServicesLeaderElection() error = %v", err)
	}
}

func TestStartServicesLeaderElectionRestartsAfterLeaseLoss(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{})}
	p := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: "default", UID: types.UID("restart")}}
	metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	defer metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil) }()
	waitForElectionRunner(t, runner.started)
	p.leaseMgr.Get(id).Cancel()
	waitForCondition(t, func() bool { return runner.starts.Load() == 2 }, "replacement campaign after lease loss")
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 2 {
		t.Fatalf("election attempts after lease loss = %v, want 2", got)
	}
	if currentLease := p.leaseMgr.Get(id); currentLease == nil || currentLease.Ctx.Err() != nil {
		t.Fatal("live service did not recreate its lease after loss")
	}
	svcCtx.Cancel()
	if err := <-done; err != nil {
		t.Fatalf("StartServicesLeaderElection() error = %v", err)
	}
}

func TestServiceWatcherWaitGroupDoesNotOwnCampaignShutdown(t *testing.T) {
	releaseStop := make(chan struct{})
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{}), releaseStop: releaseStop}
	p := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "shutdown", Namespace: "default", UID: types.UID("shutdown"),
	}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	var wg sync.WaitGroup
	startDone := make(chan error, 1)
	go func() {
		startDone <- p.StartServicesLeaderElection(svcCtx, service, &wg)
	}()
	waitForElectionRunner(t, runner.started)
	svcCtx.Cancel()
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("StartServicesLeaderElection() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Service election watcher did not stop after cancellation")
	}
	waitForElectionRunner(t, runner.stopping)

	waitStarted := make(chan struct{})
	waitDone := make(chan struct{})
	go func() {
		close(waitStarted)
		wg.Wait()
		close(waitDone)
	}()
	<-waitStarted
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("Service watcher WaitGroup remained blocked by campaign shutdown")
	}

	close(releaseStop)
	p.electionCoordinators.Wait()
}

func TestServiceElectionAttemptWaitsForReadiness(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{})}
	p := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "readiness", Namespace: "default", UID: types.UID("readiness")}}
	metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	defer metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil) }()
	select {
	case <-runner.started:
		t.Fatal("campaign started before endpoint readiness")
	case <-time.After(25 * time.Millisecond):
	}
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 0 {
		t.Fatalf("election attempts before readiness = %v, want 0", got)
	}

	svcCtx.SignalReadiness()
	waitForElectionRunner(t, runner.started)
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 1 {
		t.Fatalf("election attempts after readiness = %v, want 1", got)
	}
	svcCtx.Cancel()
	if err := <-done; err != nil {
		t.Fatalf("StartServicesLeaderElection() error = %v", err)
	}
}

func TestSharedElectionDrainsBeforeRestartAfterAllMembersLoseReadiness(t *testing.T) {
	releaseStop := make(chan struct{})
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{}), releaseStop: releaseStop}
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{}, leaseMgr: lease.NewManager(),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner),
		withTestRestartScheduler(immediateRestartScheduler{}))
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	firstService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", UID: types.UID("first"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	secondService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "second", Namespace: "default", UID: types.UID("second"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"}}
	firstCtx := servicecontext.New(context.Background())
	secondCtx := servicecontext.New(context.Background())
	p.svcMap.Store(firstService.UID, firstCtx)
	p.svcMap.Store(secondService.UID, secondCtx)
	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- p.StartServicesLeaderElection(firstCtx, firstService, nil) }()
	go func() { secondDone <- p.StartServicesLeaderElection(secondCtx, secondService, nil) }()
	waitForElectionRunner(t, runner.started)
	namespace, name := lease.ServiceName(firstService)
	waitForLeaseVIPCount(t, p.leaseMgr, lease.NewID(p.config.LeaderElectionType, namespace, name), 2)

	resetServiceReadiness(t, firstCtx)
	resetServiceReadiness(t, secondCtx)
	waitForElectionRunner(t, runner.stopping)
	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()
	if runner.starts.Load() != 1 {
		t.Fatalf("replacement campaign started before old campaign drained: starts = %d", runner.starts.Load())
	}

	close(releaseStop)
	waitForCondition(t, func() bool { return runner.starts.Load() == 2 }, "replacement campaign after old campaign drain")
	firstCtx.Cancel()
	secondCtx.Cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first service election error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second service election error = %v", err)
	}
}

func TestSharedElectionReplacesCampaignAfterServiceGenerationAndControlPlaneDeparture(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{})}
	var activations atomic.Int64
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner),
		withTestRestartScheduler(immediateRestartScheduler{}),
		withTestElectionActivation(func(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error {
			activations.Add(1)
			return nil
		}),
	)
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	controlPlaneToken := lease.ObjectName(id, "cp")
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	var sharedLease *lease.Lease
	t.Cleanup(func() {
		svcCtx.Cancel()
		if sharedLease != nil {
			p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
		}
		p.electionCoordinators.Wait()
	})

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil) }()
	waitForElectionRunner(t, runner.started)
	waitForCondition(t, func() bool { return activations.Load() == 1 }, "initial Service activation")
	sharedLease = p.leaseMgr.Get(id)
	if sharedLease == nil {
		t.Fatal("Service-owned campaign did not acquire its lease")
	}
	if claimed, _ := p.leaseMgr.ClaimWithVIPProvider(id, controlPlaneToken, nil); claimed != sharedLease {
		t.Fatal("control plane did not join the Service-owned lease")
	}

	resetServiceReadiness(t, svcCtx)
	waitForLeaseVIPCount(t, p.leaseMgr, id, 0)
	p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
	waitForElectionRunner(t, runner.stopping)
	svcCtx.SignalReadiness()
	waitForCondition(t, func() bool { return runner.starts.Load() == 2 }, "replacement campaign after shared lease loss")
	waitForCondition(t, func() bool { return activations.Load() == 2 }, "replacement Service activation")
	if got := activations.Load(); got != 2 {
		t.Fatalf("service activations = %d, want 2", got)
	}
	if got := runner.starts.Load(); got != 2 {
		t.Fatalf("campaign starts = %d, want 2", got)
	}

	svcCtx.Cancel()
	if err := <-done; err != nil {
		t.Fatalf("Service election watcher returned an error: %v", err)
	}
	p.electionCoordinators.Wait()
	if got := runner.starts.Load(); got != 2 {
		t.Fatalf("campaign starts after shutdown = %d, want 2", got)
	}
}

func TestSharedElectionDeletedCandidateNeverActivates(t *testing.T) {
	releaseLeading := make(chan struct{})
	runner := &electionTestRunner{started: make(chan struct{}), releaseLeading: releaseLeading}
	var syncMutex sync.Mutex
	syncCalls := map[types.UID]int{}
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{}, leaseMgr: lease.NewManager(),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner), withTestElectionActivation(func(_ context.Context, service *v1.Service,
		_ *servicecontext.Context, _ *sync.WaitGroup) error {
		syncMutex.Lock()
		defer syncMutex.Unlock()
		syncCalls[service.UID]++
		return nil
	}))
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	candidateService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "candidate", Namespace: "default", UID: types.UID("candidate"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	siblingService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "sibling", Namespace: "default", UID: types.UID("sibling"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"}}
	candidateCtx := servicecontext.New(context.Background())
	siblingCtx := servicecontext.New(context.Background())
	p.svcMap.Store(candidateService.UID, candidateCtx)
	p.svcMap.Store(siblingService.UID, siblingCtx)
	candidateCtx.SignalReadiness()
	siblingCtx.SignalReadiness()

	candidateDone := make(chan error, 1)
	siblingDone := make(chan error, 1)
	go func() { candidateDone <- p.StartServicesLeaderElection(candidateCtx, candidateService, nil) }()
	go func() { siblingDone <- p.StartServicesLeaderElection(siblingCtx, siblingService, nil) }()
	waitForElectionRunner(t, runner.started)
	namespace, name := lease.ServiceName(candidateService)
	waitForLeaseVIPCount(t, p.leaseMgr, lease.NewID(p.config.LeaderElectionType, namespace, name), 2)

	candidateCtx.Cancel()
	if err := <-candidateDone; err != nil {
		t.Fatalf("candidate service election error = %v", err)
	}
	close(releaseLeading)
	waitForCondition(t, func() bool {
		syncMutex.Lock()
		defer syncMutex.Unlock()
		return syncCalls[siblingService.UID] == 1
	}, "live sibling activation")

	syncMutex.Lock()
	candidateSyncs := syncCalls[candidateService.UID]
	siblingSyncs := syncCalls[siblingService.UID]
	syncMutex.Unlock()
	if candidateSyncs != 0 {
		t.Fatalf("deleted candidate synchronized %d times, want 0", candidateSyncs)
	}
	if siblingSyncs != 1 {
		t.Fatalf("live sibling synchronized %d times, want 1", siblingSyncs)
	}
	siblingCtx.Cancel()
	if err := <-siblingDone; err != nil {
		t.Fatalf("sibling service election error = %v", err)
	}
}

func TestSharedElectionReadinessIsMemberLocal(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{})}
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{}, leaseMgr: lease.NewManager(),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	firstService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", UID: types.UID("first"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	secondService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "second", Namespace: "default", UID: types.UID("second"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"}}
	firstCtx := servicecontext.New(context.Background())
	secondCtx := servicecontext.New(context.Background())
	p.svcMap.Store(firstService.UID, firstCtx)
	p.svcMap.Store(secondService.UID, secondCtx)
	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- p.StartServicesLeaderElection(firstCtx, firstService, nil) }()
	go func() { secondDone <- p.StartServicesLeaderElection(secondCtx, secondService, nil) }()
	waitForElectionRunner(t, runner.started)
	namespace, name := lease.ServiceName(firstService)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	waitForLeaseVIPCount(t, p.leaseMgr, id, 2)

	resetServiceReadiness(t, firstCtx)
	waitForLeaseVIPCount(t, p.leaseMgr, id, 1)
	if runner.starts.Load() != 1 {
		t.Fatalf("campaign starts after one member lost readiness = %d, want 1", runner.starts.Load())
	}
	firstCtx.SignalReadiness()
	waitForLeaseVIPCount(t, p.leaseMgr, id, 2)
	if runner.starts.Load() != 1 {
		t.Fatalf("campaign starts after one member recovered readiness = %d, want 1", runner.starts.Load())
	}

	firstCtx.Cancel()
	secondCtx.Cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first service election error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second service election error = %v", err)
	}
}

func TestSharedElectionRetriesFailedMemberWithoutRestartingCampaign(t *testing.T) {
	releaseLeading := make(chan struct{})
	var releaseLeadingOnce sync.Once
	releaseLeadership := func() {
		releaseLeadingOnce.Do(func() { close(releaseLeading) })
	}
	runner := &electionTestRunner{started: make(chan struct{}), releaseLeading: releaseLeading}
	var activationMutex sync.Mutex
	activationCalls := make(map[types.UID]int)
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
	}
	initializeTestElectionCoordinators(p,
		withTestCampaignRunner(runner),
		withTestRestartScheduler(immediateRestartScheduler{}),
		withTestElectionActivation(func(_ context.Context, service *v1.Service,
			_ *servicecontext.Context, _ *sync.WaitGroup) error {
			activationMutex.Lock()
			defer activationMutex.Unlock()
			activationCalls[service.UID]++
			if service.UID == types.UID("failing") && activationCalls[service.UID] == 1 {
				return errors.New("transient datapath failure")
			}
			return nil
		}),
	)
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	failingService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "failing", Namespace: "default", UID: types.UID("failing"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	healthyService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "healthy", Namespace: "default", UID: types.UID("healthy"), Annotations: annotations,
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"}}
	failingCtx := servicecontext.New(context.Background())
	healthyCtx := servicecontext.New(context.Background())
	p.svcMap.Store(failingService.UID, failingCtx)
	p.svcMap.Store(healthyService.UID, healthyCtx)
	failingCtx.SignalReadiness()
	healthyCtx.SignalReadiness()
	failingGeneration := failingCtx.CurrentReadiness()
	healthyGeneration := healthyCtx.CurrentReadiness()
	t.Cleanup(func() {
		releaseLeadership()
		failingCtx.Cancel()
		healthyCtx.Cancel()
		p.electionCoordinators.Wait()
	})

	failingDone := make(chan error, 1)
	healthyDone := make(chan error, 1)
	go func() { failingDone <- p.StartServicesLeaderElection(failingCtx, failingService, nil) }()
	go func() { healthyDone <- p.StartServicesLeaderElection(healthyCtx, healthyService, nil) }()
	waitForElectionRunner(t, runner.started)
	namespace, name := lease.ServiceName(failingService)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	waitForLeaseVIPCount(t, p.leaseMgr, id, 2)

	releaseLeadership()
	waitForCondition(t, func() bool {
		activationMutex.Lock()
		defer activationMutex.Unlock()
		return activationCalls[failingService.UID] == 2 && activationCalls[healthyService.UID] == 1
	}, "failed member retry without healthy sibling reactivation")

	if got := runner.starts.Load(); got != 1 {
		t.Fatalf("campaign starts = %d, want 1", got)
	}
	sharedLease := p.leaseMgr.Get(id)
	if sharedLease == nil {
		t.Fatal("member retry did not retain the shared lease")
	}
	observerParticipation := sharedLease.JoinElection()
	if observerParticipation.RunsCampaign() {
		t.Fatal("member retry replaced the running campaign")
	}
	if !observerParticipation.Session.IsLeading() {
		t.Fatal("member retry replaced or stopped the leading campaign")
	}
	if !failingCtx.ReadinessGenerationCurrent(failingGeneration) ||
		!healthyCtx.ReadinessGenerationCurrent(healthyGeneration) {
		t.Fatal("member retry changed endpoint readiness generation")
	}

	failingCtx.Cancel()
	healthyCtx.Cancel()
	if err := <-failingDone; err != nil {
		t.Fatalf("failing Service election error: %v", err)
	}
	if err := <-healthyDone; err != nil {
		t.Fatalf("healthy Service election error: %v", err)
	}
	p.electionCoordinators.Wait()
}

func TestElectionShutdownWaitsForControlPlaneToReleaseServiceOwnedCampaign(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{})}
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	var wg sync.WaitGroup
	done := make(chan error, 1)
	go func() {
		done <- p.StartServicesLeaderElection(svcCtx, service, &wg)
	}()
	waitForElectionRunner(t, runner.started)
	sharedLease := p.leaseMgr.Get(id)
	controlPlaneToken := lease.ObjectName(id, "cp")
	if claimed, _ := p.leaseMgr.ClaimWithVIPProvider(id, controlPlaneToken, nil); claimed != sharedLease {
		t.Fatal("control plane did not join the Service-owned lease")
	}
	controlPlaneParticipation := sharedLease.JoinElection()
	if controlPlaneParticipation.RunsCampaign() {
		t.Fatal("control plane replaced the Service-owned campaign")
	}
	controlPlaneElection := controlPlaneParticipation.Session

	svcCtx.Cancel()
	if err := <-done; err != nil {
		t.Fatalf("Service election watcher returned an error: %v", err)
	}
	select {
	case <-runner.stopping:
		t.Fatal("final Service departure stopped a campaign still used by the control plane")
	case <-time.After(20 * time.Millisecond):
	}
	if sharedLease.Ctx.Err() != nil || !controlPlaneElection.IsLeading() || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("Service departure retired the control-plane campaign")
	}

	shutdownDone := make(chan struct{})
	go func() {
		p.electionCoordinators.Wait()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("election shutdown completed while control plane retained the campaign")
	case <-time.After(20 * time.Millisecond):
	}

	p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("election shutdown did not complete after control plane released the campaign")
	}
	select {
	case <-runner.stopping:
	default:
		t.Fatal("election shutdown completed before the campaign runner stopped")
	}
}

func TestServiceWatcherWaitGroupDoesNotOwnCampaignRetainedByControlPlane(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{})}
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	var watcherWG sync.WaitGroup
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- p.StartServicesLeaderElection(svcCtx, service, &watcherWG)
	}()
	waitForElectionRunner(t, runner.started)

	sharedLease := p.leaseMgr.Get(id)
	if sharedLease == nil {
		t.Fatal("Service-owned campaign did not acquire its lease")
	}
	controlPlaneToken := lease.ObjectName(id, "cp")
	if claimed, _ := p.leaseMgr.ClaimWithVIPProvider(id, controlPlaneToken, nil); claimed != sharedLease {
		t.Fatal("control plane did not join the Service-owned lease")
	}
	t.Cleanup(func() {
		svcCtx.Cancel()
		p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
		p.electionCoordinators.Wait()
	})

	svcCtx.Cancel()
	select {
	case err := <-watchDone:
		if err != nil {
			t.Fatalf("Service election watcher returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Service election watcher did not stop after Service departure")
	}

	watcherStopped := make(chan struct{})
	go func() {
		watcherWG.Wait()
		close(watcherStopped)
	}()
	select {
	case <-watcherStopped:
	case <-time.After(time.Second):
		t.Fatal("Service watcher WaitGroup remained blocked by a campaign retained for control plane")
	}

	select {
	case <-runner.stopping:
		t.Fatal("draining the Service watcher stopped a campaign retained for control plane")
	default:
	}
	if sharedLease.Ctx.Err() != nil || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("draining the Service watcher retired the shared control-plane lease")
	}
}

func TestServicesWatcherReturnsWatchErrorWhileControlPlaneRetainsCampaign(t *testing.T) {
	service := &v1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "service", Namespace: "default", UID: types.UID("service"), ResourceVersion: "2",
			Annotations: map[string]string{kubevip.ServiceLease: "shared"},
		},
		Spec: v1.ServiceSpec{
			Type:                  v1.ServiceTypeLoadBalancer,
			LoadBalancerIP:        "192.0.2.10",
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
		},
	}
	emitWatchError := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Error("test HTTP writer does not support streaming")
			return
		}

		switch request.URL.Path {
		case "/api/v1/namespaces/default/services":
			encoder := json.NewEncoder(writer)
			if err := encoder.Encode(map[string]any{"type": "ADDED", "object": service}); err != nil {
				t.Errorf("encode Service watch event: %v", err)
				return
			}
			flusher.Flush()
			select {
			case <-request.Context().Done():
				return
			case <-emitWatchError:
			}
			status := &metav1.Status{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status:   metav1.StatusFailure, Message: "forced service watch failure",
				Reason: metav1.StatusReasonExpired, Code: http.StatusGone,
			}
			if err := encoder.Encode(map[string]any{"type": "ERROR", "object": status}); err != nil {
				t.Errorf("encode Service watch error: %v", err)
				return
			}
			flusher.Flush()
		case "/api/v1/namespaces/default/endpoints":
			flusher.Flush()
			<-request.Context().Done()
		default:
			http.Error(writer, "unexpected request path", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	t.Cleanup(cancelWatch)

	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: server.URL,
		ContentConfig: rest.ContentConfig{
			ContentType: "application/json",
		},
	})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{})}
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config: &kubevip.Config{
			EnableServicesElection: true,
			EnableEndpoints:        true,
			ServiceNamespace:       "default",
			DebounceTime:           "0s",
		},
		clientSet:     clientSet,
		rwClientSet:   clientSet,
		leaseMgr:      lease.NewManager(),
		lbClassFilter: func(*v1.Service, *kubevip.Config) bool { return false },
		instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, service *v1.Service,
			_ *sync.WaitGroup) (*instance.Instance, error) {
			return &instance.Instance{ServiceUID: service.UID, ServiceSnapshot: service.DeepCopy()}, nil
		}),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))

	watchDone := make(chan error, 1)
	go func() {
		watchDone <- p.ServicesWatcher(watchCtx, func(svcCtx *servicecontext.Context,
			service *v1.Service, wg *sync.WaitGroup) error {
			svcCtx.SignalReadiness()
			return p.StartServicesLeaderElection(svcCtx, service, wg)
		}, false)
	}()
	waitForElectionRunner(t, runner.started)

	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	sharedLease := p.leaseMgr.Get(id)
	if sharedLease == nil {
		t.Fatal("Service-owned campaign did not acquire its lease")
	}
	controlPlaneToken := lease.ObjectName(id, "cp")
	if claimed, _ := p.leaseMgr.ClaimWithVIPProvider(id, controlPlaneToken, nil); claimed != sharedLease {
		t.Fatal("control plane did not join the Service-owned lease")
	}
	t.Cleanup(func() {
		cancelWatch()
		p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
		p.electionCoordinators.Wait()
	})

	close(emitWatchError)
	select {
	case err := <-watchDone:
		if !utils.IsPanicError(err) {
			t.Fatalf("ServicesWatcher() error = %v, want non-recoverable watch error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServicesWatcher remained blocked by a campaign retained for control plane")
	}

	select {
	case <-runner.stopping:
		t.Fatal("Service watch failure stopped a campaign retained for control plane")
	default:
	}
	if sharedLease.Ctx.Err() != nil || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("Service watch failure retired the shared control-plane lease")
	}
}

func TestServiceOwnedCampaignPublishesLeadershipAfterFinalServiceLeaves(t *testing.T) {
	releaseLeading := make(chan struct{})
	var releaseLeadingOnce sync.Once
	releaseLeadership := func() {
		releaseLeadingOnce.Do(func() { close(releaseLeading) })
	}
	runner := &electionTestRunner{
		started:        make(chan struct{}),
		stopping:       make(chan struct{}),
		releaseLeading: releaseLeading,
	}
	var activationCalls atomic.Int64
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner), withTestElectionActivation(
		func(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error {
			activationCalls.Add(1)
			return nil
		},
	))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	var wg sync.WaitGroup
	serviceDone := make(chan error, 1)
	go func() {
		serviceDone <- p.StartServicesLeaderElection(svcCtx, service, &wg)
	}()
	waitForElectionRunner(t, runner.started)

	sharedLease := p.leaseMgr.Get(id)
	if sharedLease == nil {
		t.Fatal("Service-owned campaign did not acquire its lease")
	}
	controlPlaneToken := lease.ObjectName(id, "cp")
	if claimed, _ := p.leaseMgr.ClaimWithVIPProvider(id, controlPlaneToken, nil); claimed != sharedLease {
		t.Fatal("control plane did not join the Service-owned lease")
	}
	t.Cleanup(func() {
		releaseLeadership()
		svcCtx.Cancel()
		p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
	})

	controlPlaneParticipation := sharedLease.JoinElection()
	if controlPlaneParticipation.RunsCampaign() {
		t.Fatal("control plane replaced the running Service-owned campaign")
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	leadership := make(chan bool, 1)
	go func() {
		leadership <- controlPlaneParticipation.Session.WaitForLeader(waitCtx)
	}()

	svcCtx.Cancel()
	select {
	case err := <-serviceDone:
		if err != nil {
			t.Fatalf("Service election watcher returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Service election watcher did not stop after Service departure")
	}
	if sharedLease.Ctx.Err() != nil || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("final Service departure retired the shared control-plane lease")
	}
	if controlPlaneParticipation.Session.IsLeading() {
		t.Fatal("campaign became leader before the test released it")
	}
	select {
	case result := <-leadership:
		t.Fatalf("control-plane observer stopped before leadership was decided: %t", result)
	default:
	}

	releaseLeadership()
	select {
	case result := <-leadership:
		if !result {
			t.Fatal("control-plane observer did not observe Service-owned leadership")
		}
	case <-time.After(time.Second):
		t.Fatal("control-plane observer remained blocked after the runner became leader")
	}
	if !controlPlaneParticipation.Session.IsLeading() {
		t.Fatal("Service-owned runner did not publish leadership to the shared lease")
	}
	if got := activationCalls.Load(); got != 0 {
		t.Fatalf("departed Service was activated %d times, want 0", got)
	}

	p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
	waitForElectionRunner(t, runner.stopping)
}

func TestCancelledServiceOwnedCampaignRejectsLateLeadership(t *testing.T) {
	releaseLeading := make(chan struct{})
	releaseStop := make(chan struct{})
	var releaseLeadingOnce sync.Once
	var releaseStopOnce sync.Once
	releaseLeadership := func() {
		releaseLeadingOnce.Do(func() { close(releaseLeading) })
	}
	releaseRunner := func() {
		releaseStopOnce.Do(func() { close(releaseStop) })
	}
	runner := &electionTestRunner{
		started:        make(chan struct{}),
		stopping:       make(chan struct{}),
		releaseLeading: releaseLeading,
		releaseStop:    releaseStop,
	}
	p := &Processor{
		serviceLock: newTestServiceLocks(),
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
	}
	initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	t.Cleanup(func() {
		svcCtx.Cancel()
		releaseLeadership()
		releaseRunner()
	})

	var wg sync.WaitGroup
	serviceDone := make(chan error, 1)
	go func() {
		serviceDone <- p.StartServicesLeaderElection(svcCtx, service, &wg)
	}()
	waitForElectionRunner(t, runner.started)
	sharedLease := p.leaseMgr.Get(id)
	if sharedLease == nil {
		t.Fatal("Service-owned campaign did not acquire its lease")
	}
	observerParticipation := sharedLease.JoinElection()
	if observerParticipation.RunsCampaign() {
		t.Fatal("observer replaced the Service-owned campaign")
	}
	observerElection := observerParticipation.Session

	svcCtx.Cancel()
	select {
	case err := <-serviceDone:
		if err != nil {
			t.Fatalf("Service election watcher returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Service election watcher did not stop after Service departure")
	}
	if sharedLease.Ctx.Err() == nil || p.leaseMgr.Get(id) != nil {
		t.Fatal("final Service departure did not retire its unshared lease")
	}

	releaseLeadership()
	waitForElectionRunner(t, runner.stopping)
	if observerElection.IsLeading() {
		t.Fatal("cancelled campaign accepted a late leadership callback")
	}

	releaseRunner()
	p.electionCoordinators.Wait()
}

type electionTestRunner struct {
	started        chan struct{}
	startedOnce    sync.Once
	starts         atomic.Int64
	releaseLeading <-chan struct{}
	stopping       chan struct{}
	stoppingOnce   sync.Once
	releaseStop    <-chan struct{}
}

func (r *electionTestRunner) RunCampaign(ctx context.Context, run *election.RunConfig) error {
	r.starts.Add(1)
	r.startedOnce.Do(func() { close(r.started) })
	if r.releaseLeading != nil {
		<-r.releaseLeading
	}
	run.OnStartedLeading(ctx)
	<-ctx.Done()
	if r.stopping != nil {
		r.stoppingOnce.Do(func() { close(r.stopping) })
	}
	if r.releaseStop != nil {
		<-r.releaseStop
	}
	run.OnStoppedLeading()
	return nil
}

func waitForElectionRunner(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("election campaign did not start")
	}
}

func waitForCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForLeaseVIPCount(t *testing.T, manager *lease.Manager, id lease.ID, want int) {
	t.Helper()
	waitForCondition(t, func() bool {
		serviceLease := manager.Get(id)
		return serviceLease != nil && len(serviceLease.OwnedVIPs()) == want
	}, "service election VIP count")
}

func TestSharedLeaseMemberCancellationDoesNotWaitForLeaseRetirement(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	newService := func(name string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       types.UID(name + "-uid"),
				Annotations: map[string]string{
					kubevip.ServiceLease: "shared",
				},
			},
		}
	}

	leader := newService("leader")
	follower := newService("follower")
	leaderCtx := servicecontext.New(context.Background())
	followerCtx := servicecontext.New(context.Background())
	p.svcMap.Store(leader.UID, leaderCtx)
	p.svcMap.Store(follower.UID, followerCtx)
	p.ServiceInstances = []*instance.Instance{
		{ServiceSnapshot: leader.DeepCopy(), AddCalled: true},
		{ServiceSnapshot: follower.DeepCopy(), AddCalled: true},
	}

	namespace, name := lease.ServiceName(leader)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcLease := p.leaseMgr.Add(context.Background(), id)
	svcLease.Add(lease.ServiceNamespacedName(leader))
	svcLease.Elected.Store(true)
	close(svcLease.Started)

	if !followerCtx.StartWatching() {
		t.Fatal("failed to start follower watcher")
	}
	followerCtx.SignalReadiness()
	electionDone := make(chan error, 1)
	go func() {
		defer followerCtx.StopWatching()
		electionDone <- p.StartServicesLeaderElection(followerCtx, follower, nil, true)
	}()

	deadline := time.After(time.Second)
	for !svcLease.Has(lease.ServiceNamespacedName(follower)) {
		select {
		case <-deadline:
			t.Fatal("follower did not join the shared lease")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	joined := make(chan bool, 1)
	go func() {
		svcLease.Lock()
		isMember := svcLease.Has(lease.ServiceNamespacedName(follower))
		svcLease.Unlock()
		joined <- isMember
	}()
	select {
	case isMember := <-joined:
		if !isMember {
			t.Fatal("follower membership disappeared during campaign setup")
		}
	case <-time.After(time.Second):
		t.Fatal("follower did not finish joining the active campaign")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- p.Delete(watch.Event{Type: watch.Deleted, Object: follower}, false)
	}()

	select {
	case err := <-electionDone:
		if err != nil {
			t.Fatalf("StartServicesLeaderElection returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower election routine waited for the shared lease to retire")
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("deleting follower returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower watcher did not return")
	}

	if got := p.leaseMgr.Get(id); got != svcLease {
		t.Fatal("shared lease was retired while its leader remained")
	}
	if svcLease.Ctx.Err() != nil {
		t.Fatal("shared lease context was cancelled while its leader remained")
	}
	if _, ok := p.svcMap.Load(follower.UID); ok {
		t.Fatal("follower context remained registered")
	}
	if len(p.ServiceInstances) != 1 || p.ServiceInstances[0].ServiceSnapshot.UID != leader.UID {
		t.Fatal("follower instance was not removed without disturbing its sibling")
	}

	processed := make(chan bool, 1)
	go func() {
		processed <- p.withActiveService(leader.UID, leaderCtx, func() {})
	}()
	select {
	case active := <-processed:
		if !active {
			t.Fatal("remaining service event was rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("remaining service event processing was blocked")
	}

	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: leader}, false); err != nil {
		t.Fatalf("deleting final member returned an error: %v", err)
	}
	if p.leaseMgr.Get(id) != nil {
		t.Fatal("shared lease remained after deleting its final member")
	}
	if svcLease.Ctx.Err() == nil {
		t.Fatal("shared lease context remained live after deleting its final member")
	}
	if len(p.ServiceInstances) != 0 {
		t.Fatalf("service instances remained after final cleanup: %d", len(p.ServiceInstances))
	}
	if _, ok := p.svcMap.Load(leader.UID); ok {
		t.Fatal("final service context remained registered")
	}
}

<<<<<<< HEAD
func TestSharedLeaseFollowerWithdrawsBeforeTakeover(t *testing.T) {
	p := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true},
		leaseMgr:         lease.NewManager(),
		nodeLabelManager: noop.NewManager(),
	}
	newService := func(name string) *v1.Service {
		return &v1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"),
			Annotations: map[string]string{kubevip.ServiceLease: "shared"},
		}}
	}
	leader := newService("leader")
	follower := newService("follower")
	followerCtx := servicecontext.New(context.Background())
	t.Cleanup(followerCtx.Cancel)
	p.svcMap.Store(follower.UID, followerCtx)
	followerInstance := &instance.Instance{ServiceSnapshot: follower.DeepCopy()}
	p.ServiceInstances = []*instance.Instance{
		{ServiceSnapshot: leader.DeepCopy(), AddCalled: true},
		followerInstance,
	}

	namespace, name := lease.ServiceName(follower)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcLease := p.leaseMgr.Add(context.Background(), id)
	svcLease.Add(lease.ServiceNamespacedName(leader))
	svcLease.Elected.Store(true)
	close(svcLease.Started)
	followerCtx.SignalReadiness()

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(followerCtx, follower, nil, true) }()

	deadline := time.After(time.Second)
	for {
		unlock := p.lockService(follower.UID)
		started := followerInstance.AddCalled
		unlock()
		if started {
			break
		}
		select {
		case <-deadline:
			t.Fatal("follower did not enter the active shared lease campaign")
		case <-time.After(time.Millisecond):
		}
	}

	// Local leadership loss must withdraw the follower before the restart loop
	// campaigns again; the old implementation waited for service deletion.
	svcLease.Elected.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follower did not return after leader loss: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower remained blocked after shared lease leadership was lost")
	}

	if len(p.ServiceInstances) != 1 || p.ServiceInstances[0].ServiceSnapshot.UID != leader.UID {
		t.Fatal("follower cleanup removed the sibling service")
	}
	if !svcLease.Has(lease.ServiceNamespacedName(leader)) || !svcLease.Has(lease.ServiceNamespacedName(follower)) {
		t.Fatal("follower cleanup changed shared lease membership")
	}
	if svcLease.Ctx.Err() != nil {
		t.Fatal("follower cleanup retired the shared lease while sibling remained")
	}
}

func TestSharedLeaseOwnerCancellationStopsCampaignBeforeWatcherWait(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	newService := func(name string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       types.UID(name + "-uid"),
				Annotations: map[string]string{
					kubevip.ServiceLease: "shared",
				},
			},
		}
	}

	owner := newService("owner")
	sibling := newService("sibling")
	ownerCtx := servicecontext.New(context.Background())
	siblingCtx := servicecontext.New(context.Background())
	p.svcMap.Store(owner.UID, ownerCtx)
	p.svcMap.Store(sibling.UID, siblingCtx)
	p.ServiceInstances = []*instance.Instance{
		{ServiceSnapshot: owner.DeepCopy(), AddCalled: true},
		{ServiceSnapshot: sibling.DeepCopy(), AddCalled: true},
	}

	namespace, name := lease.ServiceName(owner)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcLease := p.leaseMgr.Add(context.Background(), id)
	ownerName := lease.ServiceNamespacedName(owner)
	siblingName := lease.ServiceNamespacedName(sibling)
	svcLease.Add(ownerName)
	svcLease.Add(siblingName)
	svcLease.Elected.Store(true)
	close(svcLease.Started)

	if !ownerCtx.StartWatching() {
		t.Fatal("failed to start owner watcher")
	}
	campaignStopped := make(chan struct{})
	var stopCampaign sync.Once
	ownerCtx.SetLeaderCancel(func() {
		stopCampaign.Do(func() {
			svcLease.Elected.Store(false)
			svcLease.Started = make(chan any)
			close(campaignStopped)
		})
	})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		<-ownerCtx.Ctx.Done()
		<-campaignStopped
		ownerCtx.StopWatching()
	}()

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- p.Delete(watch.Event{Type: watch.Deleted, Object: owner}, false)
	}()
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("deleting campaign owner returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deleting campaign owner waited for its watcher")
	}
	<-watcherDone
	select {
	case <-campaignStopped:
	default:
		t.Fatal("owner campaign was not cancelled before the watcher wait completed")
	}

	if got := p.leaseMgr.Get(id); got != svcLease {
		t.Fatal("shared lease was replaced while its sibling remained")
	}
	if svcLease.Ctx.Err() != nil {
		t.Fatal("shared lease was cancelled while its sibling remained")
	}
	if svcLease.Has(ownerName) {
		t.Fatal("campaign owner remained a lease member")
	}
	if !svcLease.Has(siblingName) {
		t.Fatal("sibling membership disappeared with the campaign owner")
	}
	if _, ok := p.svcMap.Load(owner.UID); ok {
		t.Fatal("campaign owner context remained registered")
	}
	if len(p.ServiceInstances) != 1 || p.ServiceInstances[0].ServiceSnapshot.UID != sibling.UID {
		t.Fatal("campaign owner cleanup disturbed its sibling instance")
	}

	campaigned := make(chan struct{})
	go func() {
		svcLease.Lock()
		if !svcLease.Elected.Load() && svcLease.Has(siblingName) {
			svcLease.Elected.Store(true)
			close(svcLease.Started)
		}
		svcLease.Unlock()
		close(campaigned)
	}()
	select {
	case <-campaigned:
	case <-time.After(time.Second):
		t.Fatal("sibling could not campaign on the shared lease")
	}
	if !svcLease.Elected.Load() {
		t.Fatal("sibling did not take over the shared lease")
	}

	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: sibling}, false); err != nil {
		t.Fatalf("deleting final member returned an error: %v", err)
	}
	if p.leaseMgr.Get(id) != nil || svcLease.Ctx.Err() == nil {
		t.Fatal("final member did not retire the shared lease")
	}
	if len(p.ServiceInstances) != 0 {
		t.Fatalf("service instances remained after final cleanup: %d", len(p.ServiceInstances))
	}
	if _, ok := p.svcMap.Load(sibling.UID); ok {
		t.Fatal("final service context remained registered")
	}

	replacement := p.leaseMgr.Add(context.Background(), id)
	replacement.Add(siblingName)
	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: sibling}, false); err != nil {
		t.Fatalf("repeating final member deletion returned an error: %v", err)
	}
	p.leaseMgr.Delete(id, siblingName, svcLease)
	if p.leaseMgr.Get(id) != replacement || replacement.Ctx.Err() != nil {
		t.Fatal("stale final cleanup retired the replacement lease")
	}
}
func TestStartServicesLeaderElectionMetricsLifecycleAndRetries(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{LeaderElectionType: "test"},
		leaseMgr: lease.NewManager(),
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "metrics", Namespace: "on-demand", UID: types.UID("metrics"),
	}}
	metrics.ServiceElectionLoops.DeleteLabelValues(service.Namespace, service.Name)
	metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	defer metrics.ServiceElectionLoops.DeleteLabelValues(service.Namespace, service.Name)
	defer metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)

	namespace, name := lease.ServiceName(service)
	p.leaseMgr.Add(context.Background(), lease.NewID(p.config.LeaderElectionType, namespace, name))
	svcCtx := servicecontext.New(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil, true) }()

	deadline := time.Now().Add(time.Second)
	for testutil.ToFloat64(metrics.ServiceElectionLoops.WithLabelValues(service.Namespace, service.Name)) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("on-demand election loop metric did not become active")
		}
		time.Sleep(time.Millisecond)
	}
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 0 {
		t.Fatalf("attempts before election invocation = %v, want 0", got)
	}

	svcCtx.SignalReadiness()
	if err := <-done; err != nil {
		t.Fatalf("first StartServicesLeaderElection() error = %v", err)
	}
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 1 {
		t.Fatalf("attempts after first invocation = %v, want 1", got)
	}
	if err := p.StartServicesLeaderElection(svcCtx, service, nil, true); err != nil {
		t.Fatalf("second StartServicesLeaderElection() error = %v", err)
	}
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 2 {
		t.Fatalf("attempts after retry = %v, want 2", got)
	}
	svcCtx.Cancel()
}
