package services

import (
	"context"
	"fmt"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/lease"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// The StartServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesWatchForLeaderElection(ctx context.Context) error {
	err := p.ServicesWatcher(ctx, p.StartServicesLeaderElection)
	if err != nil {
		return err
	}

	if p.config.EnableRoutingTable {
		for _, instance := range p.ServiceInstances {
			for _, cluster := range instance.Clusters {
				for i := range cluster.Network {
					_ = cluster.Network[i].DeleteRoute()
				}
				cluster.Stop()
			}
		}
	}

	log.Info("Shutting down kube-Vip")

	return nil
}

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesLeaderElection(ctx context.Context, service *v1.Service) error {
	serviceLease, _ := lease.GetName(service)
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

	svcCtx, err := p.getServiceContext(service.UID)
	if err != nil {
		return fmt.Errorf("failed to get context for service %q with UID %q: %w", service.Name, service.UID, err)
	}
	if svcCtx == nil {
		return fmt.Errorf("failed to get context for service %q with UID %q: nil context", service.Name, service.UID)
	}

	svcCtx.IsActive = true

	go func() {
		// wait for the service context to end and delete the lease then
		<-svcCtx.Ctx.Done()
		svcCtx.IsActive = false
		p.leaseMgr.Delete(service)
	}()

	svcLease, isNew := p.leaseMgr.Add(service)
	// this service is sharing lease
	if !isNew {
		// wait for leader election to start or context to be done
		select {
		case <-svcLease.Started:
		case <-svcLease.Ctx.Done():
			return nil
		}

		if lease.UsesCommon(service) {
			if err := p.SyncServices(ctx, service); err != nil {
				log.Error("service sync", "err", err)
				svcLease.Cancel()
			}
			// just block until context is cancelled
			<-svcCtx.Ctx.Done()
			if svcCtx.IsActive {
				if err := p.deleteService(service.UID); err != nil {
					log.Error("service deletion", "err", err)
				}
			}

			// Mark this service is inactive
			svcCtx.IsActive = false

			// wait for leaderelection to be finished
			<-svcLease.Ctx.Done()
		}
		return nil
	}
	// start the leader election code loop
	leaderelection.RunOrDie(svcLease.Ctx, leaderelection.LeaderElectionConfig{
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
					svcLease.Cancel()
				}
				close(svcLease.Started)
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
