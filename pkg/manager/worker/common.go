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
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	"k8s.io/client-go/kubernetes"
)

type Common struct {
	arpMgr       *arp.Manager
	cpCluster    *cluster.Cluster
	intfMgr      *networkinterface.Manager
	config       *kubevip.Config
	closing      *atomic.Bool
	signalChan   chan os.Signal
	svcProcessor *services.Processor
	mutex        *sync.Mutex
	clientSet    *kubernetes.Clientset
	electionMgr  *election.Manager
	leaseMgr     *lease.Manager
}

func newCommon(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, signalChan chan os.Signal,
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	electionMgr *election.Manager, leaseMgr *lease.Manager) *Common {
	return &Common{
		arpMgr:       arpMgr,
		intfMgr:      intfMgr,
		config:       config,
		closing:      closing,
		signalChan:   signalChan,
		svcProcessor: svcProcessor,
		mutex:        mutex,
		clientSet:    clientSet,
		electionMgr:  electionMgr,
		leaseMgr:     leaseMgr,
	}
}

func (c *Common) InitControlPlane() error {
	var err error
	c.cpCluster, err = cluster.InitCluster(c.config, false, c.intfMgr, c.arpMgr)
	if err != nil {
		return fmt.Errorf("cluster initialization error: %w", err)
	}
	return nil
}

func (c *Common) PerServiceLeader(ctx context.Context) error {
	log.Info("beginning watching services, leaderelection will happen for every service")
	err := c.svcProcessor.StartServicesWatchForLeaderElection(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (c *Common) GlobalLeader(ctx context.Context, leaseName string) {
	c.runGlobalElection(ctx, c, leaseName, c.config, c.electionMgr)
}

func (c *Common) ServicesNoLeader(ctx context.Context) error {
	log.Info("beginning watching services without leader election")
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
	if err != nil {
		return fmt.Errorf("error while watching services: %w", err)
	}
	return nil
}

func (c *Common) OnStartedLeading(ctx context.Context) {
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
	if err != nil {
		log.Error("service watcher", "err", err)
		if !c.closing.Load() {
			c.signalChan <- syscall.SIGINT
		}
	}
}

func (c *Common) OnStoppedLeading() {
	// we can do cleanup here
	c.mutex.Lock()
	defer c.mutex.Unlock()
	log.Info("leader lost", "former leader", c.config.NodeName)
	c.svcProcessor.Stop()

	log.Error("lost services leadership, restarting kube-vip")
	if !c.closing.Load() {
		c.signalChan <- syscall.SIGINT
	}
}

func (c *Common) OnNewLeader(identity string) {
	// we're notified when new leader elected
	if c.config.EnableNodeLabeling {
		labelCtx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		applyNodeLabel(labelCtx, c.clientSet, c.config.Address, c.config.NodeName, identity)
	}
	if identity == c.config.NodeName {
		// I just got the lock
		return
	}
	log.Info("new leader elected", "new leader", identity)
}

func (c *Common) runGlobalElection(ctx context.Context, a election.Actions, leaseName string,
	config *kubevip.Config, electionManager *election.Manager) {

	ns, leaseName := lease.NamespaceName(leaseName, config)

	leaseID := lease.NewID(config.LeaderElectionType, ns, leaseName)
	objectName := lease.ObjectName(leaseID, "svcs0")

	// objLease, isNew, isSharedLease := c.leaseMgr.Add(leaseID, objectName)

	objLease := c.leaseMgr.Add(ctx, leaseID)
	isNew := objLease.Add(objectName)

	// this service was already processed so we do not need to do anything
	if !isNew {
		log.Debug("this election was already done, waiting for it to finish", "lease", c.config.ServicesLeaseName)
		// Wait for either the service context or lease context to be done
		select {
		case <-ctx.Done():
			// Service was deleted
			c.leaseMgr.Delete(leaseID, objectName)
		case <-objLease.Ctx.Done():
			// Leader election ended (leadership lost or context cancelled)
		}
		return
	}

	objLease.Lock()

	defer func() {
		objLease.Unlock()
	}()

	if objLease.Elected.Load() {
		objLease.Unlock()
		log.Debug("this election was already done, shared lease", "lease", leaseID.Name())

		// wait for leader election to start or context to be done
		select {
		case <-objLease.Started:
		case <-objLease.Ctx.Done():
			// Lease was cancelled (e.g., leader election ended), return immediately
			// This allows the restart loop to create a fresh lease
			log.Debug("lease context cancelled before leader election started", "lease", leaseID.Name())
			return
		}

		a.OnStartedLeading(objLease.Ctx)

		log.Debug("waiting for lease to finish", "lease", leaseID.Name())
		// wait for leaderelection to be finished
		<-objLease.Ctx.Done()

		// we can do cleanup here
		a.OnStoppedLeading()

		log.Error("lost leadership, restarting kube-vip", "lease", leaseID.Name())
		if !c.closing.Load() {
			c.signalChan <- syscall.SIGINT
		}

		return
	}

	// For new leases (not shared), ensure cleanup when the leader election ends
	// This is critical for the restartable service watcher to be able to restart
	// the leader election after leadership loss
	defer func() {
		// Delete the lease from the manager so subsequent calls can create a fresh lease
		// This handles the case where leader election ends due to:
		// 1. Leadership loss (e.g., network timeout)
		// 2. Context cancellation
		// 3. Any other reason RunOrDie returns
		c.leaseMgr.Delete(leaseID, objectName)
	}()

	run := &election.RunConfig{
		Config:           config,
		LeaseID:          leaseID,
		LeaseAnnotations: map[string]string{},
		Mgr:              electionManager,
		OnStartedLeading: func(ctx context.Context) {
			objLease.Elected.Store(true)
			objLease.Unlock()
			close(objLease.Started)
			a.OnStartedLeading(ctx)
		},
		OnStoppedLeading: func() {
			objLease.Elected.Store(false)
			a.OnStoppedLeading()
		},
		OnNewLeader: a.OnNewLeader,
	}

	if err := election.RunOrDie(ctx, run, config); err != nil {
		log.Error("leaderelection failed", "err", err, "id", config.NodeName, "name", leaseID.Name())
	}
}
