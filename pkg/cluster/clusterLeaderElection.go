package cluster

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/utils"

	log "log/slog"
)

// StartCluster - Begins a running instance of the Leader Election cluster
func (cluster *Cluster) StartCluster(ctx context.Context, c *kubevip.Config,
	em *election.Manager, bgpServer *bgp.Server, leaseMgr *lease.Manager) error {

	ns, leaseName := lease.NamespaceName(c.LeaseName, c)

	leaseID := lease.NewID(c.LeaderElectionType, ns, leaseName)

	log.Info("cluster membership", "namespace", leaseID.Namespace(), "lock", leaseID.Name(), "id", c.NodeName)

	objectName := lease.ObjectName(leaseID, "cp")
	objLease := leaseMgr.Add(ctx, leaseID)
	isNew := objLease.Add(objectName)

	wg := sync.WaitGroup{}
	defer wg.Wait()

	// Start a goroutine that will delete the lease when the service context is cancelled.
	// This is important for proper cleanup when a service is deleted - it ensures that
	// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
	// Without this, RunOrDie would continue running until leadership is naturally lost.
	wg.Go(func() {
		<-objLease.Ctx.Done()
		leaseMgr.Delete(leaseID, objectName)
	})

	if !isNew {
		log.Debug("this election was already done, waiting for it to finish", "lease", leaseName)
		<-objLease.Ctx.Done()
		return nil
	}

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	wg.Go(func() {
		select {
		case <-signalChan:
		case <-cluster.stop:
		}

		log.Info("Received termination, signaling cluster shutdown")
		// Cancel the leader context, which will in turn cancel the leadership
		objLease.Cancel()
	})

	// (attempt to) Remove the virtual IP, in case it already exists

	for i := range cluster.Network {
		deleted, err := cluster.Network[i].DeleteIP()
		if err != nil {
			log.Error("could not delete virtualIP", "err", err)
		}
		if deleted {
			log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
		}
	}

	objLease.Lock()

	defer func() {
		objLease.Unlock()
	}()

	// this object is sharing lease with another object
	if objLease.Elected.Load() {
		log.Debug("this election was already done, shared lease", "lease", leaseName)
		// wait for leader election to start or context to be done
		select {
		case <-objLease.Started:
		case <-objLease.Ctx.Done():
			// Lease was cancelled (e.g., leader election ended), return immediately
			// This allows the restart loop to create a fresh lease
			log.Debug("lease context cancelled before leader election started", "lease", leaseName)
			return fmt.Errorf("lease %q context cancelled before leader election started", leaseName)
		}

		cluster.OnStartedLeading(c, objLease, em, bgpServer, signalChan, true)

		log.Debug("cluster waiting for leader context done", "lease", leaseName)
		// wait for leaderelection to be finished
		<-objLease.Ctx.Done()

		cluster.OnStoppedLeading(c, objLease, bgpServer, signalChan)

		return nil
	}

	run := &election.RunConfig{
		Config:           c,
		LeaseID:          leaseID,
		LeaseAnnotations: c.LeaseAnnotations,
		Mgr:              em,
		OnStartedLeading: func(context.Context) { //nolint TODO: potential clean code
			cluster.OnStartedLeading(c, objLease, em, bgpServer, signalChan, false)
		},
		OnStoppedLeading: func() {
			objLease.Elected.Store(false)
			cluster.OnStoppedLeading(c, objLease, bgpServer, signalChan)
		},
		OnNewLeader: func(identity string) {
			cluster.OnNewLeader(identity, c)
		},
	}

	if err := election.RunOrDie(objLease.Ctx, run, c); err != nil {
		objLease.Cancel()
		return fmt.Errorf("leaderelection failed: %w", err)
	}

	return nil
}

