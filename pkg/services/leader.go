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
	claimID := lease.ServiceClaimID(service)
	svcLease, err := p.claimServiceLease(svcCtx, service, id, claimID)
	if err != nil {
		return err
	}
	if svcLease == nil {
		metrics.ServiceElectionErrorsTotal.WithLabelValues(service.Namespace, service.Name, "no_lease").Inc()
		return fmt.Errorf("no existing lease found for service %q with UID %q", service.Name, service.UID)
	}
	if err := svcLease.Ctx.Err(); err != nil {
		return fmt.Errorf("lease context cancelled before election start: %w", err)
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
			return fmt.Errorf("start shared-lease service: %w", err)
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
	activationErr := make(chan error, 1)

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
				select {
				case activationErr <- err:
				default:
				}
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
	select {
	case err := <-activationErr:
		metrics.ServiceElectionErrorsTotal.WithLabelValues(service.Namespace, service.Name, "service_sync").Inc()
		return fmt.Errorf("start service after election: %w", err)
	default:
	}

	log.Info("stopping leader election", "service", service.Name, "uid", service.UID)
	return nil
}

// claimServiceLease admits only the context currently registered for this
// Service. Holding the Service key prevents an old callback from claiming a
// replacement's lease while AddOrModify installs the replacement context.
func (p *Processor) claimServiceLease(svcCtx *servicecontext.Context, service *v1.Service, id lease.ID, claimID string) (*lease.Lease, error) {
	unlockService := p.lockService(service.UID)
	defer unlockService()

	currentCtx, err := p.getServiceContext(service.UID)
	if err != nil {
		return nil, err
	}
	if currentCtx != svcCtx {
		return nil, fmt.Errorf("service context superseded before election start")
	}
	if err := svcCtx.Ctx.Err(); err != nil {
		return nil, fmt.Errorf("service context cancelled before election start: %w", err)
	}

	svcLease, isNew := p.leaseMgr.Claim(id, claimID)
	if isNew {
		go func() {
			<-svcCtx.Ctx.Done()
			p.releaseServiceLease(svcCtx, service, id, claimID, svcLease)
		}()
	}
	return svcLease, nil
}

// releaseServiceLease releases a lease membership only while svcCtx remains
// the current context for the Service. A replacement can share the same lease,
// so an old cleanup goroutine must not remove the replacement's membership.
func (p *Processor) releaseServiceLease(svcCtx *servicecontext.Context, service *v1.Service, id lease.ID, claimID string, svcLease *lease.Lease) {
	unlockService := p.lockService(service.UID)
	defer unlockService()

	currentCtx, err := p.getServiceContext(service.UID)
	if err != nil || currentCtx != svcCtx {
		return
	}
	p.leaseMgr.Delete(id, claimID, svcLease)
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
