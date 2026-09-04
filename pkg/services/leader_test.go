package services

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/prometheus/client_golang/prometheus/testutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestStartServicesLeaderElectionTracksSharedMembersAcrossReadinessLoss(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{EnableServicesElection: true},
		leaseMgr: lease.NewManager(),
	}
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	firstService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", UID: types.UID("first"), Annotations: annotations,
	}}
	secondService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "second", Namespace: "default", UID: types.UID("second"), Annotations: annotations,
	}}
	namespace, name := lease.ServiceName(firstService)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	sharedLease := p.leaseMgr.Add(context.Background(), id)
	sharedLease.ElectionStarted()

	firstCtx := servicecontext.New(context.Background())
	secondCtx := servicecontext.New(context.Background())
	p.svcMap.Store(firstService.UID, firstCtx)
	p.svcMap.Store(secondService.UID, secondCtx)

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	var wg sync.WaitGroup
	go func() { firstDone <- p.StartServicesLeaderElection(firstCtx, firstService, &wg, true) }()
	go func() { secondDone <- p.StartServicesLeaderElection(secondCtx, secondService, &wg, true) }()

	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()
	election := waitForServiceElectionMembers(t, p, id, 2)
	election.mutex.Lock()
	firstToken := election.members[firstService.UID].claimToken
	election.mutex.Unlock()

	resetServiceReadiness(t, firstCtx)
	waitForServiceElectionMembers(t, p, id, 1)
	if !sharedLease.Elected.Load() || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("one shared member losing readiness ended the healthy sibling campaign")
	}

	firstCtx.SignalReadiness()
	election = waitForServiceElectionMembers(t, p, id, 2)
	election.mutex.Lock()
	secondToken := election.members[firstService.UID].claimToken
	election.mutex.Unlock()
	if secondToken == firstToken {
		t.Fatal("readiness recovery reused the prior member claim token")
	}

	secondCtx.Cancel()
	waitForServiceElectionMembers(t, p, id, 1)
	if firstCtx.Ctx.Err() != nil || !sharedLease.Elected.Load() || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("deleting one shared member ended the healthy sibling campaign")
	}

	firstCtx.Cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first member returned error: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second member returned error: %v", err)
	}
	if p.leaseMgr.Get(id) != nil {
		t.Fatal("final shared member withdrawal did not retire the lease")
	}
}

func TestServiceMemberLeavingDoesNotCancelControlPlaneLease(t *testing.T) {
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	controlPlaneToken := lease.ObjectName(id, "cp")
	sharedLease, _ := p.leaseMgr.Acquire(context.Background(), id, controlPlaneToken)
	if !sharedLease.BeginElection() {
		t.Fatal("control-plane election did not start")
	}
	sharedLease.ElectionStarted()

	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	member, joined := p.joinServiceElection(svcCtx, service, generation)
	if !joined {
		t.Fatal("Service did not join the control-plane lease")
	}
	p.leaveServiceElection(member)

	if sharedLease.Ctx.Err() != nil || !sharedLease.Elected.Load() || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("leaving Service member cancelled the control-plane lease")
	}
	p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
}

func TestServiceMemberDeactivatesWhenExternalElectionStops(t *testing.T) {
	activated := make(chan struct{}, 1)
	p := &Processor{
		config:                  &kubevip.Config{},
		leaseMgr:                lease.NewManager(),
		scheduleElectionRestart: func(func()) {},
		serviceSync: func(_ context.Context, _ *servicecontext.Context, _ *v1.Service, _ *sync.WaitGroup, _ bool) error {
			activated <- struct{}{}
			return nil
		},
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	controlPlaneToken := lease.ObjectName(id, "cp")
	sharedLease, _ := p.leaseMgr.Acquire(context.Background(), id, controlPlaneToken)
	if !sharedLease.BeginElection() {
		t.Fatal("external election did not start")
	}
	sharedLease.ElectionStarted()

	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	member, joined := p.joinServiceElection(svcCtx, service, generation)
	if !joined {
		t.Fatal("Service did not join the external election")
	}
	var wg sync.WaitGroup
	member.election.startCampaign(&wg)
	select {
	case <-activated:
	case <-time.After(time.Second):
		t.Fatal("Service member was not activated by external leadership")
	}

	sharedLease.ElectionStopped()
	wg.Wait()
	member.election.mutex.Lock()
	active := member.active
	member.election.mutex.Unlock()
	if active {
		t.Fatal("Service member remained active after external leadership ended")
	}
	p.leaveServiceElection(member)
	p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
}

func TestStartServicesLeaderElectionRejectsNilContext(t *testing.T) {
	p := &Processor{}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", UID: types.UID("service")}}
	if err := p.StartServicesLeaderElection(nil, service, nil, true); err == nil {
		t.Fatal("nil service context started leader election")
	}
}

func TestStartServicesLeaderElectionStaleContextReturnsPromptly(t *testing.T) {
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: "default", UID: types.UID("stale")}}
	staleContext := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, servicecontext.New(context.Background()))

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(staleContext, service, nil, true) }()
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
	p := &Processor{}
	var service *v1.Service
	if err := p.StartServicesLeaderElection(servicecontext.New(context.Background()), service, nil, true); err == nil {
		t.Fatal("typed-nil service started leader election")
	}
}

