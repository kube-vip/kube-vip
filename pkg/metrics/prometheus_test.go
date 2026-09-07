package metrics

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestTrackServiceElectionLoopLifecycle(t *testing.T) {
	const namespace, name = "metrics-test", "service"
	ServiceElectionLoops.DeleteLabelValues(namespace, name)

	firstDone := TrackServiceElectionLoop(namespace, name)
	secondDone := TrackServiceElectionLoop(namespace, name)
	if got := testutil.ToFloat64(ServiceElectionLoops.WithLabelValues(namespace, name)); got != 2 {
		t.Fatalf("active loops = %v, want 2", got)
	}

	firstDone()
	firstDone()
	if got := testutil.ToFloat64(ServiceElectionLoops.WithLabelValues(namespace, name)); got != 1 {
		t.Fatalf("active loops after first exit = %v, want 1", got)
	}
	if !hasElectionLoopSeries(t, namespace, name) {
		t.Fatal("first exit removed the overlapping loop's series")
	}

	secondDone()
	if hasElectionLoopSeries(t, namespace, name) {
		t.Fatal("final exit did not remove the loop series")
	}
}

func TestTrackServiceElectionLoopConcurrentCleanup(t *testing.T) {
	const namespace, name = "metrics-test", "concurrent"
	ServiceElectionLoops.DeleteLabelValues(namespace, name)

	const loops = 64
	done := make([]func(), loops)
	for i := range done {
		done[i] = TrackServiceElectionLoop(namespace, name)
	}
	var wg sync.WaitGroup
	for _, cleanup := range done {
		wg.Go(cleanup)
	}
	wg.Wait()

	if hasElectionLoopSeries(t, namespace, name) {
		t.Fatal("concurrent cleanup left a loop series")
	}
}

func hasElectionLoopSeries(t *testing.T, namespace, name string) bool {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(ServiceElectionLoops)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			if metricHasLabels(metric, namespace, name) {
				return true
			}
		}
	}
	return false
}

func metricHasLabels(metric *dto.Metric, namespace, name string) bool {
	matched := 0
	for _, label := range metric.Label {
		if label.GetName() == "namespace" && label.GetValue() == namespace ||
			label.GetName() == "name" && label.GetValue() == name {
			matched++
		}
	}
	return matched == 2
}
