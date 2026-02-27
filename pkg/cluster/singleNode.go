package cluster

import (
	"context"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func (cluster *Cluster) StartVipService(ctx context.Context, c *kubevip.Config,
	em *election.Manager, bgp *bgp.Server, killFunc func()) error {
	// use a Go context so we can tell the arp loop code when we
	// want to step down
	clusterCtx, clusterCancel := context.WithCancel(ctx)
	defer clusterCancel()

	return cluster.vipService(clusterCtx, c, em, bgp, nil, killFunc)
}
