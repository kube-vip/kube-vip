package services

import (
	"context"
	"errors"
	"slices"
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

func resetServiceReadiness(t *testing.T, svcCtx *servicecontext.Context) {
	t.Helper()
	generation, _, _, ready := svcCtx.ReadinessState()
	if !ready || !svcCtx.ResetReadinessGeneration(generation) {
		t.Fatal("Service readiness generation was not reset")
	}
}

func TestOrderedServiceVIPsUsesCreationTimeAndStableIdentity(t *testing.T) {
	older := metav1.NewTime(time.Unix(100, 0))
	newer := metav1.NewTime(time.Unix(200, 0))
	services := []*v1.Service{
		{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "default", UID: "new", CreationTimestamp: newer}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.30"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "default", UID: "second", CreationTimestamp: older}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "default", UID: "first", CreationTimestamp: older}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}},
	}

	got := orderedServiceVIPs(services)
	want := []string{"192.0.2.10", "192.0.2.20", "192.0.2.30"}
	if !slices.Equal(got, want) {
		t.Fatalf("orderedServiceVIPs() = %v, want %v", got, want)
	}
}

func TestServiceElectionClaimsOnlyCurrentReadinessGeneration(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcCtx := servicecontext.New(context.Background())
	defer svcCtx.Cancel()
	p.svcMap.Store(service.UID, svcCtx)

	svcCtx.SignalReadiness()
	firstGeneration, _, _, _ := svcCtx.ReadinessState()
	first, joined := p.joinServiceElection(svcCtx, service, firstGeneration)
	if !joined {
		t.Fatal("first ready generation did not join its service election")
	}
	if first.claimToken == "" || first.claimToken == string(service.UID) {
		t.Fatalf("claim token %q is not a coordinator-issued opaque token", first.claimToken)
	}

	p.leaveServiceElection(first)
	resetServiceReadiness(t, svcCtx)
	svcCtx.SignalReadiness()
	secondGeneration, _, _, _ := svcCtx.ReadinessState()
	second, joined := p.joinServiceElection(svcCtx, service, secondGeneration)
	if !joined {
		t.Fatal("second ready generation did not join its service election")
	}
	if second.claimToken == first.claimToken {
		t.Fatal("new readiness generation reused the old lease claim token")
	}
	p.removeServiceElection(first.election)
	p.electionsMutex.Lock()
	current := p.elections[id.NamespacedName()]
	p.electionsMutex.Unlock()
	if current != second.election {
		t.Fatal("retired coordinator cleanup removed its replacement")
	}

	// Delayed cleanup from the old generation must not remove the new claim.
	p.leaveServiceElection(first)
	if p.leaseMgr.Get(id) == nil {
		t.Fatal("stale generation cleanup retired the current service election")
	}
	p.leaveServiceElection(second)
	if p.leaseMgr.Get(id) != nil {
		t.Fatal("final member withdrawal did not retire its lease")
	}
}

func TestServiceElectionStaleUIDCannotReleaseRecreatedService(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	oldService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("old"), Annotations: annotations,
	}}
	newService := oldService.DeepCopy()
	newService.UID = types.UID("new")
	siblingService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "sibling", Namespace: "default", UID: types.UID("sibling"), Annotations: annotations,
	}}

	oldCtx := servicecontext.New(context.Background())
	siblingCtx := servicecontext.New(context.Background())
	p.svcMap.Store(oldService.UID, oldCtx)
	p.svcMap.Store(siblingService.UID, siblingCtx)
	oldCtx.SignalReadiness()
	siblingCtx.SignalReadiness()
	oldGeneration, _, _, _ := oldCtx.ReadinessState()
	oldMember, joined := p.joinServiceElection(oldCtx, oldService, oldGeneration)
	if !joined {
		t.Fatal("old Service did not join its election")
	}
	siblingGeneration, _, _, _ := siblingCtx.ReadinessState()
	siblingMember, joined := p.joinServiceElection(siblingCtx, siblingService, siblingGeneration)
	if !joined {
		t.Fatal("sibling Service did not join its election")
	}
	originalElection := oldMember.election
	originalLease := originalElection.lease

	newCtx := servicecontext.New(context.Background())
	p.svcMap.Store(newService.UID, newCtx)
	newCtx.SignalReadiness()
	newGeneration, _, _, _ := newCtx.ReadinessState()
	newMember, joined := p.joinServiceElection(newCtx, newService, newGeneration)
	if !joined {
		t.Fatal("recreated Service did not join its election")
	}
	p.leaveServiceElection(oldMember)
	if newMember.election != originalElection || newMember.election.lease != originalLease {
		t.Fatal("recreated Service did not join the sibling's live election")
	}
	originalElection.mutex.Lock()
	currentNew := originalElection.members[newService.UID]
	currentSibling := originalElection.members[siblingService.UID]
	originalElection.mutex.Unlock()
	if currentNew != newMember || currentSibling != siblingMember {
		t.Fatal("stale Service UID cleanup removed a live shared-lease member")
	}
	p.leaveServiceElection(newMember)
	p.leaveServiceElection(siblingMember)
}

