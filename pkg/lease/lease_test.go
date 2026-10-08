package lease

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func createTestService(name, namespace string, annotations map[string]string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

func getSvcID(svc *v1.Service) ID {
	namespace, name := ServiceName(svc)
	id := NewID("kubernetes", namespace, name)
	return id
}

func getSvcData(svc *v1.Service) (context.Context, ID) {
	return context.TODO(), getSvcID(svc)
}

const serviceLeaseAnnotation = kubevip.ServiceLease

func TestServiceNameForMatchesServiceName(t *testing.T) {
	for _, test := range []struct {
		name          string
		namespace     string
		service       string
		lease         string
		wantNamespace string
		wantName      string
	}{
		{name: "default lease", namespace: "default", service: "api", wantNamespace: "default", wantName: "kubevip-api"},
		{name: "named lease", namespace: "default", service: "api", lease: "shared", wantNamespace: "default", wantName: "shared"},
		{name: "cross-namespace lease", namespace: "default", service: "api", lease: "leases/shared", wantNamespace: "leases", wantName: "shared"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := createTestService(test.service, test.namespace, map[string]string{kubevip.ServiceLease: test.lease})
			for name, serviceName := range map[string]func() (string, string){
				"ServiceName":    func() (string, string) { return ServiceName(service) },
				"ServiceNameFor": func() (string, string) { return ServiceNameFor(test.namespace, test.service, test.lease) },
			} {
				namespace, leaseName := serviceName()
				if namespace != test.wantNamespace || leaseName != test.wantName {
					t.Errorf("%s() = %s/%s, want %s/%s", name, namespace, leaseName, test.wantNamespace, test.wantName)
				}
			}
		})
	}
}

func electLease(t *testing.T, lease *Lease) *ElectionSession {
	t.Helper()
	participation := lease.JoinElection()
	if !participation.RunsCampaign() {
		t.Fatal("expected lease to admit an election candidate")
	}
	election := participation.Session
	if !election.Started() {
		t.Fatal("expected election candidate to become leader")
	}
	return election
}

func newTestLease(t *testing.T) *Lease {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newLease(ctx, cancel)
}

func TestManagerAcquireRegistersMembership(t *testing.T) {
	manager := NewManager()
	service := createTestService("service", "default", nil)
	id := getSvcID(service)
	objectName := ServiceNamespacedName(service)

	lease, first := manager.Acquire(context.Background(), id, objectName, nil)
	if !first {
		t.Fatal("first acquire did not register the service")
	}
	if _, second := manager.Acquire(context.Background(), id, objectName, nil); second {
		t.Fatal("second acquire registered the same service twice")
	}

	manager.Delete(id, objectName, lease)
	if manager.Get(id) != nil {
		t.Fatal("lease remained after its only acquired member was deleted")
	}
}

func TestAcquireRegistrationsRejectsWholeInvalidGroup(t *testing.T) {
	manager := NewManager()
	id := NewID("kubernetes", "default", "shared")
	specs := []RegistrationSpec{
		{Name: "duplicate", VIPProvider: StaticVIPProvider([]string{"192.0.2.1"})},
		{Name: "duplicate", VIPProvider: StaticVIPProvider([]string{"192.0.2.2"})},
	}
	if registeredLease, registrations, err := manager.AcquireRegistrations(context.Background(), id, specs); err == nil ||
		registeredLease != nil || registrations != nil {
		t.Fatalf("AcquireRegistrations() = (%v, %v, %v), want an atomic rejection", registeredLease, registrations, err)
	}
	if manager.Get(id) != nil {
		t.Fatal("invalid registration group left a partially populated Lease")
	}
}

func TestAcquireRegistrationsPublishesWholeGroup(t *testing.T) {
	manager := NewManager()
	id := NewID("kubernetes", "default", "shared")
	specs := []RegistrationSpec{
		{Name: "first", VIPProvider: StaticVIPProvider([]string{"192.0.2.1"})},
		{Name: "second", VIPProvider: StaticVIPProvider([]string{"192.0.2.2"})},
	}
	registeredLease, registrations, err := manager.AcquireRegistrations(context.Background(), id, specs)
	if err != nil {
		t.Fatalf("AcquireRegistrations() error = %v", err)
	}
	if len(registrations) != len(specs) {
		t.Fatalf("registration count = %d, want %d", len(registrations), len(specs))
	}
	if got, want := registeredLease.OwnedVIPs(), []string{"192.0.2.1", "192.0.2.2"}; !slices.Equal(got, want) {
		t.Fatalf("OwnedVIPs() = %v, want %v", got, want)
	}
	for _, registration := range registrations {
		registration.Release()
	}
}

func TestLeaseOwnedVIPsAggregatesDynamicMemberProviders(t *testing.T) {
	manager := NewManager()
	id := NewID("kubernetes", "default", "shared")
	controlPlaneVIPs := []string{"192.0.2.10"}
	serviceVIPs := []string{"192.0.2.20", "192.0.2.10"}

	sharedLease, added := manager.Acquire(context.Background(), id, "control-plane", func() []string {
		return append([]string(nil), controlPlaneVIPs...)
	})
	if !added {
		t.Fatal("control-plane member was not registered")
	}
	claimed, added := manager.ClaimWithVIPProvider(id, "service", func() []string {
		return append([]string(nil), serviceVIPs...)
	})
	if claimed != sharedLease || !added {
		t.Fatal("Service member did not join the shared Lease")
	}

	if got, want := sharedLease.OwnedVIPs(), []string{"192.0.2.10", "192.0.2.20"}; !slices.Equal(got, want) {
		t.Fatalf("OwnedVIPs() = %v, want %v", got, want)
	}

	serviceVIPs = []string{"192.0.2.30"}
	if got, want := sharedLease.OwnedVIPs(), []string{"192.0.2.10", "192.0.2.30"}; !slices.Equal(got, want) {
		t.Fatalf("OwnedVIPs() after provider update = %v, want %v", got, want)
	}

	if manager.Delete(id, "service", sharedLease) {
		t.Fatal("deleting the Service member retired a shared Lease")
	}
	if got, want := sharedLease.OwnedVIPs(), []string{"192.0.2.10"}; !slices.Equal(got, want) {
		t.Fatalf("OwnedVIPs() after member deletion = %v, want %v", got, want)
	}
}

