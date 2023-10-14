package manager

import (
	"context"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/kamhlos/upnp"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startARP() error {
	var cpCluster *cluster.Cluster
	var ns string
	var err error

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go func() {
		<-sm.signalChan
		log.Info("Received kube-vip termination, signaling shutdown")
		if sm.config.EnableControlPlane {
			cpCluster.Stop()
		}
		// Close all go routines
		close(sm.shutdownChan)
		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	if sm.config.EnableControlPlane {
		cpCluster, err = cluster.InitCluster(sm.config, false)
		if err != nil {
			return err
		}

		clusterManager, err := initClusterManager(sm)
		if err != nil {
			return err
		}

		go func() {
			err := cpCluster.StartCluster(sm.config, clusterManager, nil)
			if err != nil {
				log.Errorf("Control Plane Error [%v]", err)
				// Trigger the shutdown of this manager instance
				sm.signalChan <- syscall.SIGINT

			}
		}()

		// Check if we're also starting the services, if not we can sit and wait on the closing channel and return here
		if !sm.config.EnableServices {
			<-sm.signalChan
			log.Infof("Shutting down Kube-Vip")

			return nil
		}

		ns = sm.config.Namespace
	} else {

		ns, err = returnNameSpace()
		if err != nil {
			log.Warnf("unable to auto-detect namespace, dropping to [%s]", sm.config.Namespace)
			ns = sm.config.Namespace
		}
	}

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// Before starting the leader Election enable any additional functionality
	upnpEnabled, _ := strconv.ParseBool(os.Getenv("enableUPNP"))

	if upnpEnabled {
		sm.upnp = new(upnp.Upnp)
		err := sm.upnp.ExternalIPAddr()
		if err != nil {
			log.Errorf("Error Enabling UPNP %s", err.Error())
			// Set the struct to nil so nothing should use it in future
			sm.upnp = nil
		} else {
			log.Infof("Successfully enabled UPNP, Gateway address [%s]", sm.upnp.GatewayOutsideIP)
		}
	}

	// This will tidy any dangling kube-vip iptables rules
	if os.Getenv("EGRESS_CLEAN") != "" {
		i, err := vip.CreateIptablesClient(sm.config.EgressWithNftables, sm.config.ServiceNamespace)
		if err != nil {
			log.Warnf("[egress] Unable to clean any dangling egress rules [%v]", err)
		} else {
			log.Info("[egress] Cleaning any dangling kube-vip egress rules")
			cleanErr := i.CleanIPtables()
			if cleanErr != nil {
				log.Errorf("Error cleaning rules [%v]", cleanErr)
			}
		}
	}

	// Start a services watcher (all kube-vip pods will watch services), upon a new service
	// a lock based upon that service is created that they will all leaderElection on
	if sm.config.EnableServicesElection {
		log.Infof("beginning watching services, leaderelection will happen for every service")
		err = sm.startServicesWatchForLeaderElection(ctx)
		if err != nil {
			return err
		}
	} else {

		log.Infof("beginning services leadership, namespace [%s], lock name [%s], id [%s]", ns, sm.config.ServicesLeaseName, id)
		// we use the Lease lock type since edits to Leases are less common
		// and fewer objects in the cluster watch "all Leases".
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      sm.config.ServicesLeaseName,
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
					err = sm.servicesWatcher(ctx, sm.syncServices)
					if err != nil {
						log.Error(err)
					}
				},
				OnStoppedLeading: func() {
					// we can do cleanup here
					log.Infof("leader lost: %s", id)
					for x := range sm.serviceInstances {
						sm.serviceInstances[x].cluster.Stop()
					}

					log.Fatal("lost leadership, restarting kube-vip")
				},
				OnNewLeader: func(identity string) {
					// we're notified when new leader elected
					if sm.config.EnableNodeLabeling {
						applyNodeLabel(sm.clientSet, sm.config.Address, id, identity)
					}
					if identity == id {
						// I just got the lock
						return
					}
					log.Infof("new leader elected: %s", identity)
				},
			},
		})
	}
	return nil
}
