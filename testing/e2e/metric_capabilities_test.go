//go:build e2e

package e2e_test

import (
	"context"
	"time"

	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/testing/e2e"
)

func metricPresent(ctx context.Context, clusterName, node, name string) bool {
	metrics, err := e2e.ScrapeMetrics(ctx, clusterName, node)
	return err == nil && e2e.MetricPresent(metrics, name)
}

func metricPresentWithLabels(ctx context.Context, clusterName, node, name string, labels map[string]string) bool {
	metrics, err := e2e.ScrapeMetrics(ctx, clusterName, node)
	if err != nil {
		return false
	}
	_, matches := e2e.MetricValue(metrics, name, labels)
	return matches > 0
}

func hasMetricCapability(ctx context.Context, clusterName string, nodes []string, name string) bool {
	for _, node := range nodes {
		if !metricPresent(ctx, clusterName, node, name) {
			return false
		}
	}
	return true
}

func hasMetricCapabilityWithLabels(ctx context.Context, clusterName string, nodes []string, name string, labels map[string]string) bool {
	for _, node := range nodes {
		if !metricPresentWithLabels(ctx, clusterName, node, name, labels) {
			return false
		}
	}
	return true
}

func hasMetricCapabilityOnAnyNode(ctx context.Context, clusterName string, nodes []string, name string) bool {
	for _, node := range nodes {
		if metricPresent(ctx, clusterName, node, name) {
			return true
		}
	}
	return false
}

func assertEventuallyStableMetric(ctx context.Context, clusterName, node, name string, labels map[string]string,
	expected float64, timeout, interval, gap time.Duration,
) {
	e2e.EventuallyMetric(ctx, clusterName, node, name, labels, Equal(expected), timeout, interval)
	e2e.EventuallyMetricStable(ctx, clusterName, node, name, labels, Equal(expected), timeout, interval, gap)
}