func TestElectionContextCancellationDoesNotCancelSharedLease(t *testing.T) {
	manager := NewManager()
	id := NewID("kubernetes", "default", "shared")
	sharedLease, _ := manager.Acquire(context.Background(), id, "control-plane", nil)
	if claimed, _ := manager.ClaimWithVIPProvider(id, "service", nil); claimed != sharedLease {
		t.Fatal("second member did not join the shared lease")
	}

	electionCtx, cancelElection := sharedLease.NewElectionContext(context.Background())
	cancelElection()
	select {
	case <-electionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("election context was not cancelled")
	}
	if sharedLease.Ctx.Err() != nil || manager.Get(id) != sharedLease {
		t.Fatal("cancelling one election runner cancelled the shared lease")
	}

	memberCtx, cancelMember := sharedLease.NewElectionContext(context.Background())
	defer cancelMember()
	if manager.Delete(id, "control-plane", sharedLease) {
		t.Fatal("deleting one member retired a shared lease")
	}
	if !manager.Delete(id, "service", sharedLease) {
		t.Fatal("deleting the final member did not retire the lease")
	}
	select {
	case <-memberCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("retiring the shared lease did not cancel an election context")
	}
}

func TestWaitForElectionEndObservesRapidRestart(t *testing.T) {
	serviceLease := newTestLease(t)
	firstParticipation := serviceLease.JoinElection()
	if !firstParticipation.RunsCampaign() {
		t.Fatal("first election did not start")
	}
	first := firstParticipation.Session
	if !first.Started() {
		t.Fatal("first election did not become leader")
	}

	observerParticipation := serviceLease.JoinElection()
	observer := observerParticipation.Session
	if observerParticipation.RunsCampaign() || !observer.WaitForLeader(context.Background()) {
		t.Fatal("waiter did not observe the elected lease")
	}
	done := make(chan struct{})
	go func() {
		observer.WaitForEnd(context.Background())
		close(done)
	}()
	first.Stopped()
	replacementParticipation := serviceLease.JoinElection()
	if !replacementParticipation.RunsCampaign() {
		t.Fatal("replacement election did not start")
	}
	replacement := replacementParticipation.Session
	if !replacement.Started() {
		t.Fatal("replacement election did not become leader")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiter missed election end during rapid restart")
	}
}

func TestManagerClaimDoesNotCreateRetiredLease(t *testing.T) {
	manager := NewManager()
	service := createTestService("service", "default", nil)
	if lease, joined := manager.ClaimWithVIPProvider(getSvcID(service), ServiceNamespacedName(service), nil); lease != nil || joined {
		t.Fatal("claim created or joined a lease that does not exist")
	}
}

func TestLeaseElectionStateCoordinatesCandidates(t *testing.T) {
	lease := newTestLease(t)

	participation := lease.JoinElection()
	if !participation.RunsCampaign() {
		t.Fatal("first election candidate was not admitted")
	}
	election := participation.Session
	observerParticipation := lease.JoinElection()
	if observerParticipation.RunsCampaign() {
		t.Fatal("second election candidate was admitted while election was running")
	}
	observer := observerParticipation.Session

	joined := make(chan bool, 1)
	go func() {
		joined <- observer.WaitForLeader(context.Background())
	}()
	election.Started()
	select {
	case elected := <-joined:
		if !elected {
			t.Fatal("follower did not observe elected lease")
		}
	case <-time.After(time.Second):
		t.Fatal("follower remained blocked after election succeeded")
	}

	election.Stopped()
	if !lease.JoinElection().RunsCampaign() {
		t.Fatal("lease did not admit a new candidate after election stopped")
	}
}

func TestElectionSessionStaleStopCannotStopReplacement(t *testing.T) {
	serviceLease := newTestLease(t)

	firstParticipation := serviceLease.JoinElection()
	first := firstParticipation.Session
	if !firstParticipation.RunsCampaign() || !first.Started() {
		t.Fatal("first election session did not become leader")
	}
	observerParticipation := serviceLease.JoinElection()
	if observerParticipation.RunsCampaign() {
		t.Fatal("observer acquired an election that was already leading")
	}
	observer := observerParticipation.Session
	if observer.Started() || observer.Stopped() {
		t.Fatal("observer was allowed to mutate election state")
	}

	if !first.Stopped() {
		t.Fatal("first election session did not stop")
	}
	secondParticipation := serviceLease.JoinElection()
	second := secondParticipation.Session
	if !secondParticipation.RunsCampaign() || !second.Started() {
		t.Fatal("replacement election session did not become leader")
	}
	if first.Stopped() {
		t.Fatal("stale session stopped the replacement election")
	}
	if first.Started() {
		t.Fatal("stale session restarted after it had been replaced")
	}
	if !second.IsLeading() {
		t.Fatal("replacement election lost leadership after stale callbacks")
	}
}

func TestElectionSessionWaitForLeaderDoesNotAdoptReplacement(t *testing.T) {
	serviceLease := newTestLease(t)

	firstParticipation := serviceLease.JoinElection()
	if !firstParticipation.RunsCampaign() {
		t.Fatal("first election session did not acquire the runner")
	}
	first := firstParticipation.Session
	observerParticipation := serviceLease.JoinElection()
	if observerParticipation.RunsCampaign() {
		t.Fatal("observer acquired an election that was already campaigning")
	}
	observer := observerParticipation.Session
	if !first.Stopped() {
		t.Fatal("first election session did not stop")
	}
	secondParticipation := serviceLease.JoinElection()
	second := secondParticipation.Session
	if !secondParticipation.RunsCampaign() || !second.Started() {
		t.Fatal("replacement election session did not become leader")
	}

	if observer.WaitForLeader(context.Background()) {
		t.Fatal("observer of the first generation adopted replacement leadership")
	}
}

func TestElectionSessionWaitForEndObservesRapidReplacement(t *testing.T) {
	serviceLease := newTestLease(t)

	firstParticipation := serviceLease.JoinElection()
	first := firstParticipation.Session
	if !firstParticipation.RunsCampaign() || !first.Started() {
		t.Fatal("first election session did not become leader")
	}
	done := make(chan struct{})
	go func() {
		first.WaitForEnd(context.Background())
		close(done)
	}()

	first.Stopped()
	secondParticipation := serviceLease.JoinElection()
	second := secondParticipation.Session
	if !secondParticipation.RunsCampaign() || !second.Started() {
		t.Fatal("replacement election session did not become leader")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiter missed the end of its generation during rapid replacement")
	}
}

