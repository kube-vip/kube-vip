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
	if !hasElectionLoopSeries(t, name) {
		t.Fatal("first exit removed the overlapping loop's series")
	}

	secondDone()
	if hasElectionLoopSeries(t, name) {
		t.Fatal("final exit did not remove the loop series")
	}
}

func TestTrackServiceElectionLoopConcurrentCleanup(t *testing.T) {
	const namespace, name = "metrics-test", "concurrent"
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

	if hasElectionLoopSeries(t, name) {
		t.Fatal("concurrent cleanup left a loop series")
	}
}

func TestTrackServiceElectionLoopSameNameReplacementDuringCleanup(t *testing.T) {
	const namespace, name = "metrics-test", "replacement"

	for range 100 {
		oldDone := TrackServiceElectionLoop(namespace, name)
		start := make(chan struct{})
		cleaned := make(chan struct{})
		go func() {
			<-start
			oldDone()
			close(cleaned)
		}()

		close(start)
		newDone := TrackServiceElectionLoop(namespace, name)
		<-cleaned
		if got := testutil.ToFloat64(ServiceElectionLoops.WithLabelValues(namespace, name)); got != 1 {
			t.Fatalf("replacement loop count = %v, want 1", got)
		}
		if !hasElectionLoopSeries(t, name) {
			t.Fatal("old cleanup removed replacement loop series")
		}
		newDone()
	}

	if hasElectionLoopSeries(t, name) {
		t.Fatal("replacement cleanup left a loop series")
	}
}

func hasElectionLoopSeries(t *testing.T, name string) bool {
	t.Helper()
	const namespace = "metrics-test"
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
