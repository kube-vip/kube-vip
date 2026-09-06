//go:build e2e

package e2e

import "testing"

func TestScaleCounterDeltaReportsMetricPresence(t *testing.T) {
	before := ScaleMetricSnapshot{"node": {`counter{namespace="test"}`: 2}}
	after := ScaleMetricSnapshot{"node": {`counter{namespace="test"}`: 5}}

	if delta, found := ScaleCounterDelta(before, after, "counter", map[string]string{"namespace": "test"}); delta != 3 || !found {
		t.Fatalf("ScaleCounterDelta() = (%v, %t), want (3, true)", delta, found)
	}
	if delta, found := ScaleCounterDelta(before, after, "missing", nil); delta != 0 || found {
		t.Fatalf("ScaleCounterDelta() for missing metric = (%v, %t), want (0, false)", delta, found)
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