func TestServiceElectionActivatesMemberJoiningActiveCampaign(t *testing.T) {
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
	firstCtx := servicecontext.New(context.Background())
	secondCtx := servicecontext.New(context.Background())
	p.svcMap.Store(firstService.UID, firstCtx)
	p.svcMap.Store(secondService.UID, secondCtx)
	firstCtx.SignalReadiness()
	firstGeneration, _, _, _ := firstCtx.ReadinessState()
	first, joined := p.joinServiceElection(firstCtx, firstService, firstGeneration)
	if !joined {
		t.Fatal("first member did not join its election")
	}

	election := first.election
	election.mutex.Lock()
	election.campaign = &serviceElectionCampaign{done: make(chan struct{}), leaderCtx: context.Background()}
	svcLease := election.lease
	election.mutex.Unlock()
	svcLease.ElectionStarted()

	secondCtx.SignalReadiness()
	secondGeneration, _, _, _ := secondCtx.ReadinessState()
	second, joined := p.joinServiceElection(secondCtx, secondService, secondGeneration)
	if !joined {
		t.Fatal("late member did not join its election")
	}
	election.startCampaign(&sync.WaitGroup{})

	election.mutex.Lock()
	active := second.active
	election.mutex.Unlock()
	if !active {
		t.Fatal("member joining an active campaign was not activated")
	}
	election.mutex.Lock()
	election.campaign = nil
	election.mutex.Unlock()
	p.leaveServiceElection(first)
	p.leaveServiceElection(second)
}

func TestServiceElectionLeadershipLossWaitsForMemberActivation(t *testing.T) {
	syncStarted := make(chan struct{})
	releaseSync := make(chan struct{})
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	var p *Processor
	p = &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
		serviceSync: func(_ context.Context, _ *servicecontext.Context, _ *v1.Service, _ *sync.WaitGroup, _ bool) error {
			close(syncStarted)
			<-releaseSync
			unlockService := p.lockService(service.UID)
			p.appendServiceInstance(&instance.Instance{ServiceUID: service.UID, ServiceSnapshot: service.DeepCopy(), AddCalled: true})
			unlockService()
			return nil
		},
	}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	member, joined := p.joinServiceElection(svcCtx, service, generation)
	if !joined {
		t.Fatal("Service did not join its election")
	}

	election := member.election
	campaign := &serviceElectionCampaign{done: make(chan struct{})}
	election.mutex.Lock()
	election.campaign = campaign
	svcLease := election.lease
	election.mutex.Unlock()
	svcLease.ElectionStarted()

	activationDone := make(chan struct{})
	go func() {
		election.activateMember(context.Background(), member, svcLease, campaign, &sync.WaitGroup{})
		close(activationDone)
	}()
	<-syncStarted

	stopDone := make(chan struct{})
	go func() {
		election.stopCampaign(svcLease, campaign)
		close(stopDone)
	}()
	waitForCondition(t, func() bool {
		election.mutex.Lock()
		defer election.mutex.Unlock()
		return campaign.stopped
	}, "campaign leadership loss")
	select {
	case <-stopDone:
		t.Fatal("campaign stop completed before in-flight member activation")
	default:
	}

	close(releaseSync)
	select {
	case <-activationDone:
	case <-time.After(time.Second):
		t.Fatal("member activation did not finish")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("campaign stop did not finish after member activation")
	}

	if svcLease.Elected.Load() {
		t.Fatal("lease remained elected after campaign stop")
	}
	election.mutex.Lock()
	active := member.active
	election.mutex.Unlock()
	if active {
		t.Fatal("member remained active after campaign stop")
	}
	if p.findServiceInstance(service) != nil {
		t.Fatal("in-flight activation left a tracked Service instance after leadership loss")
	}
	p.leaveServiceElection(member)
}

