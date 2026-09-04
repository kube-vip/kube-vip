package cluster_test

import (
	"net/http"
	"testing"
	"time"
)

func TestRoutingTableHealthCheck_WithdrawsRouteWhenServiceStops(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	network := &mockNetwork{ip: "10.0.0.1", cidr: testCIDR}
	cfg := newRoutingTableConfig(healthcheck.server.URL, healthcheck.caPath)
	cfg.PreserveVIPOnLeadershipLoss = true
	cancel, done := startRoutingTableVipService(t, cfg, network)

	expectEventually(t, network.isRoutePresent,
		"RT route should be installed while the control-plane health check is healthy")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("routing-table VIP service did not stop after context cancellation")
	}

	if network.isRoutePresent() || network.routeDeleteCalls() == 0 {
		t.Fatalf("RT route remained after service stop: present=%v deleteCalls=%d",
			network.isRoutePresent(), network.routeDeleteCalls())
	}
}