func TestStartServicesLeaderElectionDoesNotRegisterCancelledContext(t *testing.T) {
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "cancelled", Namespace: "default", UID: types.UID("cancelled")}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.Cancel()

	if err := p.StartServicesLeaderElection(svcCtx, service, nil, true); err == nil {
		t.Fatal("cancelled service context started leader election")
	}
	namespace, name := lease.ServiceName(service)
	if p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, namespace, name)) != nil {
		t.Fatal("cancelled service context registered a lease")
	}
}

func TestStartServicesLeaderElectionRegistersOneMemberForConcurrentCalls(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{})}
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager(), electionRun: runner.run}
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
			errors <- p.StartServicesLeaderElection(svcCtx, service, nil, true)
		}()
	}
	close(start)
	waitForElectionRunner(t, runner.started)
	namespace, name := lease.ServiceName(service)
	waitForServiceElectionMembers(t, p, lease.NewID(p.config.LeaderElectionType, namespace, name), 1)
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
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager(), electionRun: runner.run}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "recreate", Namespace: "default", UID: types.UID("recreate")}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	oldLease := p.leaseMgr.Add(context.Background(), id)
	oldLease.Cancel()
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil, true) }()
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
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager(), electionRun: runner.run}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: "default", UID: types.UID("restart")}}
	metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	defer metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil, true) }()
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

func TestServiceElectionWaitGroupDrainsCampaignOnShutdown(t *testing.T) {
	releaseStop := make(chan struct{})
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{}), releaseStop: releaseStop}
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager(), electionRun: runner.run}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "shutdown", Namespace: "default", UID: types.UID("shutdown"),
	}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	var wg sync.WaitGroup
	startDone := make(chan error, 1)
	go func() {
		startDone <- p.StartServicesLeaderElection(svcCtx, service, &wg, true)
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
		t.Fatal("Service WaitGroup completed while campaign shutdown was blocked")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseStop)
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("Service WaitGroup did not complete after campaign shutdown")
	}
}

func TestServiceElectionAttemptWaitsForReadiness(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{})}
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager(), electionRun: runner.run}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "readiness", Namespace: "default", UID: types.UID("readiness")}}
	metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	defer metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil, true) }()
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
		config: &kubevip.Config{}, leaseMgr: lease.NewManager(), electionRun: runner.run,
		scheduleElectionRestart: func(restart func()) { restart() },
	}
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	firstService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", UID: types.UID("first"), Annotations: annotations,
	}}
	secondService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "second", Namespace: "default", UID: types.UID("second"), Annotations: annotations,
	}}
	firstCtx := servicecontext.New(context.Background())
	secondCtx := servicecontext.New(context.Background())
	p.svcMap.Store(firstService.UID, firstCtx)
	p.svcMap.Store(secondService.UID, secondCtx)
	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- p.StartServicesLeaderElection(firstCtx, firstService, nil, true) }()
	go func() { secondDone <- p.StartServicesLeaderElection(secondCtx, secondService, nil, true) }()
	waitForElectionRunner(t, runner.started)
	namespace, name := lease.ServiceName(firstService)
	waitForServiceElectionMembers(t, p, lease.NewID(p.config.LeaderElectionType, namespace, name), 2)

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

