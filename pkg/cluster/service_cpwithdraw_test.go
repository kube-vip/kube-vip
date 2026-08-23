package cluster_test

import (
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestStartVipService_WithdrawsStaticRouteOnContextCancel(t *testing.T) {
	t.Parallel()

	bgpManager := newMockBGPRouteManager()
	cfg := &kubevip.Config{
		EnableBGP: true,
		NodeName:  "cp-node-1",
	}
	cancelContext, vipServiceDone := startVipService(t, cfg, bgpManager)

	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced")

	cancelContext()
	select {
	case <-vipServiceDone:
	case <-time.After(5 * time.Second):
		t.Fatal("vipService did not stop after context cancellation")
	}

	if bgpManager.isAnnounced() {
		t.Fatal("static BGP route remained announced after service context cancellation")
	}
}
