package cluster_test

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestStartVipService_WithdrawsStaticRouteOnContextCancel(t *testing.T) {
	t.Parallel()

	bgpManager := newMockBGPRouteManager()
	cfg := &kubevip.Config{
		EnableBGP: true,
		NodeName:  "cp-node-1",
	}
	cancelContext, _ := startVipService(t, cfg, bgpManager)

	expectEventually(t, bgpManager.isAnnounced, "route should be announced")
	cancelContext()

	withdrawal := bgpManager.waitForWithdrawal(t)
	assertCleanupContext(t, withdrawal)
	if bgpManager.isAnnounced() {
		t.Fatal("static BGP route remained announced after service context cancellation")
	}
}
