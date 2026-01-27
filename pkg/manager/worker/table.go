package worker

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	"k8s.io/client-go/kubernetes"
)

type Table struct {
	Common
}

func NewTable(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, signalChan chan os.Signal,
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	electionMgr *election.Manager) *Table {
	return &Table{
		Common: Common{
			arpMgr:       arpMgr,
			intfMgr:      intfMgr,
			config:       config,
			closing:      closing,
			signalChan:   signalChan,
			svcProcessor: svcProcessor,
			mutex:        mutex,
			clientSet:    clientSet,
			electionMgr:  electionMgr,
		},
	}
}

func (t *Table) Configure(ctx context.Context) error {
	log.Info("destination for routes", "table", t.config.RoutingTableID, "protocol", t.config.RoutingProtocol)

	if t.config.CleanRoutingTable {
		go func() {
			// we assume that after 10s all services should be configured so we can delete redundant routes
			time.Sleep(time.Second * 10)
			if err := t.cleanRoutes(); err != nil {
				log.Error("error checking for old routes", "err", err)
			}
		}()
	}

	if t.config.EgressClean {
		vip.ClearIPTables(t.config.EgressWithNftables, t.config.ServiceNamespace, iptables.ProtocolIPv4)
		vip.ClearIPTables(t.config.EgressWithNftables, t.config.ServiceNamespace, iptables.ProtocolIPv6)
		log.Debug("IPtables rules cleaned on startup")
	}

	return nil
}

func (t *Table) StartControlPlane(ctx context.Context, electionManager *election.Manager, _, _ string) {
	if err := t.cpCluster.StartVipService(ctx, t.config, electionManager, nil); err != nil {
		log.Error("Control Plane", "err", err)
		// Trigger the shutdown of this manager instance
		if !t.closing.Load() {
			t.signalChan <- syscall.SIGINT
		}
	} else {
		log.Debug("start VipServer for cluster manager successful")
	}
}

func (t *Table) ConfigureServices() {
	// No configuration required
}

func (t *Table) StartServices(ctx context.Context, id string) error {
	log.Debug("starting Services")

	if t.config.EnableServicesElection {
		if err := t.PerServiceLeader(ctx); err != nil {
			return err
		}
	} else if t.config.EnableLeaderElection {
		t.GlobalLeader(ctx, id, t.config.ServicesLeaseName)
	} else {
		if err := t.ServicesNoLeader(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (t *Table) Name() string {
	return "Routing Table"
}

func (t *Table) cleanRoutes() error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	routes, err := vip.ListRoutes(t.config.RoutingTableID, t.config.RoutingProtocol)
	if err != nil {
		return fmt.Errorf("error getting routes: %w", err)
	}

	for i := range routes {
		found := false
		if t.config.EnableControlPlane {
			found = (routes[i].Dst.IP.String() == t.config.Address)
		} else {
			found = endpoints.CountRouteReferences(&routes[i], &t.svcProcessor.ServiceInstances) > 0
		}

		if !found {
			err = netlink.RouteDel(&(routes[i]))
			if err != nil {
				log.Error("[route] deletion", "route", routes[i], "err", err)
			}
			log.Debug("[route] deletion", "route", routes[i])
		}

	}
	return nil
}
