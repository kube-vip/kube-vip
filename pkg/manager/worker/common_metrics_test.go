package worker

import (
	"context"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type blockingElectionActions struct {
	started chan struct{}
}

func (a *blockingElectionActions) OnStartedLeading(ctx context.Context) {
	close(a.started)
	<-ctx.Done()
}

func (*blockingElectionActions) OnStoppedLeading() {}

func (*blockingElectionActions) OnNewLeader(string) {}

func TestLeaderMetricIsPublishedBeforeActionsRun(t *testing.T) {
	nodeName := t.Name()
	leaseName := "test-lease"
	actions := &blockingElectionActions{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		onStartedLeading(ctx, actions, nodeName, leaseName)
		close(done)
	}()

	select {
	case <-actions.started:
	case <-time.After(time.Second):
		t.Fatal("leadership actions did not start")
	}
	if value := testutil.ToFloat64(metrics.IsLeader.WithLabelValues(nodeName, leaseName)); value != 1 {
		t.Fatalf("leader metric = %v, want 1 while leadership actions are running", value)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("leadership actions did not stop")
	}
}
