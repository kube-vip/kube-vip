package services

import (
	"context"
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
