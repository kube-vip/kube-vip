package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestStartServicesLeaderElection_ReturnsOnLeaseLossWithoutServiceDeletion is a regression test for
// the issue: StartServicesLeaderElection deadlocked forever whenever it returned for
// any reason other than the Service itself being deleted.
//
// The function starts a lease-cleanup goroutine that only exits once svcCtx.Ctx is cancelled (Service
// deleted), then defers wg.Wait() on the same WaitGroup that goroutine belonged to. Since the Service
// stays alive across an ordinary lease loss, that goroutine and therefore the deferred wg.Wait()
// never returned, permanently wedging the leader-election restart loop in startLeaderElection for that
// service. The only workaround was to delete and recreate the Service.
func TestStartServicesLeaderElection_ReturnsOnLeaseLossWithoutServiceDeletion(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
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

	// Simulate ordinary leadership/lease loss (e.g. a renewal failure): the lease context ends
	// but the Service itself is untouched, so svcCtx.Ctx must stay alive.
	svcLease.Cancel()

	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(svc.UID, svcCtx)

	done := make(chan error, 1)
	go func() {
		done <- p.StartServicesLeaderElection(svcCtx, svc, nil, true)
	}()

	select {
	case <-done:
		// Expected: the function must return promptly when only the lease - not the service -
		// has gone away, so the restart loop can retry the election.
	case <-time.After(5 * time.Second):
		t.Fatal("StartServicesLeaderElection did not return after the lease context was " +
			"cancelled while the service context remained alive; this reproduces the deadlock " +
			"where leader election could never be retried for a live service")
	}

	if svcCtx.Ctx.Err() != nil {
		t.Fatal("service context should not have been cancelled by an ordinary lease loss")
	}

	// The lease-cleanup goroutine should still be running, waiting for the service to be
	// deleted; confirm it is not left dangling forever by cancelling the service context now.
	svcCtx.Cancel()
}

func TestSharedLeaseFollowerCancellationDoesNotStopLeader(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{EnableServicesElection: true},
		leaseMgr: lease.NewManager(),
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
	sharedLease.Add(lease.ServiceNamespacedName(leaderService))
	if !sharedLease.BeginElection() {
		t.Fatal("leader service did not become candidate")
	}
	sharedLease.ElectionStarted()
	sharedLease.Add(lease.ServiceNamespacedName(followerService))

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

func TestStartServicesLeaderElectionReturnsErrorWithoutLease(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: types.UID("service"),
	}}
	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, svcCtx)

	err := p.StartServicesLeaderElection(svcCtx, service, nil, true)
	if err == nil || !strings.Contains(err.Error(), "no existing lease found") {
		t.Fatalf("StartServicesLeaderElection() error = %v, want missing lease error", err)
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

	claimed, isNew := p.leaseMgr.Claim(id, lease.ServiceNamespacedName(service))
	if claimed == nil || !isNew {
		t.Fatal("cancelled callback claimed the replacement lease membership")
	}
	p.leaseMgr.Delete(id, lease.ServiceNamespacedName(service), claimed)
}

func TestServiceLeaderContextCancelsWhenServiceEndsBeforeSharedLease(t *testing.T) {
	svcCtx := servicecontext.New(context.Background())
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	defer cancelLease()

	leaderCtx, leaderCancel, stopServiceCancel := newServiceLeaderContext(svcCtx, leaseCtx)
	defer leaderCancel()
	defer stopServiceCancel()
	svcCtx.Cancel()

	select {
	case <-leaderCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("leader election context remained active after its Service context ended")
	}
	if leaseCtx.Err() != nil {
		t.Fatal("service cancellation ended the shared lease context")
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
	sharedLease.Add(lease.ServiceNamespacedName(service))
	oldCtx.Cancel()

	// AddOrModify drops the cancelled context's membership before installing its replacement.
	p.leaseMgr.Delete(id, lease.ServiceNamespacedName(service), sharedLease)
	freshCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, freshCtx)
	if _, isNew := p.leaseMgr.Claim(id, lease.ServiceNamespacedName(service)); !isNew {
		t.Fatal("replacement context did not claim shared lease membership")
	}

	p.releaseServiceLease(oldCtx, service, id, lease.ServiceNamespacedName(service), sharedLease)
	p.leaseMgr.Delete(id, "default/sibling", sharedLease)
	if p.leaseMgr.Get(id) != sharedLease {
		t.Fatal("stale cleanup removed the replacement membership from the shared lease")
	}

	p.leaseMgr.Delete(id, lease.ServiceNamespacedName(service), sharedLease)
}
