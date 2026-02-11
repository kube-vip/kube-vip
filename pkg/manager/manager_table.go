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
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startTableMode(ctx context.Context, id string) error {
	var cpCluster *cluster.Cluster
	var err error

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	rtCtx, cancel := context.WithCancel(ctx)
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

	if sm.config.EnableControlPlane {
		log.Debug("initCluster for ControlPlane")
		cpCluster, err = cluster.InitCluster(sm.config, false, sm.intfMgr, sm.arpMgr)
		if err != nil {
			log.Debug("init of ControlPlane NOT successful")
			return fmt.Errorf("cluster initialization error: %w", err)
		}
		log.Debug("init of ControlPlane successful")
	}

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go sm.waitForShutdown(rtCtx, cancel, cpCluster)

	if sm.config.EnableControlPlane {
		log.Debug("init ClusterManager")
		clusterManager, err := initClusterManager(sm)
		if err != nil {
			log.Debug("init cluster manager NOT successful")
			return fmt.Errorf("cluster manager initialization error: %w", err)
		}
		log.Debug("init ClusterManager successful")
		if err := cpCluster.StartVipService(rtCtx, sm.config, clusterManager, nil); err != nil {
			log.Error("Control Plane", "err", err)
			// Trigger the shutdown of this manager instance
			if !sm.closing.Load() {
				sm.signalChan <- syscall.SIGINT
			}
		} else {
			log.Debug("start VipServer for cluster manager successful")
		}
	}

	if sm.config.EnableServices {
		// Start a services watcher (all kube-vip pods will watch services), upon a new service
		// a lock based upon that service is created that they will all leaderElection on
		if sm.config.EnableServicesElection {
			log.Info("beginning watching services, leaderelection will happen for every service")
			err = sm.svcProcessor.StartServicesWatchForLeaderElection(rtCtx)
			if err != nil {
				return err
			}
		} else if sm.config.EnableLeaderElection {
			ns, leaseName := lease.NamespaceName(sm.config.ServicesLeaseName, sm.config)

			log.Info("beginning services leadership", "namespace", ns, "lock name", leaseName, "id", id)

			leaseID := fmt.Sprintf("%s/%s", ns, leaseName)
			objectName := fmt.Sprintf("%s-svc", leaseID)

			objLease, newLease, sharedLease := sm.leaseMgr.Add(leaseID, objectName)

			// this service was already processed so we do not need to do anything
			if !newLease {
				log.Debug("this election was already done, waiting for it to finish", "lease", leaseName)
				// Wait for either the service context or lease context to be done
				select {
				case <-ctx.Done():
					// Service was deleted
					sm.leaseMgr.Delete(leaseID, objectName)
				case <-objLease.Ctx.Done():
					// Leader election ended (leadership lost or context cancelled)
				}
				return nil
			}

			// Start a goroutine that will delete the lease when the service context is cancelled.
			// This is important for proper cleanup when a service is deleted - it ensures that
			// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
			// Without this, RunOrDie would continue running until leadership is naturally lost.
			go func() {
				<-ctx.Done()
				sm.leaseMgr.Delete(leaseID, objectName)
			}()

			// this object is sharing lease with another object
			if sharedLease {
				log.Debug("this election was already done, shared lease", "lease", leaseName)
				// wait for leader election to start or context to be done
				select {
				case <-objLease.Started:
				case <-objLease.Ctx.Done():
					// Lease was cancelled (e.g., leader election ended), return immediately
					// This allows the restart loop to create a fresh lease
					log.Debug("lease context cancelled before leader election started", "lease", leaseName)
					return nil
				}

				err = sm.svcProcessor.ServicesWatcher(ctx, sm.svcProcessor.SyncServices)
				if err != nil {
					log.Error("service watcher", "err", err)
					if !sm.closing.Load() {
						sm.signalChan <- syscall.SIGINT
					}
					objLease.Cancel()
				}

				log.Debug("waiting for context to finish", "lease", leaseName)
				// Block until context is cancelled
				<-ctx.Done()

				log.Debug("waiting for lease to finish", "lease", leaseName)
				// wait for leaderelection to be finished
				<-objLease.Ctx.Done()

				// we can do cleanup here
				sm.mutex.Lock()
				defer sm.mutex.Unlock()
				log.Info("leader lost", "lease", leaseName)
				sm.svcProcessor.Stop()

				log.Error("lost services leadership, restarting kube-vip")
				if !sm.closing.Load() {
					sm.signalChan <- syscall.SIGINT
				}

				return nil
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
				sm.leaseMgr.Delete(leaseID, objectName)
			}()

			// we use the Lease lock type since edits to Leases are less common
			// and fewer objects in the cluster watch "all Leases".
			lock := &resourcelock.LeaseLock{
				LeaseMeta: metav1.ObjectMeta{
					Name:      leaseName,
					Namespace: ns,
				},
				Client: sm.clientSet.CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{
					Identity: id,
				},
			}
			// start the leader election code loop
			leaderelection.RunOrDie(rtCtx, leaderelection.LeaderElectionConfig{
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
						close(objLease.Started)
						err = sm.svcProcessor.ServicesWatcher(ctx, sm.svcProcessor.SyncServices)
						if err != nil {
							log.Error(err.Error())
							sm.signalChan <- syscall.SIGINT
						}
					},
					OnStoppedLeading: func() {
						// we can do cleanup here
						sm.mutex.Lock()
						defer sm.mutex.Unlock()
						log.Info("leader lost", "id", id)
						sm.svcProcessor.Stop()

						log.Error("lost services leadership, restarting kube-vip")
						sm.signalChan <- syscall.SIGINT
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
			err = sm.svcProcessor.ServicesWatcher(rtCtx, sm.svcProcessor.SyncServices)
			if err != nil {
				return fmt.Errorf("cannot watch services: %w", err)
			} else {
				log.Debug("watching services")
			}
		}
	}

	<-sm.shutdownChan
	log.Info("Shutting down Kube-Vip")

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
