package services

import (
	"context"
	"fmt"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
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
func (p *Processor) StartServicesLeaderElection(svcCtx *servicecontext.Context, service *v1.Service) error {
	if svcCtx == nil {
		return fmt.Errorf("no context context for service %q with UID %q: nil context", service.Name, service.UID)
	}
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

	// Start a goroutine that will delete the lease when the service context is cancelled.
	// This is important for proper cleanup when a service is deleted - it ensures that
	// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
	// Without this, RunOrDie would continue running until leadership is naturally lost.
	go func() {
		<-svcCtx.Ctx.Done()
		p.leaseMgr.Delete(service)
	}()

	svcLease, isNew := p.leaseMgr.Add(service)
	// this service is sharing lease with another service for the common lease feature
	if !isNew {
		// wait for leader election to start or context to be done
		select {
		case <-svcLease.Started:
		case <-svcLease.Ctx.Done():
			// Lease was cancelled (e.g., leader election ended), return immediately
			// This allows the restart loop to create a fresh lease
			log.Debug("lease context cancelled before leader election started", "service", service.Name, "uid", service.UID)
			svcCtx.IsActive = false
			return nil
		}

		// For non-common lease services, we need to wait for the existing leader election
		// to finish before returning. This prevents a infinite loop in startLeaderElection.
		// The lease context will be cancelled when RunOrDie returns and the defer deletes the lease.
		if !lease.UsesCommon(service) {
			log.Debug("lease already exists for non-common service, waiting for it to finish", "service", service.Name, "uid", service.UID)
			// Wait for either the service context or lease context to be done
			select {
			case <-svcCtx.Ctx.Done():
				// Service was deleted
			case <-svcLease.Ctx.Done():
				// Leader election ended (leadership lost or context cancelled)
			}
			return nil
		}

		// Common lease handling: sync the service and wait for context cancellation
		if !svcCtx.IsActive {
			if err := p.SyncServices(svcCtx, service); err != nil {
				log.Error("service sync", "err", err, "uid", service.UID)
				svcLease.Cancel()
			}
			svcCtx.IsActive = true
		}

		// Block until service context is cancelled
		<-svcCtx.Ctx.Done()

		if svcCtx.IsActive {
			if err := p.deleteService(service.UID); err != nil {
				log.Error("service deletion", "uid", service.UID, "err", err)
			}
		}

		// Mark this service is inactive
		svcCtx.IsActive = false

		// wait for leaderelection to be finished
		<-svcLease.Ctx.Done()

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
		p.leaseMgr.Delete(service)
	}()

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
			OnStartedLeading: func(_ context.Context) {
				// Mark this service as active (as we've started leading)
				// we run this in background as it's blocking
				svcCtx.IsActive = true
				if err := p.SyncServices(svcCtx, service); err != nil {
					log.Error("service sync", "uid", service.UID, "err", err)
					svcLease.Cancel()
				}
				close(svcLease.Started)
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Info("leadership lost", "service", service.Name, "uid", service.UID, "leader", p.config.NodeName)
				if svcCtx.IsActive {
					log.Debug("DELETING LEADER", "uid", service.UID)
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
				log.Info("new leader", "leader", identity, "service", service.Name, "uid", service.UID)
			},
		},
	})
	log.Info("stopping leader election", "service", service.Name, "uid", service.UID)
	return nil
}