func TestSharedElectionDeletedCandidateNeverActivates(t *testing.T) {
	releaseLeading := make(chan struct{})
	runner := &electionTestRunner{started: make(chan struct{}), releaseLeading: releaseLeading}
	var syncMutex sync.Mutex
	syncCalls := map[types.UID]int{}
	p := &Processor{
		config: &kubevip.Config{}, leaseMgr: lease.NewManager(), electionRun: runner.run,
		serviceSync: func(_ context.Context, _ *servicecontext.Context, service *v1.Service, _ *sync.WaitGroup, _ bool) error {
			syncMutex.Lock()
			defer syncMutex.Unlock()
			syncCalls[service.UID]++
			return nil
		},
	}
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	candidateService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "candidate", Namespace: "default", UID: types.UID("candidate"), Annotations: annotations,
	}}
	siblingService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "sibling", Namespace: "default", UID: types.UID("sibling"), Annotations: annotations,
	}}
	candidateCtx := servicecontext.New(context.Background())
	siblingCtx := servicecontext.New(context.Background())
	p.svcMap.Store(candidateService.UID, candidateCtx)
	p.svcMap.Store(siblingService.UID, siblingCtx)
	candidateCtx.SignalReadiness()
	siblingCtx.SignalReadiness()

	candidateDone := make(chan error, 1)
	siblingDone := make(chan error, 1)
	go func() { candidateDone <- p.StartServicesLeaderElection(candidateCtx, candidateService, nil, true) }()
	go func() { siblingDone <- p.StartServicesLeaderElection(siblingCtx, siblingService, nil, true) }()
	waitForElectionRunner(t, runner.started)
	namespace, name := lease.ServiceName(candidateService)
	election := waitForServiceElectionMembers(t, p, lease.NewID(p.config.LeaderElectionType, namespace, name), 2)

	candidateCtx.Cancel()
	if err := <-candidateDone; err != nil {
		t.Fatalf("candidate service election error = %v", err)
	}
	close(releaseLeading)
	waitForCondition(t, func() bool {
		election.mutex.Lock()
		defer election.mutex.Unlock()
		return len(election.members) == 1 && election.members[siblingService.UID].active
	}, "live sibling activation")

	election.mutex.Lock()
	_, candidateActive := election.members[candidateService.UID]
	election.mutex.Unlock()
	if candidateActive {
		t.Fatal("deleted candidate remained eligible for activation")
	}
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
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager(), electionRun: runner.run}
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	firstService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", UID: types.UID("first"), Annotations: annotations,
	}}
	secondService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "second", Namespace: "default", UID: types.UID("second"), Annotations: annotations,
	}}
	firstCtx := servicecontext.New(context.Background())
	secondCtx := servicecontext.New(context.Background())
	p.svcMap.Store(firstService.UID, firstCtx)
	p.svcMap.Store(secondService.UID, secondCtx)
	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- p.StartServicesLeaderElection(firstCtx, firstService, nil, true) }()
	go func() { secondDone <- p.StartServicesLeaderElection(secondCtx, secondService, nil, true) }()
	waitForElectionRunner(t, runner.started)
	namespace, name := lease.ServiceName(firstService)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	waitForServiceElectionMembers(t, p, id, 2)

	resetServiceReadiness(t, firstCtx)
	waitForServiceElectionMembers(t, p, id, 1)
	if runner.starts.Load() != 1 {
		t.Fatalf("campaign starts after one member lost readiness = %d, want 1", runner.starts.Load())
	}
	firstCtx.SignalReadiness()
	waitForServiceElectionMembers(t, p, id, 2)
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

func TestServiceOwnedCampaignSurvivesFinalServiceWhileControlPlaneRemains(t *testing.T) {
	runner := &electionTestRunner{started: make(chan struct{}), stopping: make(chan struct{})}
	p := &Processor{
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
		electionRun: runner.run,
		serviceSync: func(context.Context, *servicecontext.Context, *v1.Service, *sync.WaitGroup, bool) error { return nil },
	}
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
		done <- p.StartServicesLeaderElection(svcCtx, service, &wg, true)
	}()
	waitForElectionRunner(t, runner.started)
	sharedLease := p.leaseMgr.Get(id)
	controlPlaneToken := lease.ObjectName(id, "cp")
	if claimed, _ := p.leaseMgr.Claim(id, controlPlaneToken); claimed != sharedLease {
		t.Fatal("control plane did not join the Service-owned lease")
	}

	svcCtx.Cancel()
	if err := <-done; err != nil {
		t.Fatalf("Service election watcher returned an error: %v", err)
	}
	select {
	case <-runner.stopping:
		t.Fatal("final Service departure stopped a campaign still used by the control plane")
	case <-time.After(20 * time.Millisecond):
	}
	if sharedLease.Ctx.Err() != nil || !sharedLease.Elected.Load() || p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("Service departure retired the control-plane campaign")
	}

	p.leaseMgr.Delete(id, controlPlaneToken, sharedLease)
	wg.Wait()
	select {
	case <-runner.stopping:
	default:
		t.Fatal("campaign did not stop after its final control-plane member left")
	}
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

func (r *electionTestRunner) run(ctx context.Context, run *election.RunConfig, _ *kubevip.Config) error {
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

func waitForServiceElectionMembers(t *testing.T, p *Processor, id lease.ID, want int) *serviceElection {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.electionsMutex.Lock()
		election := p.elections[id.NamespacedName()]
		p.electionsMutex.Unlock()
		if election != nil {
			election.mutex.Lock()
			count := len(election.members)
			election.mutex.Unlock()
			if count == want {
				return election
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("service election member count did not reach %d", want)
	return nil
}
