package cluster

import (
	"context"
	"fmt"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"

	log "log/slog"
)

// StartCluster - Begins a running instance of the Leader Election cluster
func (cluster *Cluster) StartCluster(ctx context.Context, c *kubevip.Config,
	em *election.Manager, bgpServer *bgp.Server, leaseMgr *lease.Manager, killFunc func()) error {

	ns, leaseName := lease.NamespaceName(c.LeaseName, c)

	leaseID := lease.NewID(c.LeaderElectionType, ns, leaseName)

	log.Info("cluster membership", "namespace", leaseID.Namespace(), "lock", leaseID.Name(), "id", c.NodeName)

	objectName := lease.ObjectName(leaseID, "cp")
	objLease, _ := leaseMgr.Acquire(context.Background(), leaseID, objectName)
	defer leaseMgr.Delete(leaseID, objectName, objLease)

	wg := sync.WaitGroup{}
	defer wg.Wait()

	electionCtx, cancelElection := objLease.NewElectionContext(ctx)
	defer cancelElection()

	stop := cluster.StopChannel()
	wg.Go(func() {
		select {
		case <-stop:
			cancelElection()
		case <-electionCtx.Done():
		}
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

	for {
		if !objLease.BeginElection() {
			log.Debug("this election was already done, shared lease", "lease", leaseName)
			leaderGeneration, elected := objLease.WaitForLeaderGeneration(electionCtx)
			if !elected {
				if electionCtx.Err() != nil {
					return nil
				}
				// The runner that owned this shared lease's election ended it
				// without ever being elected; take over the campaign ourselves
				// instead of leaving the lease without an active runner.
				continue
			}

			leaderCtx, cancelLeader := context.WithCancel(electionCtx)
			leaderWG := sync.WaitGroup{}
			leaderWG.Go(func() {
				cluster.OnStartedLeading(leaderCtx, c, em, bgpServer, killFunc, true)
			})

			log.Debug("cluster waiting for shared election to finish", "lease", leaseName)
			objLease.WaitForElectionEndAfter(electionCtx, leaderGeneration)
			cancelLeader()
			leaderWG.Wait()

			cluster.OnStoppedLeading(c, bgpServer)

			return nil
		}
		break
	}
	defer objLease.ElectionStopped()

	run := &election.RunConfig{
		Config:           c,
		LeaseID:          leaseID,
		LeaseAnnotations: c.LeaseAnnotations,
		VIPs:             controlPlaneElectionVIPs(c),
		Mgr:              em,
		OnStartedLeading: func(ctx context.Context) {
			objLease.ElectionStarted()
			cluster.OnStartedLeading(ctx, c, em, bgpServer, killFunc, false)
		},
		OnStoppedLeading: func() {
			objLease.ElectionStopped()
			cluster.OnStoppedLeading(c, bgpServer)
		},
		OnNewLeader: func(identity string) {
			cluster.OnNewLeader(identity, c)
		},
	}

	if err := election.RunOrDie(electionCtx, run, c); err != nil {
		cluster.Stop()
		return fmt.Errorf("leaderelection failed: %w", err)
	}

	return nil
}

func controlPlaneElectionVIPs(config *kubevip.Config) []string {
	configured := config.VIP
	if config.Address != "" {
		configured = config.Address
	}
	return vip.Split(configured)
}

func (cluster *Cluster) OnStartedLeading(ctx context.Context, c *kubevip.Config,
	em *election.Manager, bgpServer *bgp.Server, killFunc func(), _ bool) {
	labels := generateLabelsFromConfig(c.Address, kubevip.HasIP)
	if err := cluster.nodeLabelMgr.AddLabel(labels); err != nil {
		log.Error("error adding label to node", "err", err)
	}
	cluster.labelAdded = true

	// As we're leading lets start the vip service
	err := cluster.StartVipService(ctx, c, em, bgpServer, killFunc)
	if err != nil {
		log.Error("starting VIP service on leader", "err", err)
		killFunc()
	}
}

func (cluster *Cluster) OnStoppedLeading(c *kubevip.Config, bgpServer *bgp.Server) {
	// we can do cleanup here
	log.Info("This node is becoming a follower within the cluster")

	if cluster.labelAdded {
		labels := generateLabelsFromConfig(c.Address, kubevip.HasIP)
		if err := cluster.nodeLabelMgr.RemoveLabel(labels); err != nil {
			log.Error("error removing label from node", "err", err)
		}
		cluster.labelAdded = false
	}

	cluster.cleanupVIPs(c)

	log.Error("lost leadership, restarting kube-vip")
}

func (cluster *Cluster) OnNewLeader(identity string, c *kubevip.Config) {
	// we're notified when new leader elected
	log.Info("New leader", "leader", identity)
}

func generateLabelsFromConfig(addr, labelKey string) map[string]string {
	return map[string]string{
		labelKey: utils.SanitizeIPForLabel(addr),
	}
}
