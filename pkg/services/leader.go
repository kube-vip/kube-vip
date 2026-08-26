package services

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

// The StartServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesWatchForLeaderElection(ctx context.Context, forcedOnly bool) error {
	err := p.ServicesWatcher(ctx, NewCallback(p.StartServicesLeaderElection, true), forcedOnly)
	if err != nil {
		return err
	}

	if p.config.EnableRoutingTable {
		p.routeMgr.Clear()
	}

	log.Info("Shutting down kube-Vip")

	return nil
}

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesLeaderElection(svcCtx *servicecontext.Context, service *v1.Service, _ *sync.WaitGroup, _ bool) error {
	if svcCtx == nil {
		return fmt.Errorf("no context context for service %q with UID %q: nil context", service.Name, service.UID)
	}

	leaseNamespace, serviceLease := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)
	objectName := lease.ServiceNamespacedName(service)

	svcLease, isNew := p.leaseMgr.Claim(id, objectName)
	if svcLease == nil {
		metrics.ServiceElectionErrorsTotal.WithLabelValues(service.Namespace, service.Name, "no_lease").Inc()
		return fmt.Errorf("no existing lease found for service %q with UID %q", service.Name, service.UID)
	}
	if err := svcLease.Ctx.Err(); err != nil {
		return fmt.Errorf("lease context cancelled before election start: %w", err)
	}

	// A cancelled service context means this call belongs to a torn-down incarnation of
	// the service. Its replacement is built as Cancel -> Delete -> Add, so the lease
	// fetched above may already be the replacement's. Registering on it here would let
	// the cleanup goroutine below retire a lease that is still in use.
	if err := svcCtx.Ctx.Err(); err != nil {
		return fmt.Errorf("service context cancelled before election start: %w", err)
	}

	// Start a goroutine that will delete the lease when the service context is cancelled.
	// This is important for proper cleanup when a service is deleted - it ensures that
	// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
	// Without this, RunOrDie would continue running until leadership is naturally lost.
	//
	// This must NOT be tracked by wg: it only completes once the service is deleted
	// (svcCtx.Ctx.Done()), which is normally long after this function itself returns
	// e.g. on an ordinary leadership loss such as a lease renewal failure. If it were
	// added to wg, the deferred wg.Wait() above would block this function and with it
	// the leader-election restart loop in startLeaderElection that calls it forever,
	// until the Service was deleted (and recreated), even though the endpoint was still
	// healthy and a new election should have started immediately.
	if isNew {
		go func() {
			<-svcCtx.Ctx.Done()
			p.leaseMgr.Delete(id, objectName, svcLease)
		}()
	}

	select {
	case <-svcCtx.Ctx.Done():
		return fmt.Errorf("service context cancelled before election start: %w", svcCtx.Ctx.Err())
	case <-svcLease.Ctx.Done():
		return fmt.Errorf("lease context cancelled before election start: %w", svcLease.Ctx.Err())
	case <-svcCtx.Readiness():
	}

	if !svcLease.BeginElection() {
		wg := sync.WaitGroup{}
		defer wg.Wait()
		if !svcLease.WaitForLeader(svcCtx.Ctx) {
			return nil
		}

		if err := p.onStartedLeading(svcCtx, service, &wg); err != nil {
			log.Error("error on started leading", "error", err)
		}

		svcLease.WaitForElectionEnd(svcCtx.Ctx)

		if err := p.onStoppedLeading(svcCtx, svcLease, service); err != nil {
			log.Error("error on stopped leading", "error", err)
		}

		return nil
	}
	wg := sync.WaitGroup{}
	defer svcLease.ElectionStopped()
	defer wg.Wait()

	log.Info("new leader election", "service", service.Name, "namespace", service.Namespace, "lock_name", serviceLease, "host_id", p.config.NodeName)
	leaderCtx, leaderCancel := context.WithCancel(svcLease.Ctx)
	svcCtx.SetLeaderCancel(leaderCancel)

	run := election.RunConfig{
		Config:           p.config,
		LeaseID:          id,
		Mgr:              p.electionMgr,
		LeaseAnnotations: map[string]string{},

		OnStartedLeading: func(_ context.Context) {
			svcLease.ElectionStarted()
			// Mark this service as active (as we've started leading)
			// we run this in background as it's blocking
			if err := p.onStartedLeading(svcCtx, service, &wg); err != nil {
				leaderCancel()
			}
			metrics.LeaderTransitionsTotal.WithLabelValues(id.Name()).Inc()
			metrics.IsLeader.WithLabelValues(p.config.NodeName, id.Name()).Set(1)
		},
		OnStoppedLeading: func() {
			// we can do cleanup here
			log.Info("leadership lost", "service", service.Name, "uid", service.UID, "leader", p.config.NodeName)
			if err := p.onStoppedLeading(svcCtx, svcLease, service); err != nil {
				metrics.ServiceReconcileErrorsTotal.WithLabelValues(service.Namespace, service.Name, "delete_service").Inc()
				leaderCancel()
			}
			metrics.IsLeader.WithLabelValues(p.config.NodeName, id.Name()).Set(0)
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

	if err := election.RunOrDie(leaderCtx, &run, p.config); err != nil {
		return fmt.Errorf("services election failed: %w", err)
	}

	log.Info("stopping leader election", "service", service.Name, "uid", service.UID)
	return nil
}

func (p *Processor) onStartedLeading(svcCtx *servicecontext.Context, service *v1.Service, wg *sync.WaitGroup) error {
	err := p.SyncServices(svcCtx, service, wg, true)
	if err != nil {
		log.Error("service sync", "uid", service.UID, "err", err)
		return err
	}
	return nil
}

func (p *Processor) onStoppedLeading(svcCtx *servicecontext.Context, svcLease *lease.Lease, service *v1.Service) error {
	currentSvcCtx, err := p.getServiceContext(service.UID)
	if err != nil {
		return err
	}
	if currentSvcCtx != nil && currentSvcCtx != svcCtx {
		log.Debug("skipping cleanup from superseded service context", "service", service.Name, "uid", service.UID)
		return nil
	}

	log.Debug("deleting service due to lost leadership", "uid", service.UID)
	err = p.deleteService(context.WithoutCancel(svcLease.Ctx), service.UID, svcCtx)
	if err != nil {
		log.Error("service deletion", "err", err)
		return err
	}
	return nil
}
