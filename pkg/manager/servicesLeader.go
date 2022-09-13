package manager

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (sm *Manager) startServicesWatchForLeaderElection(ctx context.Context) error {

	err := sm.servicesWatcher(ctx, sm.StartServicesLeaderElection)
	if err != nil {
		return err
	}

	for x := range sm.serviceInstances {
		sm.serviceInstances[x].cluster.Stop()
	}

	log.Infof("Shutting down kube-Vip")

	return nil
}

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (sm *Manager) StartServicesLeaderElection(ctx context.Context, service *v1.Service, wg *sync.WaitGroup) error {

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	serviceLease := fmt.Sprintf("kubevip-%s", service.Name)
	log.Infof("beginning services leadership, namespace [%s], lock name [%s], id [%s]", service.Namespace, serviceLease, id)
	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      serviceLease,
			Namespace: service.Namespace,
		},
		Client: sm.clientSet.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}
	activeService[string(service.UID)] = true
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
		LeaseDuration:   60 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// we run this in background as it's blocking
				go func() {
					//sm.syncServices(ctx, service, wg)
					if err := sm.syncServices(ctx, service, wg); err != nil {
						log.Errorln(err)
					}
				}()
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Infof("leader lost: %s", id)
				if err := sm.deleteService(string(service.UID)); err != nil {
					log.Errorln(err)
				}
				// Mark this service is inactive
				activeService[string(service.UID)] = false
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
	return nil
}
