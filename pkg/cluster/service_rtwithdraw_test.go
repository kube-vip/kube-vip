package cluster_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/node"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

func TestRoutingTableHealthCheck_WithdrawsRouteWhenServiceStops(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprintf("preserve=%t", preserve), func(t *testing.T) {
			t.Parallel()
			healthcheck := newTestHealthServer(t, http.StatusOK)
			t.Cleanup(healthcheck.server.Close)

			network := &mockNetwork{ip: "10.0.0.1", cidr: testCIDR}
			cfg := newRoutingTableConfig(healthcheck.server.URL, healthcheck.caPath)
			cfg.PreserveVIPOnLeadershipLoss = preserve
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
		})
	}
}

func TestRoutingTableHealthCheck_ReturnsRouteDeletionFailure(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	deleteErr := errors.New("route deletion failed")
	network := &mockNetwork{ip: "10.0.0.1", cidr: testCIDR, deleteRouteErr: deleteErr}
	cfg := newRoutingTableConfig(healthcheck.server.URL, healthcheck.caPath)
	c, err := cluster.InitCluster(cfg, true, nil, nil, route.NewManager(), nil)
	if err != nil {
		t.Fatalf("InitCluster: %v", err)
	}
	c.Network = []vip.Network{network}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.StartVipService(ctx, cfg, nil, nil, func() {})
	}()
	expectEventually(t, network.isRoutePresent, "RT route should be installed")
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, deleteErr) {
			t.Fatalf("StartVipService error = %v, want deletion error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("routing-table VIP service did not stop after context cancellation")
	}
}

func TestRoutingTableLeaderContext_WithdrawsRouteOnLeadershipLoss(t *testing.T) {
	t.Parallel()

	cfg := newRoutingTableConfig("", "")
	cfg.PreserveVIPOnLeadershipLoss = true
	network := &mockNetwork{ip: "10.0.0.1", cidr: testCIDR}
	routeMgr := route.NewManager()
	if err := routeMgr.Add(cfg.NodeName, network, true, false); err != nil {
		t.Fatalf("add route: %v", err)
	}
	c, err := cluster.InitCluster(cfg, true, nil, nil, routeMgr, node.NewManager(cfg, nil))
	if err != nil {
		t.Fatalf("InitCluster: %v", err)
	}
	c.Network = []vip.Network{network}

	leaseMgr := lease.NewManager()
	objLease := leaseMgr.Add(context.Background(), lease.NewID("kubernetes", "default", "control-plane"))
	objLease.Lock()
	done := make(chan struct{})
	go func() {
		c.OnStartedLeading(cfg, objLease, &election.Manager{}, nil, func() {}, false)
		close(done)
	}()

	expectEventually(t, func() bool { return objLease.Elected.Load() }, "leader service should start")
	c.OnStoppedLeading(cfg, objLease, nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("leader service did not stop after leadership loss")
	}
	if network.isRoutePresent() {
		t.Fatal("RT route remained after leadership loss")
	}
}
