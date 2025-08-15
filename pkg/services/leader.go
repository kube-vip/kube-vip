package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "log/slog"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

var (
	svcLocks map[string]*sync.Mutex
)

func init() {
	svcLocks = make(map[string]*sync.Mutex)
}

// The StartServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesWatchForLeaderElection(ctx context.Context) error {
	err := p.ServicesWatcher(ctx, p.StartServicesLeaderElection)
	if err != nil {
		return err
	}

	for _, instance := range p.ServiceInstances {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				_ = cluster.Network[i].DeleteRoute()
			}
			cluster.Stop()
		}
	}

	log.Info("Shutting down kube-Vip")

	return nil
}

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesLeaderElection(ctx context.Context, service *v1.Service) error {
	serviceLease := fmt.Sprintf("kubevip-%s", service.Name)
	serviceLeaseId := fmt.Sprintf("kubevip-%s/%s", serviceLease, service.Namespace)
	log.Info("new leader election", "service", service.Name, "namespace", service.Namespace, "lock_name", serviceLease, "host_id", p.config.NodeName)
	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      serviceLease,
			Namespace: service.Namespace,
		},
		Client: p.clientSet.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: p.config.NodeName,
		},
	}
	childCtx, childCancel := context.WithCancel(ctx)
	defer childCancel()

	if _, ok := svcLocks[serviceLeaseId]; !ok {
		svcLocks[serviceLeaseId] = new(sync.Mutex)
	}

	svcLocks[serviceLeaseId].Lock()
	defer svcLocks[serviceLeaseId].Unlock()

	svcCtx, err := p.getServiceContext(service.UID)
	if err != nil {
		return fmt.Errorf("failed to get context for service %q with UID %q: %w", service.Name, service.UID, err)
	}
	if svcCtx == nil {
		return fmt.Errorf("failed to get context for service %q with UID %q: nil context", service.Name, service.UID)
	}

	svcCtx.IsActive = true

	// start the leader election code loop
	leaderelection.RunOrDie(childCtx, leaderelection.LeaderElectionConfig{
		Lock: lock,
		// IMPORTANT: you MUST ensure that any code you have that
		// is protected by the lease must terminate **before**
		// you call cancel. Otherwise, you could have a background
		// loop still running and another process could
		// get elected before your background loop finished, violating
		// the stated goal of the lease.
		ReleaseOnCancel: true,
		LeaseDuration:   time.Duration(p.config.LeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(p.config.RenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(p.config.RetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// Mark this service as active (as we've started leading)
				// we run this in background as it's blocking
				if err := p.SyncServices(ctx, service); err != nil {
					log.Error("service sync", "err", err)
					childCancel()
				}
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Info("leadership lost", "service", service.Name, "leader", p.config.NodeName)
				if svcCtx.IsActive {
					if err := p.deleteService(service.UID); err != nil {
						log.Error("service deletion", "err", err)
					}
				}
				// Mark this service is inactive
				svcCtx.IsActive = false
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if identity == p.config.NodeName {
					// I just got the lock
					return
				}
				log.Info("new leader", "leader", identity)
			},
		},
	})
	log.Info("stopping leader election", "service", service.Name)
	return nil
}
