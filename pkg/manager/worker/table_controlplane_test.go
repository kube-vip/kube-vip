package worker

import (
	"context"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
)

type controlPlaneClusterSpy struct {
	startClusterCalls    int
	startVipServiceCalls int
}

func (s *controlPlaneClusterSpy) StartCluster(context.Context, *kubevip.Config, *election.Manager, *bgp.Server, *lease.Manager, func()) error {
	s.startClusterCalls++
	return nil
}

func (s *controlPlaneClusterSpy) StartVipService(context.Context, *kubevip.Config, *election.Manager, cluster.BGPRouteManager, func()) error {
	s.startVipServiceCalls++
	return nil
}

func TestTableStartControlPlane(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		enableLeaderElection bool
		wantStartCluster     int
		wantStartVIPService  int
	}{
		{
			name:                 "leader election",
			enableLeaderElection: true,
			wantStartCluster:     1,
		},
		{
			name:                "without leader election",
			wantStartVIPService: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spy := &controlPlaneClusterSpy{}
			table := &Table{Common: Common{
				config: &kubevip.Config{KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
					EnableLeaderElection: tt.enableLeaderElection,
				}},
				cpCluster: spy,
				leaseMgr:  lease.NewManager(),
				killFunc:  func() {},
			}}

			table.StartControlPlane(context.Background(), &election.Manager{})

			if spy.startClusterCalls != tt.wantStartCluster {
				t.Errorf("StartCluster calls = %d, want %d", spy.startClusterCalls, tt.wantStartCluster)
			}
			if spy.startVipServiceCalls != tt.wantStartVIPService {
				t.Errorf("StartVipService calls = %d, want %d", spy.startVipServiceCalls, tt.wantStartVIPService)
			}
		})
	}
}