func TestServiceElectionLeadershipLossCancelsMemberActivation(t *testing.T) {
	syncStarted := make(chan struct{})
	cancellationObserved := make(chan struct{})
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
		serviceSync: func(ctx context.Context, _ *servicecontext.Context, _ *v1.Service, _ *sync.WaitGroup, _ bool) error {
			close(syncStarted)
			<-ctx.Done()
			close(cancellationObserved)
			return ctx.Err()
		},
	}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	member, joined := p.joinServiceElection(svcCtx, service, generation)
	if !joined {
		t.Fatal("Service did not join its election")
	}

	election := member.election
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	campaign := &serviceElectionCampaign{done: make(chan struct{}), leaderCtx: leaderCtx, cancelLeader: cancelLeader}
	election.mutex.Lock()
	election.campaign = campaign
	svcLease := election.lease
	election.mutex.Unlock()
	svcLease.ElectionStarted()

	activationDone := make(chan struct{})
	go func() {
		election.activateMember(leaderCtx, member, svcLease, campaign, &sync.WaitGroup{})
		close(activationDone)
	}()
	<-syncStarted

	stopDone := make(chan struct{})
	go func() {
		election.stopCampaign(svcLease, campaign)
		close(stopDone)
	}()
	select {
	case <-cancellationObserved:
	case <-time.After(time.Second):
		t.Fatal("leadership loss did not cancel member activation")
	}
	select {
	case <-activationDone:
	case <-time.After(time.Second):
		t.Fatal("member activation did not finish after cancellation")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("campaign stop did not finish after activation cancellation")
	}
	p.leaveServiceElection(member)
}

func TestServiceElectionActivationFailureAfterMemberRemovalDoesNotPanic(t *testing.T) {
	syncStarted := make(chan struct{})
	releaseSync := make(chan struct{})
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
		serviceSync: func(_ context.Context, _ *servicecontext.Context, _ *v1.Service, _ *sync.WaitGroup, _ bool) error {
			close(syncStarted)
			<-releaseSync
			return errors.New("activation failed")
		},
	}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	member, joined := p.joinServiceElection(svcCtx, service, generation)
	if !joined {
		t.Fatal("Service did not join its election")
	}

	election := member.election
	campaign := &serviceElectionCampaign{done: make(chan struct{})}
	election.mutex.Lock()
	election.campaign = campaign
	svcLease := election.lease
	election.mutex.Unlock()
	svcLease.ElectionStarted()

	activationDone := make(chan any, 1)
	go func() {
		defer func() {
			activationDone <- recover()
		}()
		election.activateMember(context.Background(), member, svcLease, campaign, &sync.WaitGroup{})
	}()
	<-syncStarted

	p.leaveServiceElection(member)
	close(campaign.done)
	close(releaseSync)

	select {
	case panicValue := <-activationDone:
		if panicValue != nil {
			t.Fatalf("activation failure after member removal panicked: %v", panicValue)
		}
	case <-time.After(time.Second):
		t.Fatal("member activation did not finish")
	}
}

func TestStopCampaignDoesNotCleanupInactiveMember(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "inactive", Namespace: "default", UID: types.UID("inactive"),
	}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	member, joined := p.joinServiceElection(svcCtx, service, generation)
	if !joined {
		t.Fatal("inactive Service did not join its election")
	}
	serviceInstance := &instance.Instance{ServiceUID: service.UID, ServiceSnapshot: service.DeepCopy()}
	p.ServiceInstances = []*instance.Instance{serviceInstance}

	election := member.election
	campaign := &serviceElectionCampaign{done: make(chan struct{})}
	election.mutex.Lock()
	election.campaign = campaign
	svcLease := election.lease
	election.mutex.Unlock()
	svcLease.ElectionStarted()

	election.stopCampaign(svcLease, campaign)
	if got := p.findServiceInstance(service); got != serviceInstance {
		t.Fatal("campaign stop detached an inactive member's instance")
	}
	election.mutex.Lock()
	active := member.active
	election.mutex.Unlock()
	if active {
		t.Fatal("inactive member became active during campaign stop")
	}
	p.leaveServiceElection(member)
}

