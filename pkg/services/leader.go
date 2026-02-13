package services

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
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

	p.loadbalancersWg.Wait()

	log.Info("shutting down kube-vip")

	return nil
}

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesLeaderElection(svcCtx *servicecontext.Context, service *v1.Service) error {
	if svcCtx == nil {
		return fmt.Errorf("no context context for service %q with UID %q: nil context", service.Name, service.UID)
	}

	serviceLease, serviceLeaseID, leaseNamespace := lease.ServiceName(service)
	objectName := lease.ServiceNamespacedName(service)

	svcLease, newService, sharedLease := p.leaseMgr.Add(serviceLeaseID, objectName)

	// this service was already processed so we do not need to do anything
	if !newService {
		log.Debug("this service was already handled, waiting for it to finish", "service", service.Name, "uid", service.UID)
		// Wait for either the service context or lease context to be done
		select {
		case <-svcCtx.Ctx.Done():
			// Service was deleted
			p.leaseMgr.Delete(serviceLeaseID, objectName)
		case <-svcLease.Ctx.Done():
			// Leader election ended (leadership lost or context cancelled)
		}
		return nil
	}

	// Start a goroutine that will delete the lease when the service context is cancelled.
	// This is important for proper cleanup when a service is deleted - it ensures that
	// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
	// Without this, RunOrDie would continue running until leadership is naturally lost.
	wg := sync.WaitGroup{}
	wg.Go(func() {
		<-svcCtx.Ctx.Done()
		p.leaseMgr.Delete(serviceLeaseID, objectName)
	})

	// this service is sharing lease with another service
	if sharedLease {
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
			// we have no context left here so we use a new one
			if err := p.deleteService(context.TODO(), service.UID); err != nil {
				log.Error("service deletion", "uid", service.UID, "err", err)
			}
		}

		// Mark this service is inactive
		svcCtx.IsActive = false

		// wait for leaderelection to be finished
		<-svcLease.Ctx.Done()

		wg.Wait()

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
		p.leaseMgr.Delete(serviceLeaseID, objectName)
	}()

	log.Info("new leader election", "service", service.Name, "namespace", service.Namespace, "lock_name", serviceLease, "host_id", p.config.NodeName)

	run := election.RunConfig{
		Config:           p.config,
		LeaseID:          p.config.NodeName,
		LeaseName:        serviceLease,
		Namespace:        leaseNamespace,
		Mgr:              p.electionMgr,
		LeaseAnnotations: map[string]string{},

		OnStartedLeading: func(_ context.Context) {
			close(svcLease.Started)
			// Mark this service as active (as we've started leading)
			// we run this in background as it's blocking
			svcCtx.IsActive = true
			if err := p.SyncServices(svcCtx, service); err != nil {
				log.Error("service sync", "uid", service.UID, "err", err)
				svcLease.Cancel()
			}
		},
		OnStoppedLeading: func() {
			// we can do cleanup here
			log.Info("leadership lost", "service", service.Name, "uid", service.UID, "leader", p.config.NodeName)
			if svcCtx.IsActive {
				log.Debug("deleting service due to lost leadership", "uid", service.UID)
				if err := p.deleteService(svcCtx.Ctx, service.UID); err != nil {
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
	}

	if err := election.RunOrDie(svcLease.Ctx, &run, p.config); err != nil {
		return fmt.Errorf("services election failed: %w", err)
	}

	log.Info("stopping leader election", "service", service.Name, "uid", service.UID)

	wg.Wait()
	return nil
}
