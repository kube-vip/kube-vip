package cluster

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/utils"

	log "log/slog"
)

// StartCluster - Begins a running instance of the Leader Election cluster
func (cluster *Cluster) StartCluster(ctx context.Context, c *kubevip.Config, sm *election.Manager, bgpServer *bgp.Server) error {
	var err error

	log.Info("cluster membership", "namespace", c.Namespace, "lock", c.LeaseName, "id", c.NodeName)

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	leaderCtx, leaderCancel := context.WithCancel(ctx)
	defer leaderCancel()

	// use a Go context so we can tell the arp loop code when we
	// want to step down
	clusterCtx, clusterCancel := context.WithCancel(ctx)
	defer clusterCancel()

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	if cluster.completed == nil {
		cluster.completed = make(chan bool, 1)
		defer close(cluster.completed)
	}

	if cluster.stop == nil {
		cluster.stop = make(chan bool, 1)
	}

	go func() {
		select {
		case <-signalChan:
		case <-cluster.stop:
		}

		log.Info("Received termination, signaling cluster shutdown")
		// Cancel the leader context, which will in turn cancel the leadership
		leaderCancel()
	}()

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

	// Defer a function to check if the bgpServer has been created and if so attempt to close it
	defer func() {
		if bgpServer != nil {
			bgpServer.Close()
		}
	}()

	if c.EnableBGP && bgpServer == nil {
		// Lets start BGP
		log.Info("Starting the BGP server to advertise VIP routes to VGP peers")
		bgpServer, err = bgp.NewBGPServer(c.BGPConfig)
		if err != nil {
			log.Error("new BGP server", "err", err)
		}
		if err := bgpServer.Start(ctx, nil); err != nil {
			log.Error("starting BGP server", "err", err)
		}
	}

	run := &election.RunConfig{
		Config:           c,
		LeaseID:          c.NodeName,
		LeaseName:        c.LeaseName,
		Namespace:        c.Namespace,
		LeaseAnnotations: c.LeaseAnnotations,
		Mgr:              sm,
		OnStartedLeading: func(context.Context) { //nolint TODO: potential clean code
			// When we become leader, ensure we can take over VIPs even if they're preserved on other nodes
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

			// Start ARP advertisements now that we have leadership
			log.Info("Start ARP/NDP advertisement")
			go cluster.arpMgr.StartAdvertisement(clusterCtx)

			// As we're leading lets start the vip service
			err := cluster.vipService(clusterCtx, c, sm, bgpServer, leaderCancel)
			if err != nil {
				log.Error("starting VIP service on leader", "err", err)
				signalChan <- syscall.SIGINT
			}
		},
		OnStoppedLeading: func() {
			// we can do cleanup here
			log.Info("This node is becoming a follower within the cluster")

			// Stop the cluster context if it is running
			clusterCancel()

			// Stop the BGP server
			if bgpServer != nil {
				err := bgpServer.Close()
				if err != nil {
					log.Warn("close BGP server", "err", err)
				}
			}

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
			signalChan <- syscall.SIGINT
		},
		OnNewLeader: func(identity string) {
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
						log.Info("cleaned up preserved VIP to avoid conflict", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface(), "new_leader", identity)
					} else {
						log.Debug("VIP was not present on this node", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
					}
				}
			}
		},
	}

	if err := election.RunOrDie(leaderCtx, run, c); err != nil {
		return fmt.Errorf("leaderelection failed: %w", err)
	}

	return nil
}
