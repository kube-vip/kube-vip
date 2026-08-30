package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestServicesLeaderElectionLoopRecoversFromLeaseLoss(t *testing.T) {
	runner := newElectionTestRunner()
	p := &Processor{
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
		elections:   make(map[string]*serviceElection),
		electionRun: runner.run,
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matst-example",
			Namespace: "dsm-system",
			UID:       types.UID("test-uid"),
		},
	}

	leaseNamespace, serviceLease := lease.ServiceName(svc)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)
	svcLease := p.leaseMgr.Add(context.Background(), id)

	svcLease.Cancel()

	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(svc.UID, svcCtx)
	svcCtx.SignalReadiness()

	done := make(chan error, 1)
	go func() {
		done <- p.runServicesLeaderElectionLoop(svcCtx, svc, nil, true)
	}()

	awaitCondition(t, func() bool {
		current := p.leaseMgr.Get(id)
		return current != nil && current != svcLease && current.Ctx.Err() == nil
	}, "replacement lease")
	select {
	case err := <-done:
		t.Fatalf("public election loop returned while service remained alive: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	svcCtx.Cancel()
	if err := awaitError(t, done, "public election loop cancellation"); err != nil {
		t.Fatalf("runServicesLeaderElectionLoop() error = %v", err)
	}
}

func TestSharedLeaseFollowerCancellationDoesNotStopLeader(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{EnableServicesElection: true},
		leaseMgr: lease.NewManager(),
		electionRun: func(ctx context.Context, run *election.RunConfig, _ *kubevip.Config) error {
			run.OnStartedLeading(ctx)
			<-ctx.Done()
			run.OnStoppedLeading()
			return nil
		},
	}
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	leaderService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "leader", Namespace: "default", UID: types.UID("leader"), Annotations: annotations,
	}}
	followerService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "follower", Namespace: "default", UID: types.UID("follower"), Annotations: annotations,
	}}

	leaseNamespace, serviceLease := lease.ServiceName(leaderService)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)
	sharedLease := p.leaseMgr.Add(context.Background(), id)
	sharedLease.Add(lease.ServiceClaimID(leaderService))
	if !sharedLease.BeginElection() {
		t.Fatal("leader service did not become candidate")
	}
	sharedLease.ElectionStarted()
	sharedLease.Add(lease.ServiceClaimID(followerService))

	followerCtx := servicecontext.New(context.Background())
	p.svcMap.Store(followerService.UID, followerCtx)
	followerCtx.SignalReadiness()
	done := make(chan error, 1)
	go func() {
		done <- p.StartServicesLeaderElection(followerCtx, followerService, nil, true)
	}()

	time.Sleep(10 * time.Millisecond)
	followerCtx.Cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follower election returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower election did not return after follower context cancellation")
	}

	if !sharedLease.Elected.Load() {
		t.Fatal("follower cancellation cleared the shared leader election state")
	}
	if sharedLease.BeginElection() {
		t.Fatal("follower cancellation admitted a second candidate while leader remains elected")
	}
}

func TestServicesLeaderElectionLoopRecreatesMissingLease(t *testing.T) {
	runner := newElectionTestRunner()
	p := &Processor{
		config:      &kubevip.Config{},
		leaseMgr:    lease.NewManager(),
		elections:   make(map[string]*serviceElection),
		electionRun: runner.run,
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)

	done := make(chan error, 1)
	go func() { done <- p.runServicesLeaderElectionLoop(svcCtx, service, nil, true) }()
	awaitCondition(t, func() bool {
		return p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, "default", "kubevip-service")) != nil
	}, "missing lease recreation")
	select {
	case err := <-done:
		t.Fatalf("public election loop returned after recreating lease: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	svcCtx.Cancel()
	if err := awaitError(t, done, "public election loop cancellation"); err != nil {
		t.Fatalf("runServicesLeaderElectionLoop() error = %v", err)
	}
}

func TestStartServicesLeaderElectionDoesNotClaimForCancelledContext(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	leaseNamespace, serviceLease := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)
	p.leaseMgr.Add(context.Background(), id)

	staleCtx := servicecontext.New(context.Background())
	staleCtx.Cancel()
	freshCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, freshCtx)
	if err := p.StartServicesLeaderElection(staleCtx, service, nil, true); err == nil {
		t.Fatal("cancelled service context started leader election")
	}

	claimID := lease.ServiceClaimID(service)
	claimed, isNew := p.leaseMgr.Claim(id, claimID)
	if claimed == nil || !isNew {
		t.Fatal("cancelled callback claimed the replacement lease membership")
	}
	p.leaseMgr.Delete(id, claimID, claimed)
}

func TestConcurrentStartServicesLeaderElectionRegistersOneMember(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	service := electionTestService("concurrent-start", "192.0.2.10")
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	p.leaseMgr.Add(parent, id)
	svcCtx := servicecontext.New(parent)
	p.svcMap.Store(service.UID, svcCtx)
	svcCtx.SignalReadiness()

	const callers = 100
	start := make(chan struct{})
	errs := make(chan error, callers)
	var entered atomic.Int64
	unlockStart := p.lockService(service.UID)
	for range callers {
		go func() {
			<-start
			entered.Add(1)
			errs <- p.StartServicesLeaderElection(svcCtx, service, nil, true)
		}()
	}
	close(start)
	awaitCondition(t, func() bool { return entered.Load() == callers }, "all concurrent callers started")
	unlockStart()
	await(t, runner.started, "concurrent campaign")
	awaitCondition(t, func() bool {
		c, member := p.serviceElectionMember(service, svcCtx)
		if member == nil {
			return false
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.members) == 1
	}, "single concurrent member")
	time.Sleep(25 * time.Millisecond)
	svcCtx.Cancel()
	for range callers {
		if err := awaitError(t, errs, "concurrent StartServices return"); err != nil {
			t.Fatalf("StartServicesLeaderElection() error = %v", err)
		}
	}
	awaitCondition(t, func() bool { return p.leaseMgr.Get(id) == nil }, "single member lease retirement")
}

