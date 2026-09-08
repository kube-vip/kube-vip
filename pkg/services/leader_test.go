package services

import (
	"context"
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
	for {
		if svcLease.Has(lease.ServiceNamespacedName(follower)) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("follower did not join the shared lease")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	joined := make(chan struct{})
	go func() {
		svcLease.Lock()
		svcLease.Unlock()
		close(joined)
	}()
	select {
	case <-joined:
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