func TestElectionSessionBroadcastsTransitionsToAllObservers(t *testing.T) {
	serviceLease := newTestLease(t)

	ownerParticipation := serviceLease.JoinElection()
	if !ownerParticipation.RunsCampaign() {
		t.Fatal("first election session did not acquire the runner")
	}
	ownerSession := ownerParticipation.Session

	const observerCount = 32
	observers := make([]*ElectionSession, 0, observerCount)
	for range observerCount {
		observerParticipation := serviceLease.JoinElection()
		if observerParticipation.RunsCampaign() {
			t.Fatal("observer acquired an election that was already campaigning")
		}
		observer := observerParticipation.Session
		observers = append(observers, observer)
	}

	leaderResults := make(chan bool, observerCount)
	var leaderWaiters sync.WaitGroup
	for _, observer := range observers {
		leaderWaiters.Go(func() {
			leaderResults <- observer.WaitForLeader(context.Background())
		})
	}
	if !ownerSession.Started() {
		t.Fatal("owner session did not become leader")
	}
	waitForElectionBoolResults(t, leaderResults, observerCount, true)
	leaderWaiters.Wait()

	ended := make(chan struct{}, observerCount)
	var endWaiters sync.WaitGroup
	for _, observer := range observers {
		endWaiters.Go(func() {
			observer.WaitForEnd(context.Background())
			ended <- struct{}{}
		})
	}
	if !ownerSession.Stopped() {
		t.Fatal("owner session did not stop")
	}
	for range observerCount {
		select {
		case <-ended:
		case <-time.After(time.Second):
			t.Fatal("not every observer saw the election end")
		}
	}
	endWaiters.Wait()
}

func TestElectionSessionBroadcastsCandidateStopToAllObservers(t *testing.T) {
	serviceLease := newTestLease(t)

	ownerParticipation := serviceLease.JoinElection()
	if !ownerParticipation.RunsCampaign() {
		t.Fatal("first election session did not acquire the runner")
	}
	ownerSession := ownerParticipation.Session

	const observerCount = 32
	results := make(chan bool, observerCount)
	var waiters sync.WaitGroup
	for range observerCount {
		observerParticipation := serviceLease.JoinElection()
		if observerParticipation.RunsCampaign() {
			t.Fatal("observer acquired an election that was already campaigning")
		}
		observer := observerParticipation.Session
		waiters.Go(func() {
			results <- observer.WaitForLeader(context.Background())
		})
	}
	if !ownerSession.Stopped() {
		t.Fatal("owner session did not stop")
	}
	waitForElectionBoolResults(t, results, observerCount, false)
	waiters.Wait()
}

func TestElectionSessionPreservesRapidStartStopResult(t *testing.T) {
	serviceLease := newTestLease(t)

	ownerParticipation := serviceLease.JoinElection()
	if !ownerParticipation.RunsCampaign() {
		t.Fatal("first election session did not acquire the runner")
	}
	ownerSession := ownerParticipation.Session
	observerParticipation := serviceLease.JoinElection()
	if observerParticipation.RunsCampaign() {
		t.Fatal("observer acquired an election that was already campaigning")
	}
	observer := observerParticipation.Session
	if !ownerSession.Started() || !ownerSession.Stopped() {
		t.Fatal("owner session did not complete its leadership transition")
	}
	if observer.WaitForLeader(context.Background()) {
		t.Fatal("observer reported leadership after the generation had already stopped")
	}
}

func TestElectionSessionWaitForEndReturnsBeforeLeadership(t *testing.T) {
	serviceLease := newTestLease(t)

	participation := serviceLease.JoinElection()
	if !participation.RunsCampaign() {
		t.Fatal("first election session did not acquire the runner")
	}
	election := participation.Session
	done := make(chan struct{})
	go func() {
		election.WaitForEnd(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WaitForEnd blocked before the session became leader")
	}
}

func waitForElectionBoolResults(t *testing.T, results <-chan bool, count int, want bool) {
	t.Helper()
	for range count {
		select {
		case got := <-results:
			if got != want {
				t.Fatalf("WaitForLeader() = %t, want %t", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("not every observer received the election transition")
		}
	}
}

func TestLeaseWaitForLeaderReturnsWhenCandidateStops(t *testing.T) {
	lease := newTestLease(t)
	participation := lease.JoinElection()
	if !participation.RunsCampaign() {
		t.Fatal("candidate was not admitted")
	}
	election := participation.Session
	observerParticipation := lease.JoinElection()
	if observerParticipation.RunsCampaign() {
		t.Fatal("observer acquired an election that was already campaigning")
	}
	observer := observerParticipation.Session

	joined := make(chan bool, 1)
	go func() {
		joined <- observer.WaitForLeader(context.Background())
	}()
	election.Stopped()
	select {
	case elected := <-joined:
		if elected {
			t.Fatal("follower observed a leader after candidate stopped")
		}
	case <-time.After(time.Second):
		t.Fatal("follower remained blocked after candidate stopped")
	}
}

// TestLeaseSupportsRetakingElectionAfterCandidateStops exercises the retry
// StartCluster relies on for a shared control-plane/Services lease: once
// WaitForLeader reports the campaign ended without ever electing a leader, a
// waiter must be able to begin its own election immediately instead of being
// left with no active runner.
func TestLeaseSupportsRetakingElectionAfterCandidateStops(t *testing.T) {
	lease := newTestLease(t)
	participation := lease.JoinElection()
	if !participation.RunsCampaign() {
		t.Fatal("candidate was not admitted")
	}
	election := participation.Session
	observerParticipation := lease.JoinElection()
	if observerParticipation.RunsCampaign() {
		t.Fatal("observer acquired an election that was already campaigning")
	}
	observer := observerParticipation.Session

	retried := make(chan bool, 1)
	go func() {
		if observer.WaitForLeader(context.Background()) {
			retried <- false
			return
		}
		retried <- lease.JoinElection().RunsCampaign()
	}()
	election.Stopped()

	select {
	case tookOver := <-retried:
		if !tookOver {
			t.Fatal("waiter could not begin its own election after the shared campaign ended without a leader")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter remained blocked after candidate stopped")
	}
}

func TestLeaseWaitForElectionEndReleasesFollowers(t *testing.T) {
	lease := newTestLease(t)
	participation := lease.JoinElection()
	if !participation.RunsCampaign() {
		t.Fatal("candidate was not admitted")
	}
	election := participation.Session
	election.Started()

	finished := make(chan struct{})
	go func() {
		election.WaitForEnd(context.Background())
		close(finished)
	}()
	election.Stopped()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("follower remained blocked after leadership stopped")
	}
}

func TestLeaseWaitForLeaderReturnsWhenContextCancelled(t *testing.T) {
	for _, cancelWait := range []struct {
		name   string
		cancel func(context.CancelFunc, context.CancelFunc)
	}{
		{"caller context", func(cancelCaller, _ context.CancelFunc) { cancelCaller() }},
		{"lease context", func(_, cancelLease context.CancelFunc) { cancelLease() }},
	} {
		t.Run(cancelWait.name, func(t *testing.T) {
			leaseCtx, cancelLease := context.WithCancel(context.Background())
			defer cancelLease()
			lease := newLease(leaseCtx, cancelLease)
			participation := lease.JoinElection()
			if !participation.RunsCampaign() {
				t.Fatal("candidate was not admitted")
			}
			election := participation.Session

			callerCtx, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			result := make(chan bool, 1)
			go func() {
				result <- election.WaitForLeader(callerCtx)
			}()

			cancelWait.cancel(cancelCaller, cancelLease)
			select {
			case elected := <-result:
				if elected {
					t.Fatal("waiter observed a leader after cancellation")
				}
			case <-time.After(time.Second):
				t.Fatal("waiter remained blocked after cancellation")
			}
		})
	}
}

func TestLeaseWaitForElectionEndReturnsWhenContextCancelled(t *testing.T) {
	for _, cancelWait := range []struct {
		name   string
		cancel func(context.CancelFunc, context.CancelFunc)
	}{
		{"caller context", func(cancelCaller, _ context.CancelFunc) { cancelCaller() }},
		{"lease context", func(_, cancelLease context.CancelFunc) { cancelLease() }},
	} {
		t.Run(cancelWait.name, func(t *testing.T) {
			leaseCtx, cancelLease := context.WithCancel(context.Background())
			defer cancelLease()
			lease := newLease(leaseCtx, cancelLease)
			election := electLease(t, lease)

			callerCtx, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			finished := make(chan struct{})
			go func() {
				election.WaitForEnd(callerCtx)
				close(finished)
			}()

			cancelWait.cancel(cancelCaller, cancelLease)
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("waiter remained blocked after cancellation")
			}
		})
	}
}

func TestManagerAcquireRegistersConcurrentMemberOnce(t *testing.T) {
	manager := NewManager()
	service := createTestService("service", "default", nil)
	id := getSvcID(service)
	objectName := ServiceNamespacedName(service)

	type result struct {
		lease *Lease
		isNew bool
	}
	results := make(chan result, 64)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() {
			lease, isNew := manager.Acquire(context.Background(), id, objectName, nil)
			results <- result{lease, isNew}
		})
	}
	wg.Wait()
	close(results)

	var lease *Lease
	newMembers := 0
	for result := range results {
		if lease == nil {
			lease = result.lease
		} else if result.lease != lease {
			t.Fatal("concurrent acquires returned different leases")
		}
		if result.isNew {
			newMembers++
		}
	}
	if newMembers != 1 {
		t.Fatalf("new member registrations = %d, want 1", newMembers)
	}

	manager.Delete(id, objectName, lease)
}

