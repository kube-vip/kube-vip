package worker

import (
	"context"
	log "log/slog"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"k8s.io/client-go/kubernetes"
)

type ARP struct {
	Common
}

func NewARP(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, killFunc func(),
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	electionMgr *election.Manager, leaseMgr *lease.Manager) *ARP {
	return &ARP{
		Common: *newCommon(arpMgr, intfMgr, config, closing, killFunc,
			svcProcessor, mutex, clientSet, electionMgr, leaseMgr),
	}
}

func (a *ARP) Configure(ctx context.Context, wg *sync.WaitGroup) error {
	log.Info("Start ARP/NDP advertisement Global")
	wg.Go(func() {
		a.arpMgr.StartAdvertisement(ctx)
	})
	return nil
}

func (a *ARP) StartControlPlane(ctx context.Context, electionManager *election.Manager) {
	err := a.cpCluster.StartCluster(ctx, a.config, electionManager, nil, a.leaseMgr, a.killFunc)
	if err != nil {
		log.Error("starting control plane", "err", err)
	}

	// Trigger the shutdown of this manager instance
	a.killFunc()
}

func (a *ARP) ConfigureServices() {
	// This will tidy any dangling kube-vip iptables rules
	if a.config.EgressClean {
		vip.ClearIPTables(a.config.EgressWithNftables, a.config.ServiceNamespace, iptables.ProtocolIPv4)
	}
}

func (a *ARP) StartServices(ctx context.Context) error {
	// Start a services watcher (all kube-vip pods will watch services), upon a new service
	// a lock based upon that service is created that they will all leaderElection on
	if a.config.EnableServicesElection {
		if err := a.PerServiceLeader(ctx); err != nil {
			return err
		}
	} else {
		a.GlobalLeader(ctx, a.config.ServicesLeaseName)
	}
	return nil
}

func (a *ARP) Name() string {
	return "ARP"
}
