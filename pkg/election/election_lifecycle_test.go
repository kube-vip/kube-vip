package election

import (
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"testing"
)

func TestRunTrackedElectionLifecycleUpdatesGauge(t *testing.T) {
	kind := "lifecycle-test"
	before := testutil.ToFloat64(metrics.ElectionLoops.WithLabelValues(kind))
	invoked := false
	if err := runTrackedElection(kind, func() error {
		invoked = true
		if got := testutil.ToFloat64(metrics.ElectionLoops.WithLabelValues(kind)); got != before+1 {
			t.Fatalf("active election gauge = %v, want %v", got, before+1)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !invoked {
		t.Fatal("runner was not invoked")
	}
	if got := testutil.ToFloat64(metrics.ElectionLoops.WithLabelValues(kind)); got != before {
		t.Fatalf("election gauge after exit = %v, want %v", got, before)
	}
}