func TestManagerClaimRegistersConcurrentMemberOnce(t *testing.T) {
	manager := NewManager()
	service := createTestService("service", "default", nil)
	id := getSvcID(service)
	objectName := ServiceNamespacedName(service)
	const existingMember = "existing"
	lease, added := manager.Acquire(context.Background(), id, existingMember, nil)
	if !added {
		t.Fatal("existing Lease participant was not registered")
	}

	type result struct {
		lease *Lease
		isNew bool
	}
	results := make(chan result, 64)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() {
			claimed, isNew := manager.ClaimWithVIPProvider(id, objectName, nil)
			results <- result{claimed, isNew}
		})
	}
	wg.Wait()
	close(results)

	newMembers := 0
	for result := range results {
		if result.lease != lease {
			t.Fatal("concurrent claims returned a different lease")
		}
		if result.isNew {
			newMembers++
		}
	}
	if newMembers != 1 {
		t.Fatalf("new member registrations = %d, want 1", newMembers)
	}

	if manager.Delete(id, objectName, lease) {
		t.Fatal("claimed member retired a Lease still held by the existing participant")
	}
	if !manager.Delete(id, existingMember, lease) {
		t.Fatal("final participant did not retire the Lease")
	}
}

// TestManager_Acquire_NewLease tests acquiring a new Lease for a new Service.
func TestManager_Acquire_NewLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	ctx, id := getSvcData(svc)
	serviceLease, isNew := mgr.Acquire(ctx, id, ServiceNamespacedName(svc), nil)

	if !isNew {
		t.Error("expected isNew to be true for first Acquire")
	}
	if serviceLease == nil {
		t.Fatal("expected lease to be non-nil")
	}
	if serviceLease.Ctx == nil {
		t.Error("expected lease context to be non-nil")
	}
	if serviceLease.Cancel == nil {
		t.Error("expected lease cancel func to be non-nil")
	}
}

// TestManager_Acquire_ExistingRegistration tests acquiring an already registered Service.
func TestManager_Acquire_ExistingRegistration(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)
	ctx, id := getSvcData(svc)
	objectName := ServiceNamespacedName(svc)

	first, isNew1 := mgr.Acquire(ctx, id, objectName, nil)
	second, isNew2 := mgr.Acquire(ctx, id, objectName, nil)

	if !isNew1 {
		t.Error("expected first Acquire to return isNew=true")
	}
	if isNew2 {
		t.Error("expected second Acquire to return isNew=false")
	}
	if first != second {
		t.Error("expected same lease to be returned for same service")
	}
}

// TestManager_DuplicateAcquireDoesNotAddRegistration verifies idempotent registration.
func TestManager_DuplicateAcquireDoesNotAddRegistration(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	// Acquire twice, simulating duplicate processing of the same Service.
	objectName := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, first := mgr.Acquire(ctx1, leaseID1, objectName, nil)

	ctx2, leaseID2 := getSvcData(svc)
	lease2, second := mgr.Acquire(ctx2, leaseID2, objectName, nil)
	if !first || second || lease1 != lease2 {
		t.Fatalf("duplicate Acquire results: first=%v second=%v same lease=%v", first, second, lease1 == lease2)
	}

	// One Delete removes the single registration created by both Acquire calls.
	mgr.Delete(leaseID1, objectName, nil)

	lease := mgr.Get(getSvcID(svc))
	if lease != nil {
		t.Error("expected lease to be removed after first delete if same service was processed twice")
	}
}

