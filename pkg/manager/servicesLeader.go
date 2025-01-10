package manager

import (
	"context"
	"fmt"
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

	for _, instance := range sm.serviceInstances {
		for _, cluster := range instance.clusters {
			for i := range cluster.Network {
				_ = cluster.Network[i].DeleteRoute()
			}
			cluster.Stop()
		}
	}

	log.Infof("Shutting down kube-Vip")

	return nil
}

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (sm *Manager) StartServicesLeaderElection(ctx context.Context, service *v1.Service, wg *sync.WaitGroup) error {
	serviceLease := fmt.Sprintf("kubevip-%s", service.Name)
	log.Infof("(svc election) service [%s], namespace [%s], lock name [%s], host id [%s]", service.Name, service.Namespace, serviceLease, sm.config.NodeName)
	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      serviceLease,
			Namespace: service.Namespace,
		},
		Client: sm.clientSet.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: sm.config.NodeName,
		},
	}
	parentCtx, parentCancel := context.WithCancel(ctx)
	defer parentCancel()

	activeService[string(service.UID)] = true
	// start the leader election code loop
	leaderelection.RunOrDie(parentCtx, leaderelection.LeaderElectionConfig{
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
				// Mark this service as active (as we've started leading)
				// we run this in background as it's blocking
				wg.Add(1)
				go func() {
					if err := sm.syncServices(ctx, service, wg); err != nil {
						log.Errorln(err)
						parentCancel()
					}
				}()
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Infof("(svc election) service [%s] leader lost: [%s]", service.Name, sm.config.NodeName)
				if activeService[string(service.UID)] {
					if err := sm.deleteService(string(service.UID)); err != nil {
						log.Errorln(err)
					}
				}
				// Mark this service is inactive
				activeService[string(service.UID)] = false
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if identity == sm.config.NodeName {
					// I just got the lock
					return
				}
				log.Infof("(svc election) new leader elected: %s", identity)
			},
		},
	})
	log.Infof("(svc election) for service [%s] stopping", service.Name)
	return nil
}
