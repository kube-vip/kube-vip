package worker

import (
	"context"
	"fmt"
	log "log/slog"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/services"
	"k8s.io/client-go/kubernetes"
)

type Common struct {
	arpMgr       *arp.Manager
	cpCluster    *cluster.Cluster
	intfMgr      *networkinterface.Manager
	config       *kubevip.Config
	closing      *atomic.Bool
	killFunc     func()
	svcProcessor *services.Processor
	mutex        *sync.Mutex
	clientSet    *kubernetes.Clientset
	electionMgr  *election.Manager
	leaseMgr     *lease.Manager
	routeMgr     *route.Manager
	nodeLabelMgr node.Labeler
}

func newCommon(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, killFunc func(),
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	electionMgr *election.Manager, leaseMgr *lease.Manager, routeMgr *route.Manager,
	nodeLabelMgr node.Labeler) *Common {
	return &Common{
		arpMgr:       arpMgr,
		intfMgr:      intfMgr,
		config:       config,
		closing:      closing,
		killFunc:     killFunc,
		svcProcessor: svcProcessor,
		mutex:        mutex,
		clientSet:    clientSet,
		electionMgr:  electionMgr,
		leaseMgr:     leaseMgr,
		routeMgr:     routeMgr,
		nodeLabelMgr: nodeLabelMgr,
	}
}

func (c *Common) InitControlPlane() error {
	var err error
	c.cpCluster, err = cluster.InitCluster(c.config, false, c.intfMgr, c.arpMgr, c.routeMgr, c.nodeLabelMgr)
	if err != nil {
		return fmt.Errorf("cluster initialization error: %w", err)
	}
	return nil
}

func (c *Common) PerServiceLeader(ctx context.Context, forcedOnly bool) error {
	if forcedOnly {
		log.Info(fmt.Sprintf("beginning watching services, leaderelection will happen for services annotated with '%s = \"true\"'", kubevip.ForcePerServiceElection))
	} else {
		log.Info("beginning watching services, leaderelection will happen for every service")
	}

	err := c.svcProcessor.StartServicesWatchForLeaderElection(ctx, forcedOnly)
	if err != nil {
		return err
	}
	return nil
}

func (c *Common) GlobalLeader(ctx context.Context, leaseName string) {
	wg := sync.WaitGroup{}
	defer wg.Wait()

	servicesCtx, servicesCtxCancel := context.WithCancel(ctx)
	defer servicesCtxCancel()

	if c.config.PerServiceElectionOnDemand {
		wg.Go(func() {
			if err := c.PerServiceLeader(servicesCtx, true); err != nil {
				log.Error("per-service leader election failed with", "error", err)
				servicesCtxCancel()
			}
		})
	}

	var vipProvider lease.VIPProvider
	if c.svcProcessor != nil {
		vipProvider = c.svcProcessor.OwnedServiceVIPs
	}
	c.runGlobalElectionWithVIPProvider(servicesCtx, c, leaseName, c.config, c.electionMgr, vipProvider)
}

func (c *Common) ServicesNoLeader(ctx context.Context) error {
	wg := sync.WaitGroup{}
	defer wg.Wait()

	servicesCtx, servicesCtxCancel := context.WithCancel(ctx)
	defer servicesCtxCancel()

	if c.config.PerServiceElectionOnDemand {
		wg.Go(func() {
			if err := c.PerServiceLeader(servicesCtx, true); err != nil {
				log.Error("per-service leader election failed with", "error", err)
				servicesCtxCancel()
			}
		})
	}

	log.Info("beginning watching services without leader election")
	err := c.svcProcessor.ServicesWatcher(servicesCtx, c.svcProcessor.SyncServices, false)
	if err != nil {
		return fmt.Errorf("error while watching services: %w", err)
	}
	return nil
}

func (c *Common) Cleanup() {
	// NOT IMPLEMENTED
}

func (c *Common) OnStartedLeading(ctx context.Context) {
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices, false)
	if err != nil {
		log.Error("service watcher", "err", err)
		c.killFunc()
	}
}

func (c *Common) OnStoppedLeading() {
	// we can do cleanup here
	c.mutex.Lock()
	defer c.mutex.Unlock()
	log.Info("leader lost", "former leader", c.config.NodeName)
	c.svcProcessor.Stop()

	log.Error("lost services leadership, restarting kube-vip")
	c.killFunc()
}

func (c *Common) OnNewLeader(identity string) {
	if identity == c.config.NodeName {
		// I just got the lock
		return
	}
	log.Info("new leader elected", "new leader", identity)
}

func (c *Common) runGlobalElection(ctx context.Context, a election.Actions, leaseName string,
	config *kubevip.Config, electionManager *election.Manager, vips []string) {
	c.runGlobalElectionWithVIPProvider(ctx, a, leaseName, config, electionManager, lease.StaticVIPProvider(vips))
}

func (c *Common) runGlobalElectionWithVIPProvider(ctx context.Context, a election.Actions, leaseName string,
	config *kubevip.Config, electionManager *election.Manager, vipProvider lease.VIPProvider) {

	log.Debug("starting global election")
	ns, leaseName := lease.NamespaceName(leaseName, config)

	leaseID := lease.NewID(config.LeaderElectionType, ns, leaseName)
	objectName := lease.ObjectName(leaseID, "svcs0")

	objLease, _ := c.leaseMgr.Acquire(context.Background(), leaseID, objectName, vipProvider)
	defer c.leaseMgr.Delete(leaseID, objectName, objLease)
	electionCtx, cancelElection := objLease.NewElectionContext(ctx)
	defer cancelElection()

	var electionSession *lease.ElectionSession
	for {
		participation := objLease.JoinElection()
		if participation.RunsCampaign() {
			electionSession = participation.Session
			break
		}
		log.Debug("this election was already done, shared lease", "lease", leaseID.Name())
		if !participation.Session.WaitForLeader(electionCtx) {
			if electionCtx.Err() != nil {
				return
			}
			continue
		}

		leaderCtx, cancelLeader := context.WithCancel(electionCtx)
		wg := sync.WaitGroup{}
		wg.Go(func() {
			a.OnStartedLeading(leaderCtx)
		})
		participation.Session.WaitForEnd(electionCtx)
		cancelLeader()
		wg.Wait()
		if electionCtx.Err() != nil {
			return
		}
		a.OnStoppedLeading()

		log.Error("lost leadership, restarting kube-vip", "lease", leaseID.Name())
		c.killFunc()
		return
	}
	wg := sync.WaitGroup{}
	defer electionSession.Stopped()
	defer wg.Wait()

	run := &election.RunConfig{
		Config:           config,
		LeaseID:          leaseID,
		LeaseAnnotations: map[string]string{},
		VIPsProvider:     objLease.OwnedVIPs,
		Mgr:              electionManager,
		OnStartedLeading: func(ctx context.Context) {
			if !electionSession.Started() {
				return
			}
			wg.Go(func() {
				a.OnStartedLeading(ctx)
				metrics.LeaderTransitionsTotal.WithLabelValues(leaseID.Name()).Inc()
				metrics.IsLeader.WithLabelValues(config.NodeName, leaseID.Name()).Set(1)
			})
		},
		OnStoppedLeading: func() {
			if !electionSession.IsCurrent() {
				return
			}
			a.OnStoppedLeading()
			metrics.IsLeader.WithLabelValues(config.NodeName, leaseID.Name()).Set(0)
		},
		OnNewLeader: a.OnNewLeader,
	}

	if err := election.RunOrDie(electionCtx, run, config); err != nil {
		log.Error("leaderelection failed", "err", err, "id", config.NodeName, "name", leaseID.Name())
	}
}
