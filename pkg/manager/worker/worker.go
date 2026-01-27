package worker

import (
	"context"
	"os"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
)

type Worker interface {
	Configure(context.Context) error
	InitControlPlane() error
	StartControlPlane(context.Context, *cluster.Manager)
	ConfigureServices()
	StartServices(ctx context.Context, id string) error
	Name() string
}

func New(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, signalChan chan os.Signal,
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	bgpServer *bgp.Server, bgpSessionInfoGauge *prometheus.GaugeVec) Worker {
	if config.EnableARP {
		return NewARP(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet)
	}

	if config.EnableBGP {
		return NewBGP(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet, bgpServer, bgpSessionInfoGauge)
	}

	if config.EnableRoutingTable {
		return NewTable(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet)
	}

	if config.EnableWireguard {
		return NewWireguard(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet)
	}

	return nil
}
