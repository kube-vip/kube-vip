package worker

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	api "github.com/osrg/gobgp/v3/api"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
)

type BGP struct {
	Common
	bgpServer           *bgp.Server
	bgpSessionInfoGauge *prometheus.GaugeVec
}

func NewBGP(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, signalChan chan os.Signal,
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	bgpServer *bgp.Server, bgpSessionInfoGauge *prometheus.GaugeVec,
	electionMgr *election.Manager, leaseMgr *lease.Manager) *BGP {
	return &BGP{
		Common: *newCommon(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet, electionMgr, leaseMgr),
		bgpServer:           bgpServer,
		bgpSessionInfoGauge: bgpSessionInfoGauge,
	}
}

func (b *BGP) Configure(ctx context.Context, _ *sync.WaitGroup) error {
	var err error
	if b.bgpServer == nil {
		b.bgpServer, err = bgp.NewBGPServer(b.config.BGPConfig)
		if err != nil {
			return fmt.Errorf("creating BGP server: %w", err)
		}
	}

	log.Info("Starting the BGP server to advertise VIP routes to BGP peers")
	if err := b.bgpServer.Start(ctx, func(p *api.WatchEventResponse_PeerEvent) {
		ipaddr := p.GetPeer().GetState().GetNeighborAddress()
		port := uint64(179)
		peerDescription := fmt.Sprintf("%s:%d", ipaddr, port)

		for stateName, stateValue := range api.PeerState_SessionState_value {
			metricValue := 0.0
			if stateValue == int32(p.GetPeer().GetState().GetSessionState().Number()) {
				metricValue = 1
			}

			b.bgpSessionInfoGauge.With(prometheus.Labels{
				"state": stateName,
				"peer":  peerDescription,
			}).Set(metricValue)
		}
	}); err != nil {
		return fmt.Errorf("starting BGP server: %w", err)
	}

	return nil
}

func (b *BGP) Cleanup() {
	// Defer a function to check if the bgpServer has been created and if so attempt to close it
	if b.bgpServer != nil {
		b.bgpServer.Close()
	}
}

func (b *BGP) StartControlPlane(ctx context.Context, electionManager *election.Manager) {
	var err error
	if b.config.EnableLeaderElection {
		err = b.cpCluster.StartCluster(ctx, b.config, electionManager, b.bgpServer, b.leaseMgr)
	} else {
		err = b.cpCluster.StartVipService(ctx, b.config, electionManager, b.bgpServer)
	}
	if err != nil {
		log.Error("Control Plane", "err", err)
		// Trigger the shutdown of this manager instance
		if !b.closing.Load() {
			b.signalChan <- syscall.SIGINT
		}
	}
}

func (b *BGP) ConfigureServices() {
	// No configuration required
}

func (b *BGP) StartServices(ctx context.Context) error {
	if b.config.EnableServicesElection {
		if err := b.PerServiceLeader(ctx); err != nil {
			return err
		}
	} else {
		if err := b.ServicesNoLeader(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (b *BGP) ServicesGlobalLeader(ctx context.Context, id string) {
	// NOT IMPLEMENTED
}

func (b *BGP) ServicesNoLeader(ctx context.Context) error {
	log.Info("beginning watching services without leader election")
	err := b.svcProcessor.ServicesWatcher(ctx, b.svcProcessor.SyncServices)
	if err != nil {
		return err
	}
	return nil
}

func (b *BGP) Name() string {
	return "ARP"
}