// TestManager_Delete_CancelsContext tests the context cancellation on delete
func TestManager_Delete_CancelsContext(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	objectName := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, _ := mgr.Acquire(ctx1, leaseID1, objectName, nil)

	// Verify context is not cancelled
	select {
	case <-lease1.Ctx.Done():
		t.Fatal("expected context to not be cancelled initially")
	default:
		// Expected
	}

	// Delete the lease
	mgr.Delete(leaseID1, objectName, nil)

	// Verify context is cancelled
	select {
	case <-lease1.Ctx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("expected context to be cancelled after delete")
	}
}

// TestManager_Acquire_AfterDelete_CreatesNewLease tests acquiring a Service after deleting it.
func TestManager_Acquire_AfterDelete_CreatesNewLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	objectName := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, _ := mgr.Acquire(ctx1, leaseID1, objectName, nil)

	mgr.Delete(leaseID1, objectName, nil)

	ctx2, leaseID2 := getSvcData(svc)
	lease2, isNew := mgr.Acquire(ctx2, leaseID2, objectName, nil)

	if !isNew {
		t.Error("expected isNew to be true after delete and re-add")
	}
	if lease1 == lease2 {
		t.Error("expected new lease to be different from old lease")
	}
}

// TestManager_Acquire_DifferentServices tests acquiring Services with different names.
func TestManager_Acquire_DifferentServices(t *testing.T) {
	mgr := NewManager()
	svc1 := createTestService("svc1", "default", nil)
	svc2 := createTestService("svc2", "default", nil)

	objectName1 := ServiceNamespacedName(svc1)

	ctx1, leaseID1 := getSvcData(svc1)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)

	objectName2 := ServiceNamespacedName(svc2)

	ctx2, leaseID2 := getSvcData(svc2)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName2, nil)

	if !isNew1 || !isNew2 {
		t.Error("expected both acquires to return isNew=true")
	}
	if lease1 == lease2 {
		t.Error("expected different leases for different services")
	}
}

// TestManager_Acquire_SameNameDifferentNamespace tests acquiring Services with the same name in different namespaces.
func TestManager_Acquire_SameNameDifferentNamespace(t *testing.T) {
	mgr := NewManager()
	svc1 := createTestService("test-svc", "namespace1", nil)
	svc2 := createTestService("test-svc", "namespace2", nil)

	objectName1 := ServiceNamespacedName(svc1)

	ctx1, leaseID1 := getSvcData(svc1)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)

	objectName2 := ServiceNamespacedName(svc2)

	ctx2, leaseID2 := getSvcData(svc2)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName2, nil)

	if !isNew1 || !isNew2 {
		t.Error("expected both acquires to return isNew=true")
	}
	if lease1 == lease2 {
		t.Error("expected different leases for services in different namespaces")
	}
}

// TestManager_ConcurrentAcquire tests concurrent acquisition of one registration.
func TestManager_ConcurrentAcquire(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	var wg sync.WaitGroup
	const numGoroutines = 100

	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, added := mgr.Acquire(ctx1, leaseID1, objectName1, nil)
	if !added {
		t.Error("expected lease to be added")
	}

	// Concurrent duplicate acquires must observe the existing registration.
	for range numGoroutines {
		wg.Go(func() {
			acquired, added := mgr.Acquire(ctx1, leaseID1, objectName1, nil)
			if added {
				t.Error("expected lease to already exist")
			}
			if acquired != lease1 {
				t.Error("concurrent Acquire returned a different Lease")
			}
		})
	}
	wg.Wait()

	mgr.Delete(leaseID1, objectName1, nil)

	// After a one delete, lease should be gone
	lease := mgr.Get(getSvcID(svc))
	if lease != nil {
		t.Error("expected lease to be removed after all concurrent deletes")
	}
}

// TestGetName_WithoutAnnotation tests with no annotation
func TestGetName_WithoutAnnotation(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", nil)

	namespace, name := ServiceName(svc)
	id := NewID("kubernetes", namespace, name)

	expectedName := "kubevip-my-service"
	expectedID := "my-namespace/kubevip-my-service"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
}

// TestGetName_WithAnnotation tests with a shared lease annotation
func TestGetName_WithAnnotation(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "shared-lease",
	})

	namespace, name := ServiceName(svc)
	id := NewID("kubernetes", namespace, name)

	expectedName := "shared-lease"
	expectedID := "my-namespace/shared-lease"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
}

// TestGetName_WithAnnotation tests with a shared lease annotation
func TestGetName_WithAnnotationAndOverriddenNamespace(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "other-namespace/shared-lease",
	})

	namespace, name := ServiceName(svc)
	id := NewID("kubernetes", namespace, name)

	expectedName := "shared-lease"
	expectedID := "other-namespace/shared-lease"
	expectedNamespace := "other-namespace"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
	if id.Namespace() != expectedNamespace {
		t.Errorf("expected namespace %q, got %q", expectedNamespace, id.Namespace())
	}
}

// TestGetName_WithoutAnnotation_Etcd tests with no annotation
func TestGetName_WithoutAnnotation_Etcd(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", nil)

	namespace, name := ServiceName(svc)
	id := NewID("etcd", namespace, name)

	expectedName := "kubevip-my-service"
	expectedID := "my-namespace-kubevip-my-service"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
}

// TestGetName_WithAnnotation_Etcd tests with a shared lease annotation
func TestGetName_WithAnnotation_Etcd(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "shared-lease",
	})

	namespace, name := ServiceName(svc)
	id := NewID("etcd", namespace, name)

	expectedName := "shared-lease"
	expectedID := "my-namespace-shared-lease"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
}

// TestGetName_WithAnnotation_Etcd tests with a shared lease annotation
func TestGetName_WithAnnotationAndOverriddenNamespace_Etcd(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "other-namespace/shared-lease",
	})

	namespace, name := ServiceName(svc)
	id := NewID("etcd", namespace, name)

	expectedName := "shared-lease"
	expectedID := "other-namespace-shared-lease"
	expectedNamespace := "other-namespace"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
	if id.Namespace() != expectedNamespace {
		t.Errorf("expected namespace %q, got %q", expectedNamespace, id.Namespace())
	}
}

