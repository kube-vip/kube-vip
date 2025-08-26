package manager

import (
	"context"
	"fmt"
	"syscall"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startTableMode(id string) error {
	var cpCluster *cluster.Cluster
	var err error

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log.Info("destination for routes", "table", sm.config.RoutingTableID, "protocol", sm.config.RoutingProtocol)

	if sm.config.CleanRoutingTable {
		go func() {
			// we assume that after 10s all services should be configured so we can delete redundant routes
			time.Sleep(time.Second * 10)
			if err := sm.cleanRoutes(); err != nil {
				log.Error("error checking for old routes", "err", err)
			}
		}()
	}

	if sm.config.EgressClean {
		vip.ClearIPTables(sm.config.EgressWithNftables, sm.config.ServiceNamespace, iptables.ProtocolIPv4)
		vip.ClearIPTables(sm.config.EgressWithNftables, sm.config.ServiceNamespace, iptables.ProtocolIPv6)
		log.Debug("IPtables rules cleaned on startup")
	}

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go func() {
		<-sm.signalChan
		log.Info("Received kube-vip termination, signaling shutdown")
		if sm.config.EnableControlPlane {
			cpCluster.Stop()
		}

		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	if sm.config.EnableServices {
		log.Debug("starting Services")
		ns, err := returnNameSpace()
		if err != nil {
			log.Warn("unable to auto-detect namespace", "dropping to", sm.config.Namespace)
			ns = sm.config.Namespace
		}

		// Start a services watcher (all kube-vip pods will watch services), upon a new service
		// a lock based upon that service is created that they will all leaderElection on
		if sm.config.EnableServicesElection {
			log.Info("beginning watching services, leaderelection will happen for every service")
			err = sm.svcProcessor.StartServicesWatchForLeaderElection(ctx)
			if err != nil {
				return err
			}
		} else if sm.config.EnableLeaderElection {

			log.Info("beginning services leadership", "namespace", ns, "lock name", plunderLock, "id", id)
			// we use the Lease lock type since edits to Leases are less common
			// and fewer objects in the cluster watch "all Leases".
			lock := &resourcelock.LeaseLock{
				LeaseMeta: metav1.ObjectMeta{
					Name:      plunderLock,
					Namespace: ns,
				},
				Client: sm.clientSet.CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{
					Identity: id,
				},
			}
			// start the leader election code loop
			leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
				Lock: lock,
				// IMPORTANT: you MUST ensure that any code you have that
				// is protected by the lease must terminate **before**
				// you call cancel. Otherwise, you could have a background
				// loop still running and another process could
				// get elected before your background loop finished, violating
				// the stated goal of the lease.
				ReleaseOnCancel: true,
				LeaseDuration:   time.Duration(sm.config.LeaseDuration) * time.Second,
				RenewDeadline:   time.Duration(sm.config.RenewDeadline) * time.Second,
				RetryPeriod:     time.Duration(sm.config.RetryPeriod) * time.Second,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: func(ctx context.Context) {
						err = sm.svcProcessor.ServicesWatcher(ctx, sm.svcProcessor.SyncServices)
						if err != nil {
							log.Error(err.Error())
							panic("")
						}
					},
					OnStoppedLeading: func() {
						// we can do cleanup here
						sm.mutex.Lock()
						defer sm.mutex.Unlock()
						log.Info("leader lost", "id", id)
						sm.svcProcessor.Stop()

						log.Error("lost leadership, restarting kube-vip")
						panic("")
					},
					OnNewLeader: func(identity string) {
						// we're notified when new leader elected
						if identity == id {
							// I just got the lock
							return
						}
						log.Info("new leader elected", "id", identity)
					},
				},
			})
		} else {
			log.Info("beginning watching services without leader election")
			err = sm.svcProcessor.ServicesWatcher(ctx, sm.svcProcessor.SyncServices)
			if err != nil {
				log.Error("Cannot watch services", "err", err)
			} else {
				log.Debug("watching services")
			}
		}
	}
	if sm.config.EnableControlPlane {
		log.Debug("initCluster for ControlPlane")
		cpCluster, err = cluster.InitCluster(sm.config, false, sm.intfMgr, sm.arpMgr)
		if err != nil {
			log.Debug("init of ControlPlane NOT successful")
			return fmt.Errorf("cluster initialization error: %w", err)
		} else {
			log.Debug("init of ControlPlane successful")
		}
		log.Debug("init ClusterManager")
		clusterManager, err := initClusterManager(sm)
		if err != nil {
			log.Debug("init cluster manager NOT successful")
			return fmt.Errorf("cluster manager initialization error: %w", err)
		} else {
			log.Debug("init ClusterManager successful")
		}

		if err := cpCluster.StartVipService(sm.config, clusterManager, nil); err != nil {
			log.Error("Control Plane", "err", err)
			// Trigger the shutdown of this manager instance
			sm.signalChan <- syscall.SIGINT
		} else {
			log.Debug("start VipServer for cluster manager successful")
		}
	}

	return nil
}

func (sm *Manager) cleanRoutes() error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	routes, err := vip.ListRoutes(sm.config.RoutingTableID, sm.config.RoutingProtocol)
	if err != nil {
		return fmt.Errorf("error getting routes: %w", err)
	}

	for i := range routes {
		found := false
		if sm.config.EnableControlPlane {
			found = (routes[i].Dst.IP.String() == sm.config.Address)
		} else {
			found = endpoints.CountRouteReferences(&routes[i], &sm.svcProcessor.ServiceInstances) > 0
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
