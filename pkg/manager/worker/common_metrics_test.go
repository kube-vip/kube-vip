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

type gaugeCheckingStopActions struct {
	blockingElectionActions
	nodeName, leaseName string
	valueDuringStop     float64
}

func (a *gaugeCheckingStopActions) OnStoppedLeading() {
	a.valueDuringStop = testutil.ToFloat64(metrics.IsLeader.WithLabelValues(a.nodeName, a.leaseName))
}

func TestLeaderMetricIsClearedBeforeStopActionsRun(t *testing.T) {
	actions := &gaugeCheckingStopActions{nodeName: t.Name(), leaseName: "test-lease", valueDuringStop: -1}
	metrics.IsLeader.WithLabelValues(actions.nodeName, actions.leaseName).Set(1)

	onStoppedLeading(actions, actions.nodeName, actions.leaseName)

	if actions.valueDuringStop != 0 {
		t.Fatalf("leader metric = %v during stop actions, want 0", actions.valueDuringStop)
	}
}