// TestManager_LeaderElectionRestartScenario simulates the bug scenario where
// leadership is lost and the restartable service watcher tries to restart
// the leader election. This test verifies that after deleting the lease,
// a new lease can be created.
func TestManager_LeaderElectionRestartScenario_etcd(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)

	if !isNew1 {
		t.Fatal("expected first acquire to return isNew=true")
	}

	// Simulate leadership acquired.
	electLease(t, lease1)

	// Simulate leadership lost - the leader election function should delete the lease
	// This is the fix: delete the lease when RunOrDie returns
	mgr.Delete(leaseID1, objectName1, nil)

	// Verify lease is removed
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed after delete")
	}

	// Simulate restartable service watcher calling StartServicesLeaderElection again
	ctx2, leaseID2 := getSvcData(svc)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName1, nil)
	if !isNew2 {
		t.Fatal("expected second acquire after delete to return isNew=true")
	}

	// Verify we got a new lease with no elected leader.
	if lease1 == lease2 {
		t.Error("expected new lease to be different from old lease")
	}
	newElection := lease2.JoinElection()
	if newElection.Session.IsLeading() {
		t.Error("expected new lease to have no elected leader")
	}
	newElection.Session.Stopped()
}

// TestManager_CommonLeaseScenario tests the common lease feature where
// multiple services share the same lease.
func TestManager_CommonLeaseScenario(t *testing.T) {
	mgr := NewManager()

	// Two services sharing the same lease via annotation
	sharedLeaseAnnotations := map[string]string{
		serviceLeaseAnnotation: "shared-lease",
	}
	svc1 := createTestService("svc1", "default", sharedLeaseAnnotations)
	svc2 := createTestService("svc2", "default", sharedLeaseAnnotations)

	// First service gets a new lease
	objectName1 := ServiceNamespacedName(svc1)

	ctx1, leaseID1 := getSvcData(svc1)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)
	if !isNew1 {
		t.Error("expected first acquire to return isNew=true")
	}

	// Simulate first service starting leadership.
	participation := lease1.JoinElection()
	if !participation.RunsCampaign() {
		t.Fatal("expected first service to become election candidate")
	}
	election := participation.Session
	if !election.Started() {
		t.Fatal("expected first service to become leader")
	}

	objectName2 := ServiceNamespacedName(svc2)

	ctx2, leaseID2 := getSvcData(svc2)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName2, nil)

	// Second service should get the same lease
	if !isNew2 {
		t.Error("expected second acquire with same lease name to return isNew=true")
	}
	if lease1 != lease2 {
		t.Error("expected same lease for services with same lease annotation")
	}
	observerParticipation := lease2.JoinElection()
	observer := observerParticipation.Session
	if observerParticipation.RunsCampaign() || !observer.WaitForLeader(context.Background()) {
		t.Fatal("shared-lease follower did not observe the elected lease")
	}

	// Delete first service - lease should still exist
	mgr.Delete(leaseID1, objectName1, nil)
	if mgr.Get(getSvcID(svc1)) == nil {
		t.Error("expected lease to still exist after first delete")
	}

	// Delete second service - lease should be removed
	mgr.Delete(leaseID2, objectName2, nil)
	if mgr.Get(getSvcID(svc2)) != nil {
		t.Error("expected lease to be removed after all services deleted")
	}
}

// TestManager_RaceCondition_LeaseExistsBeforeDelete tests the scenario where
// a second goroutine calls Acquire before the first goroutine's defer deletes the lease.
// This simulates the race condition that could cause the gaps in the logs where there is no leader.
func TestManager_RaceCondition_LeaseExistsBeforeDelete(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// Simulate first leader election start
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)
	if !isNew1 {
		t.Fatal("expected first Acquire to return isNew=true")
	}

	// Simulate leadership acquired.
	firstElection := electLease(t, lease1)

	// Simulate a second goroutine calling Acquire BEFORE the first goroutine's defer deletes the lease.
	// This is the race condition scenario
	ctx2, leaseID2 := getSvcData(svc)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName1, nil)

	if isNew2 {
		t.Error("expected second Acquire before delete to return isNew=false")
	}
	if lease1 != lease2 {
		t.Error("expected same lease to be returned")
	}

	if !firstElection.IsLeading() {
		t.Error("expected lease to remain elected")
	}

	// Now the first goroutine's defer deletes the lease
	mgr.Delete(leaseID1, objectName1, nil)

	// Duplicate acquisition did not add a second registration, so one Delete retired the Lease.
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to not exist")
	}

	// Second delete does nothing
	mgr.Delete(leaseID2, objectName1, nil)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to not exist")
	}
}

// TestManager_NonCommonLease_MultipleAcquires tests repeated acquisition of one participant.
func TestManager_NonCommonLease_MultipleAcquires(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil) // No common lease annotation

	// First Acquire.
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)
	if !isNew1 {
		t.Error("expected first Acquire to return isNew=true")
	}

	// Simulate leadership acquired.
	electLease(t, lease1)

	// Second Acquire, simulating another goroutine or restart attempt.
	ctx2, leaseID2 := getSvcData(svc)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName1, nil)
	if isNew2 {
		t.Error("expected second Acquire to return isNew=false")
	}
	if lease1 != lease2 {
		t.Error("expected same lease")
	}

	// Third Acquire.
	ctx3, leaseID3 := getSvcData(svc)
	lease3, isNew3 := mgr.Acquire(ctx3, leaseID3, objectName1, nil)
	if isNew3 {
		t.Error("expected third Acquire to return isNew=false")
	}
	if lease1 != lease3 {
		t.Error("expected same lease")
	}

	// One Delete removes the registration; subsequent Deletes are no-ops.
	mgr.Delete(leaseID1, objectName1, nil)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be deleted")
	}

	mgr.Delete(leaseID2, objectName1, nil)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be deleted")
	}

	mgr.Delete(leaseID3, objectName1, nil)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be deleted")
	}
}

// TestManager_LeaseContextCancelledBeforeStarted tests the scenario where
// the lease context is cancelled before the Started channel is closed.
// This can happen if leadership is never acquired and the context times out.
func TestManager_LeaseContextCancelledBeforeStarted(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// First Acquire.
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)

	if !isNew1 {
		t.Fatal("expected first Acquire to return isNew=true")
	}

	ctx2, leaseID2 := getSvcData(svc)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName1, nil)

	if isNew2 {
		t.Error("expected second Acquire to return isNew=false")
	}

	newElection := lease2.JoinElection()
	if newElection.Session.IsLeading() {
		t.Error("expected lease to have no elected leader")
	}
	newElection.Session.Stopped()

	// Cancel the lease context (simulating timeout or leadership loss before acquiring)
	lease1.Cancel()

	// Verify context is cancelled
	select {
	case <-lease2.Ctx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("expected context to be cancelled")
	}

	// Delete should still work
	mgr.Delete(leaseID1, objectName1, nil)
	mgr.Delete(leaseID2, objectName1, nil)

	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed")
	}
}

