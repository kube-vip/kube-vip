package manager

import (
	"context"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startTableMode() error {
	var ns string
	var err error

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log.Infof("all routing table entries will exist in table [%d]", sm.config.RoutingTableID)

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go func() {
		<-sm.signalChan
		log.Info("Received termination, signaling shutdown")

		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	ns, err = returnNameSpace()
	if err != nil {
		log.Warnf("unable to auto-detect namespace, dropping to [%s]", sm.config.Namespace)
		ns = sm.config.Namespace
	}

	// Start a services watcher (all kube-vip pods will watch services), upon a new service
	// a lock based upon that service is created that they will all leaderElection on
	if sm.config.EnableServicesElection {
		log.Infof("beginning watching services, leaderelection will happen for every service")
		err = sm.startServicesWatchForLeaderElection(ctx)
		if err != nil {
			return err
		}
	} else if sm.config.EnableLeaderElection {

		log.Infof("beginning services leadership, namespace [%s], lock name [%s], id [%s]", ns, plunderLock, id)
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
					err = sm.servicesWatcher(ctx, sm.syncServices)
					if err != nil {
						log.Error(err)
					}
				},
				OnStoppedLeading: func() {
					// we can do cleanup here
					log.Infof("leader lost: %s", id)
					for _, instance := range sm.serviceInstances {
						for _, cluster := range instance.clusters {
							cluster.Stop()
						}
					}

					log.Fatal("lost leadership, restarting kube-vip")
				},
				OnNewLeader: func(identity string) {
					// we're notified when new leader elected
					if identity == id {
						// I just got the lock
						return
					}
					log.Infof("new leader elected: %s", identity)
				},
			},
		})
	} else {
		log.Infof("beginning watching services without leader election")
		err = sm.servicesWatcher(ctx, sm.syncServices)
		if err != nil {
			log.Errorf("Cannot watch services, %v", err)
		}
	}
	return nil
}