func (cluster *Cluster) OnStartedLeading(c *kubevip.Config, objLease *lease.Lease,
	em *election.Manager, bgpServer *bgp.Server, signalChan chan os.Signal, isShared bool) {
	objLease.Elected.Store(true)
	objLease.Unlock()

	// When we become leader, ensure we can take over VIPs even if they're preserved on other nodes
	if !isShared {
		close(objLease.Started)
	}

	if c.PreserveVIPOnLeadershipLoss {
		log.Info("Becoming leader with VIP preservation enabled - ensuring VIP takeover")
		// Force add the VIPs (this will work even if they exist due to the precheck logic)
		for i := range cluster.Network {
			added, err := cluster.Network[i].AddIP(true, false)
			if err != nil {
				log.Error("failed to ensure VIP on leader takeover", "vip", cluster.Network[i].IP(), "err", err)
			} else if added {
				log.Info("took over VIP as new leader", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
			} else {
				log.Info("VIP already configured on interface", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
			}
		}
	}

	// As we're leading lets start the vip service
	err := cluster.vipService(objLease.Ctx, c, em, bgpServer, objLease.Cancel, signalChan)
	if err != nil {
		log.Error("starting VIP service on leader", "err", err)
		signalChan <- syscall.SIGINT
	}
}

func (cluster *Cluster) OnStoppedLeading(c *kubevip.Config, objLease *lease.Lease,
	bgpServer *bgp.Server, signalChan chan os.Signal) {
	// we can do cleanup here
	log.Info("This node is becoming a follower within the cluster")

	// Stop the cluster context if it is running
	objLease.Cancel()

	// Handle VIP cleanup based on configuration
	if c.PreserveVIPOnLeadershipLoss {
		// For IPv6, we must remove VIPs immediately to avoid DAD failures on the new leader
		// IPv6 Duplicate Address Detection will fail if the new leader tries to add an IP that is still present on this node's interface
		// We need to check each VIP individually and only remove IPv6 VIPs
		for i := range cluster.Network {
			if utils.IsIPv6(cluster.Network[i].IP()) {
				log.Info("Removing IPv6 VIP immediately (required to prevent DAD failures on new leader)", "ip", cluster.Network[i].IP())
				deleted, err := cluster.Network[i].DeleteIP()
				if err != nil {
					log.Warn("delete VIP", "err", err)
				}
				if deleted {
					log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
				}
			} else {
				log.Info("Preserving IPv4 VIP address on interface, only stopped ARP broadcasting", "ip", cluster.Network[i].IP())
			}
		}
	} else {
		// Legacy behavior: delete VIP addresses on leadership loss
		log.Info("Deleting VIP addresses on leadership loss (legacy behavior)")
		for i := range cluster.Network {
			deleted, err := cluster.Network[i].DeleteIP()
			if err != nil {
				log.Warn("delete VIP", "err", err)
			}
			if deleted {
				log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
			}
		}
	}

	log.Error("lost leadership, restarting kube-vip")
}

func (cluster *Cluster) OnNewLeader(identity string, c *kubevip.Config) {
	// we're notified when new leader elected
	log.Info("New leader", "leader", identity)

	// If we're not the new leader and we have VIPs preserved from previous leadership,
	// we need to clean them up to avoid conflicts.
	if identity != c.NodeName && c.PreserveVIPOnLeadershipLoss {
		log.Info("Cleaning up preserved VIPs as another node became leader", "new_leader", identity)
		for i := range cluster.Network {
			deleted, err := cluster.Network[i].DeleteIP()
			if err != nil {
				log.Warn("failed to cleanup preserved VIP", "vip", cluster.Network[i].IP(), "err", err)
			}
			if deleted {
				log.Info("cleaned up preserved VIP to avoid conflict", "IP", cluster.Network[i].IP(),
					"interface", cluster.Network[i].Interface(), "new_leader", identity)
			} else {
				log.Debug("VIP was not present on this node", "IP", cluster.Network[i].IP(),
					"interface", cluster.Network[i].Interface())
			}
		}
	}
}
