//go:build e2e

package e2e

import (
	"os"
	"testing"
)

func TestParseMetricsFixture(t *testing.T) {
	payload, err := os.ReadFile("testdata/metrics.prom")
	if err != nil {
		t.Fatal(err)
	}

	metrics, err := parseMetrics(string(payload))
	if err != nil {
		t.Fatalf("parseMetrics() error = %v", err)
	}

	if value, matches := MetricValue(metrics, "kube_vip_is_leader", map[string]string{"node": "control-plane"}); value != 1 || matches != 1 {
		t.Fatalf("MetricValue() = (%v, %d), want (1, 1)", value, matches)
	}
	if value := SumMetric(metrics, "kube_vip_is_leader", map[string]string{"lease_name": "plndr-cp-lock"}); value != 1 {
		t.Fatalf("SumMetric() = %v, want 1", value)
	}
	if value, found := MaxMetric(metrics, "kube_vip_election_errors_total", map[string]string{"reason": "api\nserver"}); value != 3 || !found {
		t.Fatalf("MaxMetric() = (%v, %t), want (3, true)", value, found)
	}
	if value, matches := MetricValue(metrics, "kube_vip_reconcile_seconds_count", nil); value != 4 || matches != 1 {
		t.Fatalf("histogram count = (%v, %d), want (4, 1)", value, matches)
	}
}

func TestMetricHelpersReportMissingAndAmbiguousSeries(t *testing.T) {
	metrics := map[string]float64{
		`metric{node="one"}`: 1,
		`metric{node="two"}`: 2,
	}

	if _, matches := MetricValue(metrics, "metric", nil); matches != 2 {
		t.Fatalf("MetricValue() matches = %d, want 2", matches)
	}
	if _, matches := MetricValue(metrics, "missing", nil); matches != 0 {
		t.Fatalf("MetricValue() matches = %d, want 0", matches)
	}
	if _, ok := CounterDelta(metrics, map[string]float64{`metric{node="one"}`: 4}, "metric", nil); ok {
		t.Fatal("CounterDelta() reported an ambiguous selector as available")
	}
	if delta, ok := CounterDelta(metrics, map[string]float64{`metric{node="one"}`: 4}, "metric", map[string]string{"node": "one"}); delta != 3 || !ok {
		t.Fatalf("CounterDelta() = (%v, %t), want (3, true)", delta, ok)
	}
}

func TestParseMetricsRejectsInvalidFixture(t *testing.T) {
	if _, err := parseMetrics("not valid prometheus text"); err == nil {
		t.Fatal("parseMetrics() succeeded for invalid input")
	}
}