// TestManager_RestartAfterLeaseContextCancelled tests that after the lease
// context is cancelled and the lease is deleted, a new lease can be created.
func TestManager_RestartAfterLeaseContextCancelled(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// First Acquire.
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, _ := mgr.Acquire(ctx1, leaseID1, objectName1, nil)

	// Cancel context before Started is closed
	lease1.Cancel()

	// Delete the lease
	mgr.Delete(leaseID1, objectName1, nil)

	// Verify lease is gone
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed after delete")
	}

	// Acquire again - should create a new Lease.
	ctx2, leaseID2 := getSvcData(svc)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName1, nil)

	if !isNew2 {
		t.Error("expected new lease after delete")
	}

	// Verify new lease has a fresh active context and no elected leader.
	select {
	case <-lease2.Ctx.Done():
		t.Error("expected new lease context to be active")
	default:
		// Expected
	}

	newElection := lease2.JoinElection()
	if newElection.Session.IsLeading() {
		t.Error("expected new lease to have no elected leader")
	}
	newElection.Session.Stopped()
}

// TestManager_NonCommonLease_WaitForLeaseContextDone tests the scenario where
// a non-common lease Service calls Acquire while another leader election is running.
// The caller should wait for the lease context to be done before returning.
// This test verifies the fix for the tight spin loop issue.
func TestManager_NonCommonLease_WaitForLeaseContextDone(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("egress-service", "default", nil) // Non-common lease

	// First Acquire simulates the first leader election starting.
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)

	if !isNew1 {
		t.Fatal("expected first Acquire to return isNew=true")
	}

	// Simulate leadership acquired.
	firstElection := electLease(t, lease1)

	// Second Acquire simulates another goroutine trying to start leader election.
	// This should return isNew=false
	ctx2, leaseID2 := getSvcData(svc)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName1, nil)

	if isNew2 {
		t.Error("expected second Acquire to return isNew=false")
	}

	if lease1 != lease2 {
		t.Error("expected same lease to be returned")
	}

	if !firstElection.IsLeading() {
		t.Error("expected lease to remain elected")
	}

	// In the actual code (leader.go), when isNew=false for non-common lease,
	// the code waits on either svcCtx.Ctx.Done() or svcLease.Ctx.Done()
	// Here we verify that the lease context gets cancelled when we delete the lease

	// Start a goroutine that waits for the lease context to be done
	// This simulates what the leader.go code does
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-lease2.Ctx.Done():
			close(waitDone)
		case <-time.After(1 * time.Second):
			// Timeout - test will fail
		}
	}()

	// Verify the goroutine is still waiting (lease context not yet cancelled)
	select {
	case <-waitDone:
		t.Fatal("goroutine should still be waiting")
	case <-time.After(50 * time.Millisecond):
		// Expected - still waiting
	}

	// Now simulate the first leader election ending (defer deletes the lease)
	mgr.Delete(leaseID1, objectName1, nil)

	// The Lease context should now be cancelled because its only registration was removed.
	// Duplicate Acquire does not add another registration; a second delete is a no-op.
	mgr.Delete(leaseID2, objectName1, nil)

	// Now the goroutine should have completed
	select {
	case <-waitDone:
		// Expected - lease context was cancelled
	case <-time.After(200 * time.Millisecond):
		t.Error("expected goroutine to complete after lease context cancelled")
	}

	// Verify lease is removed
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed")
	}
}

// TestManager_NonCommonLease_SpinLoopPrevention tests that the fix prevents
// a tight spin loop when a non-common lease service repeatedly calls Acquire
// while leader election is running. The key behavior is that when isNew=false,
// the lease context should be used to block until the leader election ends.
func TestManager_NonCommonLease_SpinLoopPrevention(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("egress-service", "default", nil) // Non-common lease

	// First Acquire starts leader election.
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, isNew1 := mgr.Acquire(ctx1, leaseID1, objectName1, nil)

	if !isNew1 {
		t.Fatal("expected first Acquire to return isNew=true")
	}

	electLease(t, lease1)

	// Track how many times Acquire is called in a tight loop.
	// In the buggy code, this would spin forever
	// In the fixed code, Acquire returns isNew=false and the caller blocks on lease.Ctx.Done()
	addCount := 0
	done := make(chan struct{})

	go func() {
		for i := 0; i < 100; i++ {
			ctxTmp, leaseIDTmp := getSvcData(svc)
			leaseTmp, isNewTmp := mgr.Acquire(ctxTmp, leaseIDTmp, objectName1, nil)
			addCount++
			if isNewTmp {
				// This shouldn't happen while the first lease exists
				t.Error("unexpected isNew=true")
				break
			}
			// In the fixed code, we would block here on lease.Ctx.Done()
			// For this test, we just verify that isNew=false is returned
			// and the same lease is returned each time
			if leaseTmp != lease1 {
				t.Error("expected same lease")
				break
			}
		}
		close(done)
	}()

	// Wait for the loop to complete
	select {
	case <-done:
		// Expected
	case <-time.After(1 * time.Second):
		t.Fatal("loop timed out")
	}

	// All 100 acquires should have completed, returning isNew=false.
	if addCount != 100 {
		t.Errorf("expected 100 acquires, got %d", addCount)
	}

	mgr.Delete(leaseID1, objectName1, nil)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed after first delete")
	}
}

