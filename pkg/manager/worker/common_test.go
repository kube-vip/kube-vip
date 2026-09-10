package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
)

type sharedElectionActions struct {
	started chan struct{}
	stopped chan struct{}
}

func (a *sharedElectionActions) OnStartedLeading(ctx context.Context) {
	close(a.started)
	<-ctx.Done()
}

func (a *sharedElectionActions) OnStoppedLeading() {
	close(a.stopped)
}

func (a *sharedElectionActions) OnNewLeader(string) {}

func TestGlobalElectionFollowsSharedLeaseLeadership(t *testing.T) {
	config := &kubevip.Config{KubernetesLeaderElection: kubevip.KubernetesLeaderElection{LeaseName: "default/shared"}}
	leaseID := lease.NewID(config.LeaderElectionType, "default", "shared")
	leaseMgr := lease.NewManager()
	sharedLease, _ := leaseMgr.Acquire(context.Background(), leaseID, "service")
	if !sharedLease.BeginElection() {
		t.Fatal("Service election did not start")
	}
	sharedLease.ElectionStarted()

	actions := &sharedElectionActions{started: make(chan struct{}), stopped: make(chan struct{})}
	var killed atomic.Bool
	common := &Common{config: config, leaseMgr: leaseMgr, killFunc: func() { killed.Store(true) }}
	done := make(chan struct{})
	go func() {
		common.runGlobalElection(context.Background(), actions, config.LeaseName, config, nil, nil)
		close(done)
	}()
	select {
	case <-actions.started:
	case <-time.After(time.Second):
		t.Fatal("global election follower did not activate")
	}

	sharedLease.ElectionStopped()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("global election follower did not stop after leadership ended")
	}
	select {
	case <-actions.stopped:
	default:
		t.Fatal("global election follower did not run leadership cleanup")
	}
	if !killed.Load() {
		t.Fatal("global election follower did not request restart after leadership loss")
	}
	if sharedLease.Ctx.Err() != nil || leaseMgr.Get(leaseID) != sharedLease {
		t.Fatal("global election follower cancelled the surviving Service lease")
	}
	leaseMgr.Delete(leaseID, "service", sharedLease)
}

func TestGlobalElectionFollowerShutdownIsNotLeadershipLoss(t *testing.T) {
	config := &kubevip.Config{KubernetesLeaderElection: kubevip.KubernetesLeaderElection{LeaseName: "default/shared"}}
	leaseID := lease.NewID(config.LeaderElectionType, "default", "shared")
	leaseMgr := lease.NewManager()
	sharedLease, _ := leaseMgr.Acquire(context.Background(), leaseID, "service")
	if !sharedLease.BeginElection() {
		t.Fatal("Service election did not start")
	}
	sharedLease.ElectionStarted()

	actions := &sharedElectionActions{started: make(chan struct{}), stopped: make(chan struct{})}
	var killed atomic.Bool
	common := &Common{config: config, leaseMgr: leaseMgr, killFunc: func() { killed.Store(true) }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		common.runGlobalElection(ctx, actions, config.LeaseName, config, nil, nil)
		close(done)
	}()
	select {
	case <-actions.started:
	case <-time.After(time.Second):
		t.Fatal("global election follower did not activate")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("global election follower did not stop with its parent context")
	}
	select {
	case <-actions.stopped:
		t.Fatal("graceful follower shutdown was reported as leadership loss")
	default:
	}
	if killed.Load() {
		t.Fatal("graceful follower shutdown requested a process restart")
	}
	if sharedLease.Ctx.Err() != nil || leaseMgr.Get(leaseID) != sharedLease {
		t.Fatal("global follower shutdown cancelled the surviving Service lease")
	}
	leaseMgr.Delete(leaseID, "service", sharedLease)
}