func TestStartServicesLeaderElectionStaleContextReturnsPromptly(t *testing.T) {
	p := &Processor{config: &kubevip.Config{}, leaseMgr: lease.NewManager()}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: "default", UID: types.UID("stale")}}
	stale := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, servicecontext.New(context.Background()))

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(stale, service, nil, true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stale context returned nil error")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("stale direct StartServices call did not return promptly")
	}
}

func TestStartServicesLeaderElectionUsesStableSharedParent(t *testing.T) {
	runner := newElectionTestRunner()
	p := newElectionTestProcessor(runner.run)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := electionTestService("first", "10.0.0.1")
	second := electionTestService("second", "10.0.0.2")
	namespace, name := lease.ServiceName(first)
	p.leaseMgr.Add(parent, lease.NewID(p.config.LeaderElectionType, namespace, name))
	firstCtx := servicecontext.New(parent)
	secondCtx := servicecontext.New(parent)
	p.svcMap.Store(first.UID, firstCtx)
	p.svcMap.Store(second.UID, secondCtx)
	firstCtx.SignalReadiness()
	secondCtx.SignalReadiness()
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- p.StartServicesLeaderElection(firstCtx, first, nil, true) }()
	go func() { secondDone <- p.StartServicesLeaderElection(secondCtx, second, nil, true) }()
	awaitCondition(t, func() bool {
		return p.serviceElectionMemberExists(first, firstCtx) && p.serviceElectionMemberExists(second, secondCtx)
	}, "shared members")

	firstCtx.Cancel()
	if err := awaitError(t, firstDone, "first service cancellation"); err != nil {
		t.Fatalf("first StartServicesLeaderElection() error = %v", err)
	}
	if secondCtx.Parent().Err() != nil || secondCtx.Ctx.Err() != nil {
		t.Fatal("canceling first service canceled its sibling or stable parent")
	}
	secondCtx.Cancel()
	if err := awaitError(t, secondDone, "second service cancellation"); err != nil {
		t.Fatalf("second StartServicesLeaderElection() error = %v", err)
	}
}

func TestStartServicesLeaderElectionRejectsTypedNilService(t *testing.T) {
	p := &Processor{}
	var service *v1.Service
	if err := p.StartServicesLeaderElection(servicecontext.New(context.Background()), service, nil, true); err == nil {
		t.Fatal("typed nil service was accepted")
	}
}

func TestReleaseServiceLeaseDoesNotRemoveSharedLeaseReplacement(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"), Annotations: annotations,
	}}
	leaseNamespace, serviceLease := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)
	sharedLease := p.leaseMgr.Add(context.Background(), id)
	sharedLease.Add("default/sibling")

	oldCtx := servicecontext.New(context.Background())
	claimID := lease.ServiceClaimID(service)
	sharedLease.Add(claimID)
	oldCtx.Cancel()

	// Reconcile drops the cancelled context's membership before installing its replacement.
	p.leaseMgr.Delete(id, claimID, sharedLease)
	freshCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, freshCtx)
	if _, isNew := p.leaseMgr.Claim(id, claimID); !isNew {
		t.Fatal("replacement context did not claim shared lease membership")
	}

	p.releaseServiceLease(oldCtx, service, id, claimID, sharedLease)
	p.leaseMgr.Delete(id, "default/sibling", sharedLease)
	if p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("stale cleanup removed the replacement membership from the shared lease")
	}

	p.leaseMgr.Delete(id, claimID, sharedLease)
}

func TestReleaseServiceLeaseDoesNotRemoveRecreatedServiceClaim(t *testing.T) {
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
	leaseNamespace, serviceLease := lease.ServiceName(oldService)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)
	sharedLease := p.leaseMgr.Add(context.Background(), id)
	sharedLease.Add("default/sibling")

	oldCtx := servicecontext.New(context.Background())
	p.svcMap.Store(oldService.UID, oldCtx)
	oldClaimID := lease.ServiceClaimID(oldService)
	if _, isNew := p.leaseMgr.Claim(id, oldClaimID); !isNew {
		t.Fatal("old Service did not claim shared lease membership")
	}
	oldCtx.Cancel()
	p.leaseMgr.Delete(id, oldClaimID, sharedLease)

	newCtx := servicecontext.New(context.Background())
	p.svcMap.Store(newService.UID, newCtx)
	newClaimID := lease.ServiceClaimID(newService)
	if _, isNew := p.leaseMgr.Claim(id, newClaimID); !isNew {
		t.Fatal("recreated Service did not claim shared lease membership")
	}

	p.releaseServiceLease(oldCtx, oldService, id, oldClaimID, sharedLease)
	p.leaseMgr.Delete(id, "default/sibling", sharedLease)
	if p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("late cleanup from old Service removed the recreated Service claim")
	}

	p.leaseMgr.Delete(id, newClaimID, sharedLease)
}