func TestCancelledContextDrainsActiveMemberBeforeReplacement(t *testing.T) {
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	svcCtx := servicecontext.New(context.Background())
	if !svcCtx.StartWatching() {
		t.Fatal("watcher ownership was not acquired")
	}
	p.svcMap.Store(service.UID, svcCtx)
	p.ServiceInstances = []*instance.Instance{{ServiceUID: service.UID, ServiceSnapshot: service.DeepCopy(), AddCalled: true}}
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	member, joined := p.joinServiceElection(svcCtx, service, generation)
	if !joined {
		t.Fatal("Service did not join election")
	}
	member.election.mutex.Lock()
	member.active = true
	member.election.mutex.Unlock()
	svcCtx.Cancel()

	replacement := make(chan *servicecontext.Context, 1)
	errs := make(chan error, 1)
	go func() {
		current, err := p.ensureServiceContext(context.Background(), service)
		if err != nil {
			errs <- err
			return
		}
		replacement <- current
	}()
	select {
	case <-replacement:
		t.Fatal("replacement was created before active-member cleanup")
	case err := <-errs:
		t.Fatalf("ensureServiceContext() error = %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	member.election.deactivateMember(member)
	p.leaveServiceElection(member)
	svcCtx.StopWatching()

	select {
	case current := <-replacement:
		if current == svcCtx || current.Ctx.Err() != nil {
			t.Fatal("replacement context is not live")
		}
	case err := <-errs:
		t.Fatalf("ensureServiceContext() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("replacement was not created after cleanup drained")
	}
	if p.findServiceInstance(service) != nil {
		t.Fatal("active member datapath was not cleaned before replacement")
	}
}

func TestServiceElectionDelayedCleanupDoesNotDeleteNewReadinessGeneration(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)
	p.ServiceInstances = []*instance.Instance{{ServiceUID: service.UID, ServiceSnapshot: service.DeepCopy()}}
	svcCtx.SignalReadiness()
	firstGeneration, _, _, _ := svcCtx.ReadinessState()
	first, joined := p.joinServiceElection(svcCtx, service, firstGeneration)
	if !joined {
		t.Fatal("first readiness generation did not join")
	}

	resetServiceReadiness(t, svcCtx)
	svcCtx.SignalReadiness()
	secondGeneration, _, _, _ := svcCtx.ReadinessState()
	second, joined := p.joinServiceElection(svcCtx, service, secondGeneration)
	if !joined {
		t.Fatal("second readiness generation did not join")
	}
	if err := p.onStoppedLeadingMember(first, first.election.lease); err != nil {
		t.Fatalf("delayed old-generation cleanup returned an error: %v", err)
	}
	if p.findServiceInstance(service) == nil {
		t.Fatal("delayed old-generation cleanup deleted the current Service instance")
	}
	p.leaveServiceElection(second)
}

// TestServiceElectionRestartDelayBacksOffOnRepeatedFailuresAndResets guards
// against a persistent activation failure turning into a tight Lease
// acquire/release and VIP add/delete loop: the restart delay must grow after
// a failure and drop back to the base delay once activation succeeds.
func TestServiceElectionRestartDelayBacksOffOnRepeatedFailuresAndResets(t *testing.T) {
	id := lease.NewID("kubernetes", "default", "svc")
	svcLease := lease.NewManager().Add(context.Background(), id)
	campaign := &serviceElectionCampaign{done: make(chan struct{})}
	election := &serviceElection{lease: svcLease, campaign: campaign}

	baseDelay := serviceElectionRestartBaseDelay
	election.cancelCampaign(svcLease, campaign)

	election.mutex.Lock()
	afterFailure := election.restartDelayLocked()
	election.mutex.Unlock()
	if afterFailure <= baseDelay {
		t.Fatalf("restart delay did not grow after a failure: base=%v after=%v", baseDelay, afterFailure)
	}

	election.resetRestartFailures()
	election.mutex.Lock()
	afterReset := election.restartDelayLocked()
	election.mutex.Unlock()
	if afterReset != baseDelay {
		t.Fatalf("restart delay was not reset after a successful activation: got=%v want=%v", afterReset, baseDelay)
	}
}

func TestServiceElectionRestartTimerStopsOnRetirement(t *testing.T) {
	p := &Processor{}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	restarted := make(chan struct{}, 1)
	p.scheduleServiceElectionRestart(ctx, time.Hour, &wg, func() { restarted <- struct{}{} })
	cancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("restart timer did not drain after coordinator retirement")
	}
	select {
	case <-restarted:
		t.Fatal("restart ran after coordinator retirement")
	default:
	}
}
