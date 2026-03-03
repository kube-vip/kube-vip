package worker

import (
	"context"
	"os"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
)

type Worker interface {
	Configure(context.Context, *sync.WaitGroup) error
	InitControlPlane() error
	StartControlPlane(context.Context, *election.Manager)
	ConfigureServices()
	StartServices(ctx context.Context) error
	Name() string
	Cleanup()
}

func New(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, signalChan chan os.Signal,
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	bgpServer *bgp.Server, bgpSessionInfoGauge *prometheus.GaugeVec,
	electionMgr *election.Manager, leaseMgr *lease.Manager) Worker {
	if config.EnableARP {
		return NewARP(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet, electionMgr, leaseMgr)
	}

	if config.EnableBGP {
		return NewBGP(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet, bgpServer, bgpSessionInfoGauge,
			electionMgr, leaseMgr)
	}

	if config.EnableRoutingTable {
		return NewTable(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet, electionMgr, leaseMgr)
	}

	if config.EnableWireguard {
		return NewWireGuard(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet, electionMgr, leaseMgr)
	}

	return nil
}
