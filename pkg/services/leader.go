package services

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

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

// StartServicesLeaderElection watches one Service's endpoint readiness while
// its per-lease coordinator owns campaign lifetime.
func (p *Processor) StartServicesLeaderElection(svcCtx *servicecontext.Context, service *v1.Service,
	wg *sync.WaitGroup, _ bool) error {
	if service == nil {
		return fmt.Errorf("no service for leader election")
	}
	if svcCtx == nil {
		return fmt.Errorf("no context for service %q with UID %q", service.Name, service.UID)
	}
	currentContext, err := p.getServiceContext(service.UID)
	if err != nil {
		return fmt.Errorf("get current service context: %w", err)
	}
	if currentContext != svcCtx {
		return fmt.Errorf("service context is no longer current for service %q with UID %q", service.Name, service.UID)
	}
	if err := svcCtx.Ctx.Err(); err != nil {
		return fmt.Errorf("service context cancelled before election start: %w", err)
	}
	if _, loaded := p.electionLoops.LoadOrStore(svcCtx, struct{}{}); loaded {
		return nil
	}
	defer p.electionLoops.Delete(svcCtx)
	if wg == nil {
		wg = &sync.WaitGroup{}
	}
	loops := metrics.ServiceElectionLoops.WithLabelValues(service.Namespace, service.Name)
	loops.Inc()
	defer loops.Dec()
	p.watchServiceElection(svcCtx, service, wg)
	return nil
}

// onStoppedLeadingMember acquires the Service lock and holds it through member
// validation and datapath cleanup. Callers must not already hold that lock.
func (p *Processor) onStoppedLeadingMember(member *serviceElectionMember, svcLease *lease.Lease) error {
	unlockService := p.lockService(member.service.UID)
	defer unlockService()

	currentSvcCtx, err := p.getServiceContext(member.service.UID)
	if err != nil {
		return err
	}
	if currentSvcCtx != member.serviceContext {
		log.Debug("skipping cleanup from superseded service context", "service", member.service.Name, "uid", member.service.UID)
		return nil
	}

	currentMember := member.election.currentMember(member.service.UID)
	if currentMember != member {
		log.Debug("skipping cleanup from superseded readiness generation", "service", member.service.Name, "uid", member.service.UID)
		return nil
	}

	log.Debug("deleting service due to lost leadership", "uid", member.service.UID)
	err = p.deleteCurrentServiceByUID(context.WithoutCancel(svcLease.Ctx), member.service.UID)
	if err != nil {
		log.Error("service deletion", "err", err)
		return err
	}
	return nil
}
