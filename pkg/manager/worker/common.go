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
	"github.com/kube-vip/kube-vip/pkg/utils"
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
	id           string
	electionMgr  *election.Manager
	leaseMgr     *lease.Manager
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

func (c *Common) GlobalLeader(ctx context.Context, id string, leaseName string) {
	c.runGlobalElection(ctx, c, id, leaseName)
}

func (c *Common) ServicesNoLeader(ctx context.Context) error {
	log.Info("beginning watching services without leader election")
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
	if err != nil {
		return fmt.Errorf("error while watching services: %w", err)
	}
	return nil
}

func (c *Common) OnStartedLeading(ctx context.Context) error {
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
	if err != nil {
		log.Error("service watcher", "err", err)
		if !c.closing.Load() {
			c.signalChan <- syscall.SIGINT
		}

		return err
	}
	return nil
}

func (c *Common) OnStoppedLeading() {
	// we can do cleanup here
	c.mutex.Lock()
	defer c.mutex.Unlock()
	log.Info("leader lost", "new leader", c.id)
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
		applyNodeLabel(labelCtx, c.clientSet, c.config.Address, c.id, identity)
	}
	if identity == c.id {
		// I just got the lock
		return
	}
	log.Info("new leader elected", "new leader", identity)
}

func (c *Common) runGlobalElection(ctx context.Context, a election.Actions, id, leaseName string) {
	ns, err := utils.ReturnNameSpace()
	if err != nil {
		log.Warn("unable to auto-detect namespace, dropping to config", "namespace", c.config.Namespace)
		ns = c.config.Namespace
	}

	leaseID := fmt.Sprintf("%s/%s", ns, leaseName)
	objectName := fmt.Sprintf("%s-svcs", leaseID)

	objLease, newLease, sharedLease := c.leaseMgr.Add(leaseID, objectName)
	// this service was already processed so we do not need to do anything
	if !newLease {
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

	// Start a goroutine that will delete the lease when the service context is cancelled.
	// This is important for proper cleanup when a service is deleted - it ensures that
	// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
	// Without this, RunOrDie would continue running until leadership is naturally lost.
	go func() {
		<-ctx.Done()
		c.leaseMgr.Delete(leaseID, objectName)
	}()

	// this object is sharing lease with another object
	if sharedLease {
		log.Debug("this election was already done, shared lease", "lease", c.config.ServicesLeaseName)
		// wait for leader election to start or context to be done
		select {
		case <-objLease.Started:
		case <-objLease.Ctx.Done():
			// Lease was cancelled (e.g., leader election ended), return immediately
			// This allows the restart loop to create a fresh lease
			log.Debug("lease context cancelled before leader election started", "lease", c.config.ServicesLeaseName)
			return
		}

		err = c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
		if err != nil {
			log.Error("service watcher", "err", err)
			if !c.closing.Load() {
				c.signalChan <- syscall.SIGINT
			}
			objLease.Cancel()
		}

		log.Debug("waiting for context to finish", "lease", c.config.ServicesLeaseName)
		// Block until context is cancelled
		<-ctx.Done()

		log.Debug("waiting for lease to finish", "lease", c.config.ServicesLeaseName)
		// wait for leaderelection to be finished
		<-objLease.Ctx.Done()

		// we can do cleanup here
		c.mutex.Lock()
		defer c.mutex.Unlock()
		log.Info("leader lost", "lease", c.config.ServicesLeaseName)
		c.svcProcessor.Stop()

		log.Error("lost services leadership, restarting kube-vip")
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
		Config:           c.config,
		LeaseID:          id,
		Namespace:        ns,
		LeaseName:        leaseName,
		LeaseAnnotations: map[string]string{},
		Mgr:              c.electionMgr,
		OnStartedLeading: func(ctx context.Context) {
			close(objLease.Started)
			if err := a.OnStartedLeading(ctx); err != nil {
				objLease.Cancel()
			}
		},
		OnStoppedLeading: a.OnStoppedLeading,
		OnNewLeader:      a.OnNewLeader,
	}

	if err := election.RunOrDie(objLease.Ctx, run, c.config); err != nil {
		log.Error("leaderelection failed", "err", err, "id", id, "name", leaseName)
	}
}

func (c *Common) GetCPCluster() *cluster.Cluster {
	return c.cpCluster
}