// TestManager_NonCommonLease_ServiceContextCancellation tests that when
// a service is deleted (svcCtx.Ctx cancelled), the waiting goroutine
// should also unblock. This is the other exit path from the wait.
func TestManager_NonCommonLease_ServiceContextCancellation(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("egress-service", "default", nil)

	// First Acquire starts leader election.
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1, _ := mgr.Acquire(ctx1, leaseID1, objectName1, nil)
	electLease(t, lease1)

	// Second Acquire returns isNew=false.
	ctx2, leaseID2 := getSvcData(svc)
	lease2, isNew2 := mgr.Acquire(ctx2, leaseID2, objectName1, nil)
	if isNew2 {
		t.Error("expected isNew=false")
	}

	// Create a simulated service context
	svcCtx, svcCancel := context.WithCancel(context.Background())

	// Start a goroutine that waits on either svcCtx or lease context
	// This simulates the behavior in leader.go
	waitDone := make(chan string)
	go func() {
		select {
		case <-svcCtx.Done():
			waitDone <- "svcCtx"
		case <-lease2.Ctx.Done():
			waitDone <- "leaseCtx"
		case <-time.After(1 * time.Second):
			waitDone <- "timeout"
		}
	}()

	// Cancel the service context (simulates service deletion)
	svcCancel()

	// The goroutine should unblock via svcCtx.Done()
	select {
	case result := <-waitDone:
		if result != "svcCtx" {
			t.Errorf("expected to unblock via svcCtx, got %s", result)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("goroutine should have unblocked")
	}
}

// TestManager_Delete_DoesNotCancelRecreatedLease reproduces the stale-cleanup bug.
//
// Every service that starts leader election also starts a goroutine that calls
// Manager.Delete once the service context is cancelled. When a service is torn
// down and immediately rebuilt, for instance because its externalTrafficPolicy
// changed, that goroutine runs after the replacement lease was already created.
// Deleting by name alone then cancels the live replacement, and the service is
// never handled again.
func TestManager_Delete_DoesNotCancelRecreatedLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)
	ctx, id := getSvcData(svc)
	objectName := ServiceNamespacedName(svc)

	// The service is set up, and its lease is registered.
	old, _ := mgr.Acquire(ctx, id, objectName, nil)

	// The service is torn down and rebuilt straight away, so a fresh lease for the
	// same name exists before the old cleanup goroutine gets to run.
	mgr.Delete(id, objectName, old)
	fresh, _ := mgr.Acquire(ctx, id, objectName, nil)

	if old == fresh {
		t.Fatal("expected a new lease instance after delete")
	}

	// Now the cleanup for the *old* lease finally runs. It has to be a no-op.
	mgr.Delete(id, objectName, old)

	if fresh.Ctx.Err() != nil {
		t.Error("cleanup for the torn down lease cancelled the recreated lease")
	}
	if got := mgr.Get(id); got == nil {
		t.Error("cleanup for the torn down lease removed the recreated lease")
	}
}

func TestManagerDeleteDoesNotCancelReplacementAfterDirectLeaseCancellation(t *testing.T) {
	manager := NewManager()
	service := createTestService("service", "default", nil)
	id := getSvcID(service)
	objectName := ServiceNamespacedName(service)

	old, isNew := manager.Acquire(context.Background(), id, objectName, nil)
	if !isNew {
		t.Fatal("initial acquire did not register the service")
	}
	old.Cancel()

	fresh, isNew := manager.Acquire(context.Background(), id, objectName, nil)
	if !isNew {
		t.Fatal("replacement acquire did not register the service")
	}
	if fresh == old {
		t.Fatal("acquire reused a directly cancelled lease")
	}

	manager.Delete(id, objectName, old)
	if fresh.Ctx.Err() != nil {
		t.Fatal("late cleanup for a directly cancelled lease cancelled its replacement")
	}
	if manager.Get(id) != fresh {
		t.Fatal("late cleanup for a directly cancelled lease removed its replacement")
	}

	manager.Delete(id, objectName, fresh)
}

// TestManager_Acquire_AfterRetirementReturnsFreshLease reproduces the
// second half of the service rebuild race.
//
// Retiring the Lease synchronously during teardown ensures that a subsequent
// Acquire returns a genuinely fresh instance before deferred cleanup runs.
func TestManager_Acquire_AfterRetirementReturnsFreshLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)
	ctx, id := getSvcData(svc)
	objectName := ServiceNamespacedName(svc)

	old, _ := mgr.Acquire(ctx, id, objectName, nil)

	// Teardown drops the service from its lease synchronously, so the rebuild that
	// follows cannot be parented to it even though the deferred cleanup has not run.
	mgr.Delete(id, objectName, old)

	fresh, _ := mgr.Acquire(ctx, id, objectName, nil)

	if fresh == old {
		t.Fatal("replacement service context would be parented to the doomed lease")
	}

	// The deferred cleanup for the old lease now runs and must be a no-op.
	mgr.Delete(id, objectName, old)

	if fresh.Ctx.Err() != nil {
		t.Error("late cleanup cancelled the replacement lease")
	}
}

// TestManager_LeaseLifetimeInvariant pins the lifetime rule for the whole
// Acquire/Delete surface rather than one scenario: a lease stays usable for exactly as
// long as at least one object still holds it, and is replaced afterwards.
//
// That is the property the common lease depends on, and the one a per-lease
// teardown breaks: dropping one service must not cancel a lease its siblings are
// still using. Raised by Patryk in review of #1669.
func TestManager_LeaseLifetimeInvariant(t *testing.T) {
	shared := map[string]string{serviceLeaseAnnotation: "shared-lease"}

	for _, tc := range []struct {
		name    string
		objects int
	}{
		{"single object", 1},
		{"two objects sharing a lease", 2},
		{"several objects sharing a lease", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager()
			ctx, id := getSvcData(createTestService("svc0", "default", shared))

			objects := make([]string, tc.objects)
			for i := range objects {
				objects[i] = ServiceNamespacedName(createTestService(fmt.Sprintf("svc%d", i), "default", shared))
			}

			var serviceLease *Lease
			for _, o := range objects {
				acquired, added := mgr.Acquire(ctx, id, o, nil)
				if !added {
					t.Fatalf("object %q was not added", o)
				}
				if serviceLease == nil {
					serviceLease = acquired
				} else if acquired != serviceLease {
					t.Fatalf("object %q received a different shared Lease", o)
				}
			}

			// Drop the objects one at a time. Every drop but the last has to leave
			// the lease usable, because the rest still depend on it.
			for i, o := range objects {
				mgr.Delete(id, o, serviceLease)

				if remaining := len(objects) - i - 1; remaining > 0 {
					if serviceLease.Ctx.Err() != nil {
						t.Fatalf("lease was cancelled with %d object(s) still holding it", remaining)
					}
					if mgr.Get(id) != serviceLease {
						t.Fatalf("lease was dropped with %d object(s) still holding it", remaining)
					}
					continue
				}

				if serviceLease.Ctx.Err() == nil {
					t.Error("lease was not cancelled after its last object went away")
				}
				if mgr.Get(id) != nil {
					t.Error("lease was not removed after its last object went away")
				}
			}

			// A rebuild has to get a genuinely fresh lease, so nothing derived from it
			// is cancelled by the teardown that just happened.
			fresh, added := mgr.Acquire(ctx, id, objects[0], nil)
			if !added || fresh == serviceLease || fresh.Ctx.Err() != nil {
				t.Error("rebuild reused the retired lease")
			}
			mgr.Delete(id, objects[0], fresh)
		})
	}
}
