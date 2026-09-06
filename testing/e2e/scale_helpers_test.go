//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestScaleCounterDelta(t *testing.T) {
	metric := func(value float64) ScaleMetricSnapshot {
		return ScaleMetricSnapshot{"node": {`counter{namespace="test"}`: value}}
	}
	empty := ScaleMetricSnapshot{"node": {}}
	tests := []struct {
		name      string
		before    ScaleMetricSnapshot
		after     ScaleMetricSnapshot
		wantDelta float64
		wantError string
	}{
		{name: "absent before and after", before: empty, after: empty},
		{name: "initialized to zero", before: empty, after: metric(0)},
		{name: "absent before nonzero after", before: empty, after: metric(1), wantError: "presence changed"},
		{name: "absent after", before: metric(1), after: empty, wantError: "presence changed"},
		{name: "absent after zero", before: metric(0), after: empty, wantError: "presence changed"},
		{name: "stable", before: metric(2), after: metric(2)},
		{name: "reset", before: metric(5), after: metric(2), wantError: "reset"},
		{name: "increment", before: metric(2), after: metric(5), wantDelta: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			delta, err := ScaleCounterDelta(test.before, test.after, "counter", map[string]string{"namespace": "test"})
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("ScaleCounterDelta() error = %v", err)
				}
				if delta != test.wantDelta {
					t.Fatalf("ScaleCounterDelta() delta = %v, want %v", delta, test.wantDelta)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("ScaleCounterDelta() error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestScaleTransitionDeltaReportsMetricPresence(t *testing.T) {
	const metric = "kube_vip_leader_election_transitions_total"
	before := ScaleMetricSnapshot{"node": {metric + `{lease_name="lease"}`: 4}}
	after := ScaleMetricSnapshot{"node": {metric + `{lease_name="lease"}`: 6}}

	if delta, found := ScaleTransitionDelta(before, after, []string{"lease"}); delta != 2 || !found {
		t.Fatalf("ScaleTransitionDelta() = (%v, %t), want (2, true)", delta, found)
	}
	if delta, found := ScaleTransitionDelta(before, after, []string{"missing"}); delta != 0 || found {
		t.Fatalf("ScaleTransitionDelta() for missing series = (%v, %t), want (0, false)", delta, found)
	}
}
